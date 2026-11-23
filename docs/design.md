# ship: promotion as merge requests

Status: **MVP implemented** (CLI, crossplane + octopus + noop/exec drivers, e2e sandbox). See README.md. §12 (Promotion CRD + V8) is **implemented**.
Comparison with Kargo, GitOps Promoter, Telefonistka, Cloud Deploy and Octopus, with proposed adoptions: [comparison.md](comparison.md).
Kosli as evidence and runtime backend: [kosli.md](kosli.md).

## 1. Summary

`ship` keeps Kargo's promotion *model* (Freight, Stage, Promotion, history) and drops
Kargo's *automation*. A promotion is a GitLab merge request against a ledger repo,
created by a person or an agent. GitLab is the only workflow engine, so every
write runs inside pipelines that the compliance framework already governs.

`ship` itself is a small CLI with three jobs:

1. **Propose:** construct a promotion MR (`ship promote`).
2. **Validate:** fail-closed policy checks in the MR pipeline (`ship validate`).
3. **Deploy:** on merge, hand the promoted Freight to a pluggable **deployment driver** (`ship deploy`).

`ship` does not care what kind of artifact it promotes. It moves OCI digests and
reads only OCI *descriptors* (digest, mediaType, artifactType, annotations), never
artifact content. Only a driver understands what is inside.

## 2. Goals / non-goals

**Goals**

- Immutable, content-addressed promotion units (Freight = OCI digest).
- A promotion is a reviewable MR. People and agents use the same path and the same gate.
- All execution happens in GitLab pipelines, so the existing compliance controls apply unchanged.
- Package agnostic: container images, Helm charts, Kustomize/YAML bundles, Crossplane
  claim bundles, WASM, and so on all promote the same way.
- Deployment mechanism is an interchangeable driver.
- Evidence (test results, scans, sign-offs) travels with the Freight as OCI referrers.
- History and audit come from git plus GitLab. No new database.

**Non-goals (for now)**

- Auto-promotion. If needed later, it is just an agent or schedule that opens MRs.
- Discovering new artifacts (no Kargo Warehouse). Builds publish Freight explicitly.
- Long-running orchestration such as Crossplane ordering. That stays in the driver or the platform.
- A UI. A Backstage plugin that reads the ledger comes later.

## 3. Concepts

| Concept | Meaning in ship | Backed by |
|---|---|---|
| **App** | A thing that gets promoted (a service, a platform component). Declares its OCI repository. | `ship.yaml` |
| **Freight** | One immutable OCI artifact: `registry/repo@sha256:…`. Its content is opaque to ship. | OCI registry |
| **Stage** | An environment plus its admission rules (upstream stage, required evidence) and its deployment driver. | `ship.yaml` |
| **Pin** | "App X in Stage Y runs Freight Z." | `ledger/<app>/<stage>.yaml` |
| **Promotion** | A request to change a pin. | GitLab MR |
| **Evidence** | Facts about a Freight (tests passed in stage *test*, scan clean, …). | OCI referrers with `subject` = Freight digest |
| **History** | Who promoted what, when, and who approved it. | git log of the pin plus MR approvals |
| **Driver** | Makes the runtime match a pin. | Plugin (see §7) |

### 3.1 Artifact agnosticism

- `ship` resolves a Freight reference to a descriptor (one HEAD/GET of the manifest)
  and never pulls layers.
- An app may *optionally* restrict `artifactTypes` (for example, only
  `application/vnd.cncf.helm.config.v1+json`). The restriction is a policy check, not
  parsing.
- Anything that interprets content (rendering a chart, patching claims, reading an
  embedded mapping file) belongs to the driver.
- An image index or multi-arch manifest is fine. The index digest is the Freight.

## 4. Repositories and layout

**Decision (proposed): a central ledger repo.** It makes the upstream-stage check a
local git query, gives one place to see the overview, and gives one place to attach
the compliance framework. The alternative (pins inside each env repo) is discussed in §11.

```
platform/ledger                      ← promotions happen here (MRs by people/agents)
├── ship.yaml                        # apps, stages, rules, drivers   (CODEOWNERS: policy owners)
├── mappings/payments.yaml           # which claim fields get the Freight ref/digest
├── ledger/
│   ├── payments/{dev,test,prod}.yaml        # crossplane-driven app
│   └── ledger-ui/{dev,test,prod}.yaml       # octopus-driven app
├── CODEOWNERS
└── .gitlab-ci.yml                   # MR: ship validate, ship plan │ main: ship deploy
    branches: main (protected) + promote/<app>/<stage>/<digest12>

envs/{dev,test,prod}                 ← what Flux syncs; claims change only via MRs
├── apps/payments/claim.yaml         # Crossplane XRD claim; ship owns only the mapped fields
└── .gitlab-ci.yml                   # MR: compliance jobs + ship verify-env-mr (→ ship-approver)
    branches: main (protected) + ship/<app>/<stage>/<digest12>   (bot MRs from the driver)
```

