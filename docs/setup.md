# Setting up ship with the GitLab compliance framework

How an organization wires ship into GitLab (Ultimate) so that the compliance
framework, not convention, guarantees the promotion process. Companion to
[design.md](design.md).

> Policy YAML below follows current GitLab security-policy syntax; verify field
> names against your instance's version before applying.

## 1. Who owns what

| Owner | Owns |
|---|---|
| **Compliance / platform team** | compliance frameworks, the security-policy project (pipeline execution + approval policies), the ship CI image, `ship.yaml` (via CODEOWNERS), the `ship-bot` / `ship-approver` service accounts |
| **Release approvers** (group) | approving ledger MRs (the one human gate) |
| **App / service teams** | build pipelines (publish Freight + evidence), Octopus deployment processes, non-mapped fields of their claims |
| **People and agents** | proposing promotions (ledger MRs) |

## 2. The building blocks

```
GitLab (Ultimate)
│
├── Compliance frameworks (labels on projects)
│     ship-ledger ──► platform/ledger
│     ship-env    ──► sda/services/*/*, sda/applications/*/*   (default framework on those groups,
│                                                                so new env repos get it automatically)
│
├── Security policy project  platform/ship-policies   (linked to the top-level group)
│     ├── policy.yml
│     │     pipeline_execution_policy:  ledger pipeline   → scope: ship-ledger
│     │     pipeline_execution_policy:  verify-env-mr job → scope: ship-env
│     │     approval_policy:            ledger approvals  → scope: ship-ledger
│     │     approval_policy:            env approvals     → scope: ship-env
│     └── ci/
│           ├── ledger.yml       (validate / plan / deploy jobs)
│           └── env-verify.yml   (verify-env-mr job)
│
├── Groups
│     release-approvers   humans allowed to approve promotions
│     env-approvers       humans + ship-approver
│
└── Service accounts
      ship-bot        opens env MRs, pushes ship/* branches          (no approve rights that matter)
      ship-approver   approves env MRs after provenance check        (token only in the approver service; never authors anything)
```

Policies are enforced from the policy project, so a team cannot delete the
validate job from its `.gitlab-ci.yml`, weaken approval settings, or skip
verification. Changing them is itself an audited MR in `platform/ship-policies`.

## 3. Policies

### 3.1 Ledger: the pipeline is owned by policy

```yaml
pipeline_execution_policy:
  - name: ship ledger pipeline
    enabled: true
    pipeline_config_strategy: override_project_ci   # the ledger repo's own CI is ignored
    content:
      include:
        - project: platform/ship-policies
          file: ci/ledger.yml
    policy_scope:
      compliance_frameworks:
        - id: <ship-ledger framework id>
```

```yaml
# ci/ledger.yml
default:
  image: registry.example.com/platform/ship:<version>
variables:
  GIT_DEPTH: 0                      # V5 needs full pin history

ship-validate:
  stage: test
  rules: [{ if: $CI_PIPELINE_SOURCE == "merge_request_event" }]
  script: [ship validate]           # base = CI_MERGE_REQUEST_DIFF_BASE_SHA

ship-plan:
  stage: test
  rules: [{ if: $CI_PIPELINE_SOURCE == "merge_request_event" }]
  script: [ship plan]

ship-deploy:
  stage: deploy
  rules: [{ if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH }]
  resource_group: ship-deploy       # one deploy at a time, in merge order
  script: [ship deploy]             # base = CI_COMMIT_BEFORE_SHA
  # SHIP_GITLAB_TOKEN (ship-bot), OCTOPUS_API_KEY: protected+masked, main only
```

### 3.2 Ledger: approvals

```yaml
approval_policy:
  - name: promotion approval
    enabled: true
    rules:
      - type: any_merge_request
        branch_type: protected
        commits: any
    actions:
      - type: require_approval
        approvals_required: 1            # e.g. 2 if PROD needs four eyes beyond the requester
        group_approvers: [release-approvers]
    approval_settings:
      prevent_approval_by_author: true       # the requester (person or agent) can't approve
      prevent_approval_by_commit_author: true
      remove_approvals_with_new_commit: true
      block_branch_modification: true
      prevent_pushing_and_force_pushing: true
    policy_scope:
      compliance_frameworks: [{ id: <ship-ledger framework id> }]
```

Plus, in the ledger project: `main` protected (no direct push), "pipelines must
succeed", and CODEOWNERS making `ship.yaml`, `mappings/` owned by the platform
team. Stricter rules for `ledger/*/PROD.yaml` can be added via CODEOWNERS sections.

### 3.3 Env repos: inject verification, keep normal approvals

```yaml
pipeline_execution_policy:
  - name: ship env verify
    enabled: true
    pipeline_config_strategy: inject_policy      # team CI still runs; this job is added
    content:
      include:
        - project: platform/ship-policies
          file: ci/env-verify.yml
    policy_scope:
      compliance_frameworks: [{ id: <ship-env framework id> }]

approval_policy:
  - name: env change approval
    enabled: true
    rules: [{ type: any_merge_request, branch_type: protected, commits: any }]
    actions:
      - type: require_approval
        approvals_required: 1
        group_approvers: [env-approvers]         # humans + ship-approver
    approval_settings:
      prevent_approval_by_author: true           # ship-bot can never approve its own MR
      remove_approvals_with_new_commit: true     # any push after verification voids approval
    policy_scope:
      compliance_frameworks: [{ id: <ship-env framework id> }]
```

