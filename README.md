# ship

Promotion as merge requests. ship borrows Kargo's model (immutable **Freight**,
ordered **Stages**, auditable **Promotions**) but uses the forge (GitLab today) as
the only workflow engine. Every promotion is a merge request against a central
**ledger** repo, so the forge's compliance controls govern every step: protected
branches, approval rules, pipeline policies and audit events.

- **Design:** [docs/design.md](docs/design.md)
- **Organization setup (compliance framework, policies):** [docs/setup.md](docs/setup.md)
- **Forge and secret adapters:** [docs/adapters.md](docs/adapters.md)
- **Approver service:** [docs/approver-service.md](docs/approver-service.md)
- **Comparison with Kargo and others:** [docs/comparison.md](docs/comparison.md)
- **Kosli as evidence backend (proposed):** [docs/kosli.md](docs/kosli.md)

## Architecture

```
                    person / agent / Promotion CR
                                 │  ship promote / ship controller
                                 ▼
 ┌──────────────── ledger repo (forge) ───────────────────────────────┐
 │ ship.yaml (apps, stages, rules)    ledger/<app>/<stage>.yaml (pins)│
 │                                                                    │
 │  ledger MR ──► MR pipeline: ship validate (V1–V7), ship plan       │
 │            ──► V8 check: independent approval (controller)         │
 │            ──► human approval (forge approval rules)               │
 │  merge     ──► main pipeline: ship deploy (re-checks V8)           │
 └───────────────────────────────┬────────────────────────────────────┘
                                 │ drivers
         ┌───────────────────────┼─────────────────────────┐
         ▼                       ▼                         ▼
  crossplane driver        octopus driver            exec / plugin
  env-repo MR with the     release + deployment      ship-driver-<name>
  mapped claim             via Octopus API
         │
         ▼
  env repo MR ──► ship approver (webhook) verifies provenance ──► approves
              ──► merge ──► Crossplane / GitOps applies it

 OCI registry: Freight = image digest; evidence = OCI referrers (tests, deployed/<stage>)
```

The ledger is the source of truth: what *should* run where, with git history as
the audit trail. ship holds no state of its own.

## Main parts

| Part | What it does | Where |
|---|---|---|
| **Freight** | An immutable OCI digest (any artifact type). Evidence is attached as OCI referrers. | `internal/freight` |
| **Ledger** | One pin file per app and stage, changed only through MRs. | `internal/ledger` |
| **Policy** | `ship.yaml`: apps, stages, required evidence, upstream soak. | `internal/config` |
| **Rules V1–V8** | Fail-closed checks: path and schema, digest-only pins, app repo, artifact type, ordered progression with deployed-upstream evidence, required evidence, pin-only MRs, and independent approval (V8). | `internal/policy`, `internal/approval` |
| **CLI** | `ship promote / validate / plan / deploy / status / history / evidence / attest / verify-env-mr`. | `cmd/ship` |
| **Drivers** | Turn a pin change into a deployment: `crossplane`, `octopus`, `noop`, `exec`, or any `ship-driver-<name>` on PATH speaking JSON. | `internal/driver` |
| **Controller** (optional) | A Kubernetes `Promotion` CR becomes a ledger MR. Also reports the pre-merge V8 status check. | `internal/controller` |
| **Approver service** | Verifies and approves ship's env-repo MRs. Its token lives only in the service, never in CI. | `internal/approver`, `internal/envmr` |
| **Forge adapter** | ship's neutral view of the forge (repos, change requests, reviews, checks, CI context, webhooks). GitLab is the first adapter. | `internal/forge`, `internal/gitlab` |
| **Secret adapter** | Secrets are references, not values (`env://`, `file://`), and never print. | `internal/secret` |
| **Fakes** | Octopus and Kosli fakes, validated against the vendors' official API specs. | `internal/fakes` |

## Commands

```
ship promote   --app A --stage S --freight sha256:… --push   # person/agent proposes (ledger MR; --open-via api for non-GitLab)
ship validate                                                  # MR pipeline, fail-closed (V1–V7)
ship plan                                                      # MR pipeline, driver dry-run
ship deploy                                                    # main pipeline: V8, then hand pins to drivers
ship verify-env-mr --no-approve                                # env-repo MR pipeline: provenance check (report only)
ship status | history | evidence | attest
ship controller                                                # Promotion CRs -> ledger MRs (+ V8 status check)
ship approver                                                  # env-repo MR webhooks -> verify -> approve as ship-approver
```