Octopus-driven apps have nothing in env repos: their pin goes straight to an Octopus release/deployment.

### 4.1 `ship.yaml`

```yaml
apiVersion: ship/v1alpha1

apps:
  payments:                              # deployed as a Crossplane claim, synced by Flux
    repository: registry.example.com/deploy/payments
    artifactTypes:                       # optional allow-list
      - application/vnd.example.deploy-bundle.v1
    deploy:
      driver: crossplane
      config:                            # app-wide, opaque to ship
        mapping: mappings/payments.yaml
      stages:                            # per-stage config, merged over app-wide
        dev:  { envRepo: envs/dev,  path: apps/payments/claim.yaml }
        test: { envRepo: envs/test, path: apps/payments/claim.yaml }
        prod: { envRepo: envs/prod, path: apps/payments/claim.yaml }

  ledger-ui:                             # deployed by Octopus
    repository: registry.example.com/apps/ledger-ui
    deploy:
      driver: octopus
      config: { space: Spaces-1, project: ledger-ui, packageStep: deploy-ui, feed: Feeds-oci }
      stages:
        dev:  { environment: Development }
        test: { environment: Test }
        prod: { environment: Production }

stages:                                  # the promotion graph, shared by all apps
  - name: dev

  - name: test
    upstream: dev                        # Freight must have been pinned in dev

  - name: prod
    upstream: test
    requiredEvidence:
      - artifactType: application/vnd.ship.evidence.v1+json
        annotations: { ship.dev/kind: integration-test, ship.dev/stage: test, ship.dev/result: pass }

registries:                              # optional per-registry client settings
  registry.example.com: { insecure: false }
```

- Stages form a DAG through `upstream`. The MVP allows one upstream per stage; fan-in comes later.
- **Drivers belong to the app**, not the stage, because a given app is deployed by one
  system (Octopus *or* Crossplane) in every environment. Each stage only supplies its
  own target (Octopus environment, env repo). Promotion policy (stages, upstream, evidence)
  is the same whichever driver an app uses.

### 4.2 Pin file

```yaml
# ledger/payments/prod.yaml
freight: registry.example.com/deploy/payments@sha256:4f1c…
```

That one field is deliberate. Who, why and when live in the commit, the MR and the
approvals. Decoding is strict, so unknown fields are rejected.

## 5. Flows

```
 build pipeline                 person / agent                GitLab (compliance framework applies)
 ──────────────                 ──────────────                ─────────────────────────────────────
 push artifact ─► registry
 attach evidence (referrer) ─►
                                ship promote ──► branch + commit + MR ──► MR pipeline: ship validate  (fail closed)
                                                                         ship plan  (driver dry-run, MR comment)
                                                                         approvals / protected branch
                                                                  merge ─► main pipeline: ship deploy  (driver apply)
                                                                           ship record (evidence: deployed to <stage>)
```

### 5.1 Propose: `ship promote`

```
ship promote --app payments --stage prod --freight sha256:4f1c… \
             --reason "release 2026.11.3" [--push]
```

1. Resolve the short digest against the app repository and get the descriptor.
2. Run the same checks as `validate` locally, against `HEAD`, to fail fast. This is advisory.
3. Create branch `promote/payments/prod/4f1c…`, write the pin, and commit with trailers
   (`Freight:`, `Reason:`, `Requested-by:`). `Requested-by:` records the agent identity when an agent proposes.
4. With `--push`, push and open the MR. On GitLab the default is push options
   (`merge_request.create`, `.target`, `.title`, `.description`), so no API token is
   needed beyond git push. `--open-via api` (the default on other forges) does a plain
   push and opens the change request through the forge API with `forge-token`.

Agents can call the CLI. An MCP wrapper (`ship mcp`) that exposes `status`,
`evidence`, `promote` (read state and propose only) comes later. Agents never get
merge rights.

### 5.2 Validate: `ship validate --base $CI_MERGE_REQUEST_DIFF_BASE_SHA`

All policy comes from the **base** revision, never from the MR head. Otherwise an MR
could weaken `ship.yaml` and promote in the same change.

For each changed file under `ledger/`:

