# Design: Kosli as ship's evidence and runtime backend

Status: **proposed, for review.** Not implemented.
Context: the bank already runs Kosli. This changes several proposals in
[comparison.md](comparison.md) (P4, P5, P10, P11 and the gates in §6).

## 1. The split

Kosli and ship overlap less than it first seems:

| Question | Who answers | Why |
|---|---|---|
| What *should* run where? | **ship ledger** (git) | intent, reviewed as MRs |
| Who may change that, and did someone independent approve? | **GitLab** (compliance framework) + V8 | GitLab is the workflow engine; Kosli has no approval primitive |
| Does this artifact have the evidence a stage needs? | **Kosli** (environment policies, attestations) | compliance team already owns requirements there |
| What is *actually* running, and since when? | **Kosli** (environment snapshots) | observed, not self-reported |
| How does it get deployed? | **ship drivers** (Crossplane, Octopus) | unchanged |
| The audit trail an auditor reads | **both, linked**: ledger MR ↔ Kosli trail | ship reports the promotion into Kosli |

```
 build ──► registry (digest) ──► Kosli: artifact + attestations (tests, scans, SBOM…)
                                        │
 MR on ledger ── ship validate ─────────┤  "assert artifact against the DEV/TEST/PROD policy"
               (V1–V4, V7, V8 in ship)  │  "is it running in the upstream env, and for how long"
                                        ▼
 merge ── ship deploy ── re-assert (P1) ── driver ── env
                │                                     │
                └── attest promotion + deploy ──►  Kosli ◄── snapshots (k8s, ECS, servers…)
                                                    │
                    ship status / ship freight ◄────┘  pinned vs running (drift)
```

## 2. Identity: digest = fingerprint

Kosli identifies artifacts by a SHA-256 fingerprint. For OCI images,
`kosli fingerprint --artifact-type oci` takes the digest from the registry, and
snapshots show `name …@sha256:<x>` with fingerprint `<x>`. So ship maps a pin
`reg/repo@sha256:<x>` to Kosli fingerprint `<x>` with no lookup.

**Caveat to verify on the bank's setup (open question 1):** multi-arch images. The
ledger may pin an *index* digest while a Kubernetes snapshot reports the digest the
node pulled. Kosli's docs warn that a multi-arch tag can resolve to a single
architecture. If the two differ, ship must also accept the index's child manifest
digests as "the same Freight". This is cheap: ship already reads descriptors.

## 3. What changes in ship

### 3.1 Evidence backend becomes pluggable (V6)

`ship.yaml`:

```yaml
evidence:
  backend: kosli                 # or "oci" (current: OCI referrers, used in the sandbox)
  kosli:
    org: julius-baer
    url: https://app.kosli.com
stages:
  - name: PROD
    upstream: TEST
    kosli:
      environment: prod-k8s      # snapshotted Kosli environment(s) for this stage
      policies: [prod-release]   # or omit to use every policy attached to the environment
```

- V6 with `backend: kosli` calls Kosli's assert API for the fingerprint, with the
  stage's policies (`GET /api/v2/asserts/{org}/fingerprint/{sha}?policy_name=…`;
  same as `kosli assert artifact --fingerprint … --policy …`).
- Non-compliant, or Kosli unreachable → violation (fail closed). The Kosli policy
  evaluation (which rule failed) goes into the MR report.
- **Use `--policy`, not `--environment`**, for promotion gates. Kosli documents it
  as the option "useful for promotion gates between stages". Asserting against the
  target environment's attached policies also works, but then whatever is attached
  to the runtime environment silently becomes a promotion rule.
- Requirements now live in Kosli policies, owned by the compliance team, versioned
  and immutable there. `requiredEvidence` in ship.yaml stays for the `oci` backend.

### 3.2 "Deployed upstream" and soak come from observation (V5, P5)

Today V5 trusts `deployed/<stage>` evidence that `ship deploy` writes itself. With
Kosli, ship asks what was **observed running**:

- V5: the digest is pinned upstream in the ledger (unchanged) **and** Kosli has
  seen it running in the upstream stage's environment.
- Soak (P5, continuous occupancy): derived from the environment's history
  (`kosli log environment` events / snapshots with timestamps). Only time
  actually running counts, and a replacement ends the interval. This is exactly
  Kargo's "continuously occupied" semantics, from real data.
- Fallback: workloads Kosli can't observe as OCI digests (see §5) keep using
  ship's own `deployed` attestation, now written to Kosli instead of the registry.

Per stage: `kosli.observe: true | false`, default true when an environment is set.

### 3.3 Drift and status (P10, P9)

- `ship status` gains a column per stage: **running** (from the latest Kosli
  snapshot) next to **pinned** (from the ledger). Mismatch = drift: a failed or
  refused deploy, a manual change, or a deploy still in progress.
- `ship freight <version>` shows, per stage: pinned, running since, policy
  compliance (with the failing Kosli rules), and soak progress.

### 3.4 ship reports the promotion into Kosli

Kosli can't see GitLab approvals (its GitLab PR attestation only checks that an MR
*exists*; approval state doesn't affect compliance). ship closes that gap:

- **Trail per promotion** in a flow `ship-promotions` (trail name = ledger MR
  reference, e.g. `ledger-mr-123`).