```yaml
# ci/env-verify.yml
ship-verify-env-mr:
  stage: .pipeline-policy-pre                  # runs before (and independent of) team jobs
  image: registry.example.com/platform/ship:<version>
  rules: [{ if: $CI_MERGE_REQUEST_SOURCE_BRANCH_NAME =~ /^ship\// }]
  script: [ship verify-env-mr --no-approve]    # report only; SHIP_READ_TOKEN (read_api), SHIP_LEDGER_PROJECT
```

Human MRs need a human from `env-approvers`. Ship MRs get their approval from
`ship-approver`, which only the approver service (§4) holds, and only after the
same provenance check passes.

## 4. ⚠ Protecting the ship-approver credential

`verify-env-mr` runs in **env-repo MR pipelines** that also contain team-written
jobs. If the approver token were a plain CI variable, any team job in such a
pipeline could read it and approve anything. And protected variables are not
exposed to MR pipelines of unprotected branches anyway. Options:

| Option | How | Trade-off |
|---|---|---|
| **A. Approver service (recommended)** | A small service subscribed to env-repo MR webhooks runs the same verification (`ship verify-env-mr` logic) and approves. The token never enters CI. | One small service to operate; verification result is posted as an MR note + audit event. |
| B. Vault + OIDC | The job exchanges its GitLab ID token at Vault; Vault only issues the approver token when claims match (project in env groups, ref `ship/*`, job from the policy CI config). | Depends on the ID-token claims reliably identifying policy-injected jobs; needs verification on our GitLab version. |
| C. Protected `ship/*` branches | Protect `ship/*` (push: ship-bot only) so the protected variable is available, and run env MR pipelines with `override_project_ci` for those branches. | Team jobs don't run on ship MRs; more policy complexity. |

**Implemented: option A**, `ship approver` ([approver-service.md](approver-service.md),
`deploy/approver.yaml`). Setup:

1. Create the Secret `ship-system/ship-approver` with the ship-approver token and a
   random webhook secret, and deploy `deploy/approver.yaml`.
2. Add a group webhook on each env group (`sda/services`, `sda/applications`):
   URL `https://<approver>/webhook`, the webhook secret, merge request events only.
3. Give env-repo CI only a `read_api` token as `SHIP_READ_TOKEN` (for the report-only
   `ship verify-env-mr --no-approve` job). Never put the approver token in CI.

## 5. End-to-end flow for one promotion

```
 app team build pipeline
   build → push reg/gw:1.4.2 (OCI, immutable tags) → ship attest (test evidence)
                                   │
 person/agent                      ▼
   ship promote --app SV-API_GW_MGT --stage PROD --freight sha256:4f1c --push
                                   │
 platform/ledger  [framework: ship-ledger]
   MR ─► policy pipeline: ship-validate, ship-plan        (override_project_ci: can't be skipped)
      ─► approval policy: 1 × release-approvers, not the requester
      ─► merge (pipelines must succeed)
      ─► policy pipeline on main: ship-deploy
              ├─ crossplane app ─► env MR in sda/services/SV-API_GW_MGT/PROD
              └─ octopus app    ─► Octopus release 1.4.2 → Production
                                   │
 sda/services/SV-API_GW_MGT/PROD  [framework: ship-env]
   env MR (author ship-bot)
      ─► team compliance jobs + injected ship-verify-env-mr
      ─► ship-approver approves (provenance proven)       (approval policy satisfied)
      ─► auto-merge ─► Flux ─► Crossplane claim Ready
                                   │
 evidence: ship deploy records "deployed/PROD" on the Freight
           (V5 requires deployed/<upstream> before the next stage accepts it)
```

## 6. What the auditor sees

- **Compliance center**: ledger and env repos labelled with their framework,
  standards adherence (author can't approve, approvals reset on push, …) green.
- **Per promotion**: ledger MR (requester, reason, validate output, plan diff,
  approver) ↔ env MR (`Ledger-MR` trailer, verify note listing every passed
  check, ship-approver approval) ↔ Octopus deployment comment (ledger MR URL).
- **Audit events** (streamable to the SIEM): approvals, merges, policy changes,
  service-account token use.
- **History**: `ship history --app … --stage …` = git log of the pin.

## 7. Onboarding a new service

1. App team publishes Freight to `registry/…/<service>` with immutable tags and a
   `org.opencontainers.image.version` annotation.
2. Env repos created from template under `sda/services/<service>/<STAGE>`
   (group default framework → `ship-env` applied automatically), claim committed by the team.
3. Policy MR to `platform/ledger`: add the app to `ship.yaml`
   (`deploy: { template: service }`), approved by CODEOWNERS.
4. First promotion to the first stage.