| # | Rule | Why |
|---|---|---|
| V1 | Path is `ledger/<known app>/<known stage>.yaml`. Deletes and renames are rejected. | No unpinning via promotion |
| V2 | Strict schema, `freight` is a digest reference (no tags) | Immutability |
| V3 | Freight repository equals the app's repository | No cross-app smuggling |
| V4 | Descriptor resolves in the registry, and `artifactType` is in the allow-list if one is set | Artifact exists and is the right kind |
| V5 | If the stage has an `upstream`: the digest was pinned in the upstream stage at some point in the **base** history, **and** it carries `deployed` evidence for the upstream stage (`ship.dev/kind=deployed`, `ship.dev/stage=<upstream>`, `ship.dev/result=pass`, recorded by `ship deploy` after a successful Apply/Wait) | Ordered progression of what actually ran. Same-MR dev+prod pins don't count; a pin whose deploy failed or was refused (e.g. V8) doesn't count either |
| V6 | Every `requiredEvidence` entry is matched by a referrer (artifactType plus annotation match) **that passes the evidence verifier** (§8) | Gates |
| V7 | A promotion MR changes only `ledger/**`. Changes to `ship.yaml` and CI go through a separate MR with policy-owner approval | Separate policy from promotion |

The output is a human-readable report plus JSON (`--format json`). The command exits
non-zero on any violation and on any error, including a registry that can't be reached.

### 5.3 Plan: `ship plan --base …`

This optionally calls `Driver.Plan` for each changed pin and posts the result as an
MR note, so reviewers approve a concrete diff and not just a digest.

### 5.4 Deploy: `ship deploy --base $CI_COMMIT_BEFORE_SHA`

This runs on the default branch after merge. It computes the changed pins, then for
each one calls the stage's driver `Apply` and `Wait` (§7). After success it can
`ship record`, which pushes an evidence referrer (`ship.dev/kind: deployed`,
`ship.dev/stage: prod`, the pipeline URL) so downstream stages can require it.

## 6. History and status

- `ship status` shows an app × stage matrix of short digests from the ledger.
- `ship history --app payments --stage prod` reads git log of the pin file plus commit trailers.
- `ship evidence <freight>` lists referrers.

## 7. Deployment drivers

### 7.1 Contract

```go
type Driver interface {
    // Plan describes what Apply would change. No side effects.
    Plan(ctx context.Context, req Request) (Plan, error)
    // Apply makes the target reflect req.Freight. Must be idempotent.
    Apply(ctx context.Context, req Request) (Result, error)
    // Wait blocks until the target is healthy at req.Freight, or ctx expires.
    // Drivers that cannot observe health return ErrNotSupported.
    Wait(ctx context.Context, req Request) (Health, error)
}

type Request struct {
    App, Stage string
    Freight    Descriptor          // ref, digest, mediaType, artifactType, annotations
    Previous   *Descriptor         // currently pinned, if any
    Config     map[string]any      // stage/app driver config from ship.yaml, opaque to ship
    Promotion  PromotionMeta       // MR iid, commit, requested-by, pipeline URL
}
```

### 7.2 Packaging: exec drivers (proposed)

Drivers are **external executables** that speak JSON over stdin/stdout, like
credential helpers or Kargo custom steps:

```
ship-driver-<name> plan|apply|wait  < request.json  > response.json
```

- They can be written in any language and shipped as a CI image or a binary on the runner.
- A driver crash or non-zero exit counts as failure. Responses are schema-validated.
- `ship` ships with two built-ins for testing: `noop` (records only) and `exec`
  (runs a configured command with env vars `SHIP_FREIGHT`, `SHIP_APP`, `SHIP_STAGE`, …).
- The resolution order is built-in name, then `ship-driver-<name>` on `PATH`.

### 7.3 Drivers

| Driver | Target system | What Apply does |
|---|---|---|
| `crossplane` | Crossplane XRD claims in env repos, synced by Flux | Writes the claim to the env repo (§7.4) |
| `octopus` | Octopus Deploy | Ensures a release exists and deploys it to an environment (§7.5) |
| `noop` | none | Logs only. Used in tests and the sandbox |
| `exec` | anything | Runs a configured command. A bridge for one-off tooling |

The two real drivers behave very differently: one writes to git and lets Flux do the
rest, the other calls an orchestration API. The contract has to cover both. That is why
`Wait` is separate and optional, and why `Apply` must be idempotent: a pipeline retry
must not create a second Octopus release or a second env-repo commit.

### 7.4 `crossplane` driver

Today rodi-cli patches the Crossplane claims by hand. This driver replaces that step.