- **Custom attestation type** `ship-promotion` on the artifact (fingerprint)
  carrying: app, stage, ledger MR URL, requester(s), approvers, V8 result, ledger
  merge commit, policy revision. Created with `kosli create attestation-type`
  with a jq rule such as "independent approvals ≥ required", so Kosli itself can
  judge compliance.
- **Decision attestations** (`kosli attest decision --control …`) for ship's own
  checks (V1–V8, gates), so an auditor sees each control's verdict in Kosli.
- **Deploy result** attested after the driver finishes (and the snapshot then
  confirms it independently).

Kosli policies can then require `custom:ship-promotion` for production: an
artifact that reaches PROD **without** going through a ship promotion shows up
as non-compliant in Kosli's runtime view. That's a control ship can't provide on
its own: it detects bypasses (someone deploying by hand).

### 3.5 Gates (comparison.md §6)

The gate contract stays. Kosli becomes the first built-in gate (`kosli-assert`).
ServiceNow stays a separate gate, whose verdict is also written to Kosli as a
decision attestation so all evidence ends up in one place. If the bank already
attests ServiceNow tickets into Kosli, a Kosli policy can require them, and no
separate ServiceNow gate is needed.

## 4. Trust (replaces most of P11)

- Kosli service accounts (roles Admin / Member / Snapshotter / Reader), with API
  keys stored hashed. Every attestation records `reported_by`.
- ship can require that the attestations it relies on were reported by the
  expected service account. For example, the ship promotion and deployed
  attestations are only accepted from `ship-deploy`; test results only from the
  CI service account.
  Kosli's assert API judges compliance, so this check needs ship to read the
  attestation (`kosli get attestation … -o json`) for the few it cares about.
- Snapshots come from Snapshotter accounts running in the clusters, which is
  independent of whoever deploys.
- Cryptographic signing is only verified for in-toto / Sigstore SBOM attestations.
  Generic and custom attestations are not signed. So this is identity-based trust
  (who held which API key), not signatures. For a bank that is likely acceptable
  if API keys are scoped per pipeline and rotated, and the remaining signing work
  (old P11) becomes optional.

## 5. What Kosli can't see

- **Octopus to VMs / servers:** `kosli snapshot server` fingerprints files and
  directories, not OCI digests. Those stages use ship's `deployed` attestation
  (§3.2 fallback) unless the deployed package's fingerprint can be mapped to the
  Freight digest (open question 3).
- **Crossplane claims for non-container resources** (databases, buckets): there is
  no running digest to observe. Same fallback.
- **Kubernetes workloads** (Crossplane rendering Deployments, ECS, Lambda, Cloud
  Run…): fully observable.

## 6. Sandbox and tests

Kosli is SaaS, so it can't run in docker compose. **Built:** a stateful Kosli fake
(`internal/fakes/kosli`) whose every request and response is validated against
Kosli's official OpenAPI 3.1 document. It covers flows, trails, artifacts,
generic and custom attestations (jq evaluators), environments, K8S snapshot
reports, snapshot expressions, environment events, policies (subset: no
`if`/`exceptions`) and assert. It runs in unit tests and as `kosli:8082` in the
e2e sandbox. Not yet used by ship: the integration itself (§7) is not built.

Optionally, a second e2e mode against a real Kosli org with a dedicated test
environment, run only when credentials are provided. Sending data to an external
service is your call, so it's off by default.

## 7. Plan

1. `internal/kosli`: small REST client (assert, get attestation, attest custom /
   decision, environment snapshots/log), plus the fake server.
2. Evidence backend interface in `policy` (`oci` = today's code, `kosli` = new);
   V6 via assert.
3. V5 observation + continuous-occupancy soak from Kosli, with the `deployed`
   attestation fallback.
4. `ship deploy` reports the promotion trail and attestations; re-asserts before
   the driver runs (with P1).
5. `ship status` running column; `ship freight` (P9) using the same data.
6. Docs: setup.md section on Kosli service accounts, flows, attestation type,
   and the `custom:ship-promotion` policy rule.

## 8. Open questions

1. **Multi-arch:** do your clusters' snapshots report the index digest or the
   per-platform digest? (Can be checked with `kosli get snapshot <env>` against
   a known multi-arch image.)
2. **Kosli layout:** do flows already exist per app (build pipelines reporting
   artifacts)? ship should attach to those trails rather than invent new ones.
3. **Octopus targets:** what do they run, containers or packages on VMs? If
   packages, can their Kosli fingerprint be linked to the OCI digest?
4. **Policies:** are there already per-environment Kosli policies (e.g. `prod`)
   that should become the promotion gates, or should promotion-specific policies
   be created?
5. **ServiceNow:** is the change ticket already attested into Kosli (custom
   attestation)? If yes, the ServiceNow gate collapses into a Kosli policy rule.
6. **Bypass control:** should Kosli policies on production require
   `custom:ship-promotion`, making a manual deploy show as non-compliant?

## Sources

Kosli documentation: [how Kosli works](https://docs.kosli.com/understand_kosli/how_kosli_works),
[environment policy reference](https://docs.kosli.com/policy-reference/environment_policy),
[enforce policies](https://docs.kosli.com/getting_started/enforce_policies),
[kosli assert artifact](https://docs.kosli.com/client_reference/kosli_assert_artifact),
[lab 04 release controls](https://docs.kosli.com/labs/lab-04-release-controls),
[lab 05 runtime controls](https://docs.kosli.com/labs/lab-05-runtime-controls),
[service accounts](https://docs.kosli.com/administration/authentication/service_accounts).