```yaml
# kubectl create -f - (in a team namespace; requestedBy must be you)
apiVersion: ship.dev/v1alpha1
kind: Promotion
metadata: { name: gw-prod-1-4-2 }
spec: { app: SV-API_GW_MGT, stage: PROD, freight: "sha256:…", requestedBy: alice, reason: release 1.4.2 }
```

Repo layouts are `deployTemplates` in `ship.yaml`, not code.

## Runtime configuration

In CI, ship reads the forge, server URL, repo, MR and base revisions from the CI
system's predefined variables. Outside CI:

| Variable | Meaning |
|---|---|
| `SHIP_FORGE` | Forge adapter (default: detected from CI, else `gitlab`) |
| `SHIP_FORGE_URL` | Forge URL (`SHIP_GITLAB_URL` is still accepted) |
| `SHIP_LEDGER_PROJECT` | Ledger repo path (controller, approver, verify-env-mr) |

**Secrets** have logical names. Each name defaults to the environment variable
ship has always used. Override any of them with `SHIP_SECRET_<NAME>=<ref>`:

| Secret | Default | Used by |
|---|---|---|
| `forge-token` | `env://SHIP_GITLAB_TOKEN` | deploy, controller (ship-bot / ship-promoter), `promote --open-via api` |
| `forge-approver-token` | `env://SHIP_APPROVER_TOKEN` | approver service |
| `forge-read-token` | `env://SHIP_READ_TOKEN` | `verify-env-mr --no-approve` |
| `webhook-secret` | `env://SHIP_WEBHOOK_SECRET` | approver service |
| `octopus-api-key` | `env://OCTOPUS_API_KEY` | octopus driver |
| `registry-auth` | `env://SHIP_REGISTRY_AUTH` | registry access (Docker config JSON; optional, else Docker's keychain) |

For example, `SHIP_SECRET_WEBHOOK_SECRET=file:///var/run/secrets/ship/webhook`
reads the secret from a mounted Kubernetes Secret. File secrets are re-read on
every use, so rotation needs no restart.

A missing secret is an error; ship never falls back to an empty value.

## Develop

```
make test                 # unit tests (in-memory OCI registry, scratch git repos, API fakes)
make e2e-up               # GitLab CE + runner + registry + k3s (docker compose, ~2 min first boot)
make e2e-bootstrap        # users, tokens, groups, ledger + env repos, runner, CRD + controller, approver + fakes
make e2e-run              # the scenario in e2e/scenario.sh (~6 min, 47 checks)
make forgetest            # shared forge contract suite against the sandbox GitLab
make e2e-down             # remove everything, including volumes
```

### Fakes of external services

`internal/fakes` has stateful fakes of **Octopus Deploy** and **Kosli**. Every
request and response goes through `internal/fakes/contract`, which validates it
against the vendor's official API spec (`internal/fakes/specs`, see SOURCES.md):

- a wrong request from ship gets a 400;
- a fake that drifts from the real API is recorded as a violation;
- tests fail on any violation.

How to use them:

- **Unit tests:** `fakeoctopus.New(key).Server()` and
  `fakekosli.New(org, tokens).Server()` return an `http.Handler`. Call
  `AssertClean(t)` at the end.
- **Sandbox:** the `fakes` service serves Octopus on `octopus:8081` and Kosli on
  `kosli:8082`, seeded from `e2e/fixtures/fakes/octopus.yaml`. Violations are
  listed at `GET /_contract/violations`. The `payments` app deploys through the
  Octopus fake, whose Test environment is configured to fail.

### Forge contract suite

`internal/forge/forgetest` is one behavioural suite that every forge adapter must
pass: branches, commits, files, change requests, diffs, commits, comments, and
close/delete. `make forgetest` runs it against the sandbox GitLab. A future
GitHub adapter runs the same suite.

The sandbox approximates Ultimate-only controls (compliance frameworks, pipeline
execution and approval policies) with committed CI files and project settings.