- **Input:** the Freight descriptor plus a **mapping file** that says where Freight values
  go in the claim, for example:

  ```yaml
  # mappings/payments.yaml
  apiVersion: example.org/v1alpha1
  kind: AppDeployment                 # the claim kind from the XRD
  set:
    - path: spec.artifact.ref         # JSONPath-style path into the claim
      value: "{{ .Freight.Ref }}"     # registry/repo@sha256:…
    - path: metadata.annotations["ship.dev/freight"]
      value: "{{ .Freight.Digest }}"
  ```

  The mapping only reads descriptor fields (ref, digest, annotations), so ship still never
  opens the artifact. If the claim values must come from *inside* the artifact (for example
  a values file in the bundle), the driver pulls the artifact. That stays a driver concern.
- **Plan:** clone the env repo, render the patched claim, and return a unified diff. `ship plan` posts the diff on the promotion MR.
- **Apply: open an MR in the env repo** (decided). Then the env repo's own pipeline and compliance
  jobs run on the change, and every change to the env repo arrives as an MR, the same as today.
  1. Branch `ship/<app>/<stage>/<digest12>` from the env repo's default branch, commit the patched claim.
  2. Open the MR with a description that links back to the ledger MR, the ledger commit and the
     deploy pipeline, and carries `Freight:` / `Ledger-MR:` trailers. That links the two audit trails both ways.
  3. Set **auto-merge** (merge when the pipeline succeeds). The ledger MR was the approval, so the
     env MR should need no second human approval (see below).
- **Idempotency:**
  - If the claim already equals the rendered claim, nothing happens.
  - If an open MR already exists for the same branch, reuse it (pipeline retries).
  - If an open ship MR exists for the same app and stage with an *older* digest, close it with a
    "superseded by …" note, so only the latest intent can merge.
- **Wait:** two phases.
  1. *Merged* (always): poll the env MR until it is merged, which means success. If it is closed,
     its pipeline fails, or it can't be merged because of conflicts, that is a failure. A timeout
     leaves the MR open and fails the job with a link to the MR.
  2. *Ready* (optional): with a read-only kubeconfig for the target cluster, wait until the claim's
     `metadata.generation` is reflected in `status` and its `Synced` and `Ready` conditions are `True`.
     Without cluster access, phase 2 is skipped. Flux owns the cluster, so ship only reads from it and never writes.
- **Permissions in the env repo:** a project/group access token (bot user) that can create branches and MRs,
  and that is allowed to merge into the protected branch, because auto-merge runs as the user who set it.
  The token is a protected, masked CI variable available only on the ledger repo's default branch.
- **Approval rules in the env repo: ship MRs are exempt** (decided). The ledger MR stays the only
  human gate. A blanket "the bot's MRs need no approval" rule would mean anyone holding the bot token
  could change prod unreviewed. So the exemption is **conditional on provenance**:
  - **`ship verify-env-mr`** is a required job in the env repo's MR pipeline that runs only on ship MRs.
    It fails unless all of these hold:
    - The MR author is the ship bot, and the source branch matches `ship/<app>/<stage>/<digest12>`.
    - The `Ledger-MR:` / ledger commit it references is **merged** on the ledger default branch, and
      the pin at that commit equals the MR's `Freight:`.
    - The diff touches **only** the claim path configured for that app and stage, and **only** the fields
      the mapping file sets. Re-rendering the claim from the ledger pin reproduces the MR exactly.
    - The ledger MR had the required approvals (read through the API).
  - **How the approval is skipped** depends on what the instance supports, so it must be checked on our
    GitLab version and tier:
    - (a) *Native bypass:* if merge request approval policies on our instance can exempt a service
      account or access token, scope the exemption to the ship bot. The required `verify-env-mr` job
      plus "pipelines must succeed" keep it conditional.
    - (b) *Approver bot (works on any tier):* a separate `ship-approver` service account is an eligible
      approver on env repos. `verify-env-mr` approves the MR as that account only after every check passes.
      The author and the approver are then different identities, and the approval appears in GitLab's own audit log.
  - **Decided: (b) `ship-approver`.** It needs no special policy support, and an auditor sees
    an explicit, explained approval rather than a missing one.
  - Human-authored MRs in env repos keep their normal approval rules.
- **Manual edits:** if someone edits the claim directly in the env repo, the next promotion's MR
  may conflict or overwrite the edit. The plan diff shows this. Ownership of the mapped fields belongs
  to ship. Other fields in the claim remain editable by hand.
- **Ordering:** if one promotion touches several claims that have to come up in order, that is not
  solved here. The composition handles it, or the claims go in separate promotions.

### 7.5 `octopus` driver

Octopus brings its own promotion model: releases, lifecycles, environments, manual
interventions and an audit log. Running it under ship means deciding who owns what.

**Proposed split.**

| Concern | Owner |
|---|---|
| What should run where (the pin) | ship ledger |
| Approval to promote | GitLab MR (single gate) |
| How to deploy (process, steps, variables, targets) | Octopus project |
| Deployment execution and its logs | Octopus server and tentacles |

- **Release identity.** One Octopus release per Freight, with a deterministic version, for example
  `org.opencontainers.image.version` from the descriptor annotations, falling back to
  `0.0.0-sha256.<first 12 chars of digest>`. Apply first looks the release up by version and
  creates it only if it is missing. Promoting the same Freight to the next environment then
  *redeploys the same release*, which matches how Octopus already works.
- **Package pinning (immutability gap).** Octopus feeds select packages by *version/tag*, not
  by digest. The driver must close that gap:
  1. Create the release with the package version that corresponds to the Freight's tag, then
  2. Check that this tag still resolves to the pinned digest at release-creation time, and fail if not.
  3. Store the digest on the release (release notes or a project variable `Ship.Freight.Digest`), so
     deployment steps can pull `@digest` instead of the tag where the step type allows it.
  An immutable-tags policy in the registry closes the race window completely.
- **Lifecycle.** The Octopus lifecycle must not become a second, unaudited gate, and it must not
  reject promotions that ship has already approved. Either:
  - (a) a single permissive lifecycle where every phase is optional, with ship as the only progression
    rule (proposed), or
  - (b) Octopus phases that mirror the ship stages exactly, kept as defence in depth. The cost is that
    stages are then defined in two places.
  Manual interventions in the Octopus process would block `Wait`. They should be removed for
  ship-managed projects, because the MR approval already happened.
- **Apply:** ensure the release exists, then `POST` a deployment for that release to the stage's
  environment. The deployment carries the MR and pipeline URLs in its comments, which links the Octopus
  audit log back to the promotion.
- **Wait:** poll the server task until `Success` or `Failed`/`Canceled`, or until the job times out.
  A failure fails the GitLab deploy job, so the pin then sits ahead of the runtime (see §11.7).
- **Plan:** report the release (existing or to be created), the package version, the target
  environment and the currently deployed release.
- **Credentials:** an Octopus API key from a service account, scoped to the projects and environments
  ship manages, stored as a protected and masked GitLab CI variable available only on the default branch.
- **Compliance note:** execution happens on Octopus tentacles, outside GitLab. GitLab still controls
  the *decision* and the *trigger*. Confirm with compliance that Octopus already counts as an approved
  executor.

## 8. Evidence

- Evidence is any OCI artifact whose `subject` is the Freight digest. ship matches it
  on `artifactType` plus annotations.
- Producers include CI jobs (`ship attest --freight … --kind integration-test --stage test --result pass --file report.json`)
  and `ship record` after a deploy.
- **Verifier interface.** Presence alone can be forged by anyone with push access to
  the registry, so each requirement names a verifier:
  - `none`: presence only. MVP and sandbox only.
  - `cosign`: the referrer must be signed by an allowed identity (for example, the GitLab
    OIDC issuer plus a specific project and ref). This is the production default.
- Registry compatibility: ship uses the OCI 1.1 referrers API and falls back to the
  tag schema (`sha256-<digest>`) for older registries.

## 9. Security and compliance model

- **Single gate.** GitLab MR approvals, protected branches and protected environments.
  ship adds no approval system of its own.
- **Single executor.** Only GitLab pipelines write: the ledger merge and the driver
  Apply. CI and pipeline-execution policies of the compliance framework attach to the
  ledger repo (and to env repos if drivers write there).
- **Agents** get read access to the registry and ledger plus "create MR" rights. They
  never merge, never push to protected branches, and never deploy.
- **Policy changes** (`ship.yaml`, `.gitlab-ci.yml`) are CODEOWNERS-protected and are
  validated against base (V7).
- **Fail closed.** Unknown fields, unreachable registries and verifier errors all
  produce a rejection.
- The ledger needs full history (`GIT_DEPTH: 0`) for V5.

## 10. MVP scope

**In**

- CLI: `promote`, `validate`, `status`, `deploy`, `attest` (presence-only evidence).
- Rules V1–V7 except signature verification (verifier `none`).
- Drivers: `noop` and `exec`, with the exec-driver protocol defined (§7.2).
- `crossplane` driver in MR mode, tested end to end against an env repo in the sandbox GitLab:
  open, auto-merge, reuse on retry and supersede. Includes `ship verify-env-mr` with the
  `ship-approver` account (option b), plus a negative test: a hand-edited bot MR touching another
  field is rejected and stays unapproved. `Wait` covers only phase 1 (merged), because the sandbox has no cluster.
- `octopus` driver tested against a recorded or fake Octopus API. Testing against a real Octopus
  server is a follow-up: Octopus needs a license and SQL Server, so it doesn't belong in the compose sandbox.
- Example ledger repo with `.gitlab-ci.yml`.
- **E2E sandbox** (`e2e/compose.yaml`): GitLab CE, gitlab-runner (docker executor),
  and an OCI registry (`registry:3`). A bootstrap script creates a token, the ledger
  project and the runner. The scenario script runs:
  1. Push Freight A. `promote dev`, then merge, then the `exec` driver runs.
  2. `promote prod` for A is **rejected**, because A was never pinned in test (V5).
  3. `promote test`, then merge. `promote prod` is **rejected** because evidence is missing (V6).
  4. `attest` evidence. `promote prod` passes. Merge, then deploy. The `crossplane` driver opens an MR
     in `envs/prod` that auto-merges after the env repo's pipeline passes. A retry reuses the same MR.
     A newer promotion closes an older env MR that is still open.
  5. A tag ref, an app/repository mismatch, and a `ship.yaml` edit inside a promotion MR are each rejected.

**Out (next)**

- cosign verifier, `plan` MR notes, crossplane `Wait` phase 2 (cluster readiness), real-Octopus integration test,
  MCP server, Backstage plugin,
  fan-in upstreams, rollback UX (rollback = promote an earlier digest, which already
  works but has no dedicated command).

**Sandbox caveat.** GitLab CE has no compliance frameworks, pipeline execution
policies or MR approval rules, because those are Premium/Ultimate features. The sandbox
approximates them with a protected main branch and "pipelines must succeed". The real
instance needs Ultimate, with the framework label on the ledger repo.

## 11. Open questions for review

1. **Central ledger vs per-env pins.** This design proposes central. Per-env keeps the
   pin next to what it deploys, but V5 then needs cross-repo reads and the overview
   becomes an aggregation job. Is there an org rule that environment state must live in the env repo?
2. **Granularity of Freight.** Is it one artifact per app (as proposed), or a multi-app
   bundle (Kargo-style Freight with several artifacts)? A bundle can be modelled as an
   OCI index whose manifests are the parts. That is still one digest, but drivers must then handle indexes.
3. ~~**Crossplane apply mode.**~~ Decided: open an MR in the env repo and auto-merge it (§7.4).
   ~~Env-repo approvals.~~ Decided: ship MRs are exempt, conditional on `ship verify-env-mr`
   provenance checks (§7.4), approved by the `ship-approver` account (option b).
4. ~~**V5 strictness.**~~ Decided: "was ever pinned upstream" **and** `deployed/<upstream>` evidence.
   The e2e run showed why: a V8-refused deploy left the pin in TEST, which under pin-only V5 made the
   Freight PROD-eligible without ever running in TEST. Optional `stages[].upstreamSoak: 24h` adds a
   minimum time since the *first* `deployed/<upstream>` evidence (its `org.opencontainers.image.created`
   annotation, stamped by ship; evidence without a timestamp fails closed).
5. **Evidence verifier identity.** Which signing identity counts: the GitLab OIDC
   `ci_config_ref_uri`, or a bank-managed key?
6. **Language and packaging.** Go is proposed (go-containerregistry, single static binary, and it sits
   close to the Kargo/Flux ecosystem).
7. **Failed deploys.** When Apply or Wait fails after the merge, the ledger says X but the runtime still
   runs the old version. Options: leave the pin and retry the job (proposed, because the pin is *intent*),
   auto-open a revert MR, or record `deploy-failed` evidence and show the drift in `ship status`.
   Since V5 requires `deployed` evidence, an undeployed pin can no longer be promoted further; the
   remaining question is only how the drift is surfaced and resolved.
8. **Octopus lifecycle.** Should it be one permissive lifecycle (a), or phases that mirror the ship stages (b)?
   Are Octopus manual interventions mandated anywhere that would block `Wait`?
9. **Octopus packages.** What feed type do the Octopus projects use today (Docker/OCI, Helm, built-in)?
   That decides how the digest-to-version mapping works, and whether steps can pull by digest.
10. **Crossplane claim source.** Is a claim fully described by the mapping file plus descriptor fields
    (as proposed), or do the deployment values live inside the OCI artifact, so the driver must pull it?
11. **ship-approver credential.** `verify-env-mr` runs in env-repo MR pipelines alongside team jobs,
    so the approver token must not be a plain CI variable there. Recommended: a small approver service
    on env-repo MR webhooks (reusing the same verification code), or Vault + GitLab ID tokens.
    See [setup.md §4](setup.md#4--protecting-the-ship-approver-credential). **Implemented** as the
    approver service: [approver-service.md](approver-service.md).

## 12. Promotion CRD (implemented)

A Kargo-style `Promotion` custom resource. The design principle stays the same: **the CR is a
front-end that creates ledger MRs. It is not a second engine.** Everything that changes
anything still happens in GitLab under the compliance framework. The CR's `.status` is a
mirror of GitLab state, never the source of truth.

### 12.1 Where it runs

```
management cluster (owned by the platform team; NOT a Flux-managed target cluster)
├── CRD  promotions.ship.dev
├── ValidatingAdmissionPolicy  ship-promotion   (CEL, no webhook)
└── Deployment  ship-controller                 (ship controller, same binary)
        │ GitLab API (ship-promoter account)
        ▼
platform/ledger  ── ledger MR ──► validate ──► approval ──► merge ──► deploy ──► drivers
```

### 12.2 Resource

```yaml
apiVersion: ship.dev/v1alpha1
kind: Promotion
metadata:
  name: gw-prod-0cfd9bedcd63
  namespace: team-api-gw              # namespace = team; controls who may promote what
spec:                                 # immutable after create
  app: SV-API_GW_MGT
  stage: PROD
  freight: sha256:0cfd9bedcd63…       # or a full registry/repo@sha256:… reference
  reason: release 2026.11.3
  requestedBy: alice@bank.example     # must equal the creating identity (admission policy)
status:
  phase: AwaitingApproval             # see 12.3
  observedGeneration: 1
  freight: registry/deploy/gw@sha256:0cfd9b…   # resolved reference
  ledgerMR: { iid: 5, url: https://gitlab/…/merge_requests/5, approvedBy: [bob] }
  ledgerCommit: 038bb69b…
  deployPipeline: { id: 11, url: …, status: success }
  targets:                            # what the driver did
    - kind: envMR                     # crossplane
      url: https://gitlab/sda/services/SV-API_GW_MGT/PROD/-/merge_requests/1
      state: merged
      approvedBy: [ship-approver]
    # or: { kind: octopusDeployment, id: Deployments-42, state: Success }
  violations: []                      # validate output if rejected
  conditions:                         # standard k8s conditions
    - { type: Proposed,  status: "True" }
    - { type: Validated, status: "True" }
    - { type: Approved,  status: "True" }
    - { type: Merged,    status: "False" }
    - { type: Deployed,  status: "Unknown" }
```

`kubectl get promotions` printer columns: `APP  STAGE  FREIGHT  PHASE  MR  AGE`.

### 12.3 Lifecycle

```
            create CR
               │
               ▼
          ┌─────────┐ pre-check fails (V1–V6 vs ledger main)
          │ Pending │──────────────────────────────────────────► Rejected
          └────┬────┘
               │ controller opens ledger MR (branch promote/<app>/<stage>/<digest12>,
               │ trailers: Promotion: <ns>/<name>, Promotion-UID, Requested-by)
               ▼
          ┌──────────┐ validate failed                          ► Rejected (violations copied to status)
          │ Proposed │─────────────────────────────────────────────►
          └────┬─────┘
               │ validate + plan passed
               ▼
      ┌──────────────────┐ MR closed by a human                   ► Abandoned
      │ AwaitingApproval │──────────────────────────────────────────►
      └────────┬─────────┘ a newer Promotion for the same app/stage ► Superseded
               │ approved + merged
               ▼
          ┌───────────┐ deploy pipeline / driver / env MR failed ► Failed
          │ Deploying │──────────────────────────────────────────────►
          └─────┬─────┘
                ▼
            Succeeded   (deployed/<stage> evidence recorded)
```

- **Reconciliation:** poll GitLab every 15s for non-terminal Promotions. A GitLab webhook receiver
  can trigger immediate reconciles later. Every step is idempotent: a deterministic branch, and the
  MR is looked up by branch, so a controller restart never opens a second MR.
- **Deletion:** a finalizer closes the ledger MR if it is still open. Once merged, deleting the CR
  only removes the mirror, because git history is the record.
- **Terminal phases** (Succeeded, Failed, Rejected, Abandoned, Superseded) stop polling. A TTL
  controller can garbage-collect old CRs, since the ledger keeps the history.

### 12.4 Identity and the four-eyes rule (new rule V8)

The controller opens MRs as **`ship-promoter`**, so GitLab's "prevent approval by author" protects
the bot, not the human who asked. Without more, a requester could approve their own promotion.

- **Trustworthy `requestedBy`:** a ValidatingAdmissionPolicy enforces
  `object.spec.requestedBy == request.userInfo.username` on create, and `object.spec == oldObject.spec`
  on update (spec is immutable). Agents use Kubernetes service accounts, which yields
  `system:serviceaccount:<ns>:<agent>`.
- **Identity mapping:** Kubernetes and GitLab both authenticate against the bank IdP. A mapping
  (`ship.yaml: identities`, or the OIDC email claim) relates `requestedBy` to a GitLab username.
- **V8, independent approval:** at least `ledgerMinApprovals` approvals from users who are neither
  the MR author, nor the `Requested-by` identity, nor ship's bots. Enforced:
  1. **pre-merge** (preferred, Ultimate): `ship-controller` serves a GitLab **external status check**
     on the ledger project. The MR cannot merge until it reports V8 as passed.
  2. **at deploy** (fail closed, all tiers): `ship deploy` refuses to run a driver for a ledger MR
     that violates V8, and `verify-env-mr` already checks independent approval and is extended with
     the requester exclusion.

### 12.5 Who may promote what

- Kubernetes RBAC: `create` on `promotions` in a team namespace.
- Namespace → apps: a ValidatingAdmissionPolicy with a parameter (ConfigMap per namespace, or the
  namespace annotation `ship.dev/apps: SV-API_GW_MGT,…`) rejects `spec.app` outside the team's apps.
- This narrows proposing only. Approval is still GitLab's approval policy.

### 12.6 Other entry points

`ship promote` (CLI) and MRs opened by hand or by agents stay valid; the ledger is the source of truth.
Option (§12.8 Q2): the controller **imports** them as read-only Promotion CRs (label
`ship.dev/origin: gitlab`), so `kubectl get promotions` shows every promotion, not just CR-created ones.

### 12.7 Implementation sketch

- `ship controller` subcommand (same binary; reuses `policy`, `ledger`, `gitlab`, `freight`), built on
  controller-runtime. The CRD is a hand-written OpenAPI v3 schema with printer columns.
- Ledger MR creation goes through the GitLab Commits API, so the controller needs no git checkout. The
  pre-check reads `ship.yaml` and pin history through the API. Alternatively it keeps a cached bare
  clone for V5 history.
- Sandbox: add **k3s** to the compose file and run the controller as a compose service with the
  k3s kubeconfig. Scenario: `kubectl apply` a Promotion, then watch the phases go
  Pending → Proposed → AwaitingApproval → (reviewer approves in GitLab) → Deploying → Succeeded.
  Negative cases: a spoofed `requestedBy` is denied by admission; self-approval is blocked by V8; a
  skipped stage goes to Rejected; deleting the CR while the MR is open closes the MR.

### 12.8 As built

- `ship controller` (controller-runtime), manifests in `deploy/` (CRD, admission policy, RBAC, Deployment).
- Spec immutability is enforced by a CRD transition rule (`self == oldSelf`); the admission policy
  enforces `requestedBy` and the namespace→apps restriction.
- Ledger branches are `promote/<app>/<stage>/<digest12>-<uid8>`, so they are unique per Promotion.
- **V8 is enforced in three places**, all sharing `internal/approval`:
  - pre-merge: `StatusChecker` (`--status-check-id`) for every open ledger MR. A Promotion-originated MR
    also needs its CR to still exist with the same UID, and the CR's `requestedBy` counts as a requester;
  - post-merge: `ship deploy` refuses to run drivers (`--require-approval`, default on);
  - `verify-env-mr` refuses to approve env MRs of non-V8 promotions.
- Requesters are the union of `Requested-by` trailers in the MR description and every commit (plus the
  CR's `requestedBy`). Extra trailers can only make V8 stricter.
- **Not covered by the CE sandbox:** the external status check itself (Ultimate). It is unit-tested
  against a fake GitLab, and the sandbox shows the controller's `Approved=False` plus the deploy-time refusal.
- **Known limitation:** someone who force-pushes the promotion branch can replace the commit and
  remove its `Requested-by` trailer. That person is then a commit author, so in production
  `prevent_approval_by_commit_author` covers them; the status check also covers CR-originated MRs
  via the CR. Protecting `promote/*` branches would close this fully.

### 12.9 Open questions

1. **Management cluster:** is there a platform-owned cluster where ship-controller may run? Flux owns
   the target clusters, and the controller must not live there.
2. **Import:** should MRs from the CLI or agents be mirrored as read-only Promotion CRs, so there is one list of all promotions?
3. **V8 enforcement:** use an external status check (Ultimate, pre-merge), deploy-time refusal
   (post-merge, fail closed), or both? Is "requester ≠ approver" required for every stage or only PROD?
4. **Identity mapping:** does the Kubernetes username equal the GitLab username via the shared IdP, or is
   an explicit mapping needed?
5. **More CRDs:** read-only `Stage` and `Freight` mirrors (`kubectl get stages` showing current Freight
   per app), as in Kargo? This is cheap to add once the controller exists.
