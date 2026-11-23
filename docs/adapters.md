# Design: forge and secret adapters

Status: **steps 1–3 implemented** (forge interface + GitLab adapter, contract
suite, secret resolver with `env`/`file`). Steps 4–6 proposed. See §6 "As built".

Goal: ship runs on GitLab today. Make the forge (GitLab, GitHub, …) and the
secret store (CI variables, HashiCorp Vault, CyberArk, Azure Key Vault, …)
pluggable, without spreading `if gitlab … else github …` through the code, and
without weakening the controls.

## 1. What is coupled today

**Forge.** Eight files import `internal/gitlab`, and they use 22 client methods.
Other GitLab-specific pieces:

- the `CI_*` variables read in `cmd/ship`;
- push options in `ship promote --push`;
- the `X-Gitlab-Token` webhook header in the approver;
- the external status check API (V8 before merge);
- `PRIVATE-TOKEN` auth.

**Secrets.** Secrets reach ship only through environment variables:

- `SHIP_*_TOKEN`
- `SHIP_WEBHOOK_SECRET`
- `OCTOPUS_API_KEY`
- Kosli tokens, which are planned

There are two exceptions. Registry credentials come from the Docker keychain
(`authn.DefaultKeychain`). The controller's ledger clone URL may embed a
credential. Nothing in the codebase knows where a secret actually came from.

**Neither is coupled at the core.** The policy rules, ledger, config, freight,
drivers' Plan/Apply/Wait and the Kosli/Octopus code don't touch either one.

## 2. Forge adapter

### 2.1 Neutral vocabulary

| ship term | GitLab | GitHub | Others, later |
|---|---|---|---|
| `Repo` | project | repository | Bitbucket repo, Azure DevOps repo |
| `ChangeRequest` | merge request | pull request | PR |
| `Review` | approval | review (APPROVED) | approval / vote |
| `Check` | external status check | check run (from a GitHub App) | build status / PR status |
| `Comment` | note | issue comment | comment |
| `Identity` | user / service account | user / App (`app/<slug>`) | user / service principal |

### 2.2 Interface

The interface is grouped by role, not mirrored from the GitLab client, so each
adapter only has to make sense of things ship actually does:

```go
package forge

type Forge interface {
    Name() string // "gitlab", "github"
    Caps() Caps

    // Repo content
    File(ctx, repo, ref, path string) ([]byte, error)
    BranchSHA(ctx, repo, branch string) (string, error)
    IsAncestor(ctx, repo, ancestor, descendant string) (bool, error)
    Commit(ctx, repo string, c CommitSpec) (sha string, err error) // multi-file, on a branch
    DeleteBranch(ctx, repo, branch string) error

    // Change requests
    CR(ctx, repo string, id int) (*ChangeRequest, error) // state, author, head/base SHA, labels, description
    ListCRs(ctx, scope Scope, q CRQuery) ([]ChangeRequest, error) // repo or group/org
    OpenCR(ctx, repo string, s CRSpec) (*ChangeRequest, error)    // replaces push options
    CloseCR(ctx, repo string, id int) error
    Diffs(ctx, repo string, id int) ([]FileDiff, error)
    CRCommits(ctx, repo string, id int) ([]string, error)
    CRsForCommit(ctx, repo, sha string) ([]ChangeRequest, error)
    Reviews(ctx, repo string, id int) ([]Review, error) // who approved, at which head SHA
    Approve(ctx, repo string, id int, sha string) error  // pinned to SHA
    AutoMerge(ctx, repo string, id int, sha string) error
    Comment(ctx, repo string, id int, body string) error

    // Pre-merge V8
    ReportCheck(ctx, repo string, id int, sha string, c CheckResult) error

    // Identity
    Whoami(ctx) (Identity, error)
    IsServiceIdentity(ctx, id Identity) (bool, error) // bot / service account / App
}

type Caps struct {
    ServiceReviewCounts bool // a bot's approval counts toward required approvals
    NativeApprovalReset bool // approvals are dropped on push
    GroupListing        bool // list CRs across a group/org in one call
}
```

Two more small interfaces, each implemented once per forge:

```go
// CI context: what the running pipeline is about.
type CIContext interface {
    Detect() bool // GITLAB_CI / GITHUB_ACTIONS
    Repo() string
    CR() int
    BaseSHA() string   // MR diff base / PR base
    BeforeSHA() string // push "before"
    RunURL() string
}

// Webhooks: authenticate, then extract only (repo, CR id).
// The payload is still untrusted, as it is today.
type Webhook interface {
    Verify(r *http.Request, body []byte) error // X-Gitlab-Token / X-Hub-Signature-256 (HMAC)
    Parse(body []byte) (repo string, cr int, relevant bool, err error)
}
```

Selection: `ship.yaml` gets `forge: {type: gitlab, url: …}`. If it's missing,
ship detects the forge from the CI environment. The ledger refers to env repos
by plain path strings already, so the ledger format doesn't change.

### 2.3 Where the forges differ

| Concern | GitLab adapter | GitHub adapter |
|---|---|---|
| Pre-merge V8 | external status check | required check run, ruleset pinned to ship's App, so nobody else can post it |
| env-MR approval by ship-approver | service account approves (works today) | `Caps.ServiceReviewCounts` is likely false. The approver posts a required `ship/provenance` check, and the ruleset lets PRs from the ship bot skip human review. **Must be tested on the bank's org** |
| Group-wide sweep | `GroupMRs` | search API `is:pr is:open org:X head:ship/` |
| Auth | service-account PAT | GitHub App: installation tokens, valid for 1 h, scoped per repo |
| Webhook | shared token | HMAC-SHA256 |
| Opening a CR from `promote --push` | push options, or `OpenCR` | `OpenCR` |

Code paths branch on `Caps`, never on `Name()`.

### 2.4 Not abstracted: governance

Governance is set up per forge, in the setup docs, and isn't hidden behind the
interface. That covers:

- compliance frameworks
- pipeline execution policies
- approval rules
- protected branches
- CODEOWNERS
- `resource_group`

GitHub's equivalents are org rulesets that target custom properties, required
workflows, environments and `concurrency`. The controls are what auditors sign
off, and a leaky "neutral" layer over them would be worse than two clear
setup guides.

ship can still **check** governance. A `ship doctor` command per adapter reads
the forge's settings and fails if they're missing. For example: the ledger's
default branch isn't protected, the ship check isn't required, or approvals
aren't reset on push.

### 2.5 Testing

- One **shared contract test suite** (`forge/forgetest`) runs against every
  adapter, so the GitLab and GitHub implementations are held to the same behaviour.
- GitLab: the existing GitLab CE sandbox.
- GitHub: a fake validated against GitHub's official OpenAPI description
  (`github/rest-api-description`), using the same contract guard as the Kosli
  and Octopus fakes. Optionally, an e2e run against a real test org when
  credentials are provided (off by default).

## 3. Secret adapter

### 3.1 References, not values

Config and flags name **where** a secret lives. They never hold the value:

```yaml
# ship.yaml (or flags / env for the services)
secrets:
  octopus:   vault://kv/data/ship/octopus#api_key
  kosli:     vault://kv/data/ship/kosli#token
  forge:     vault-dyn://gitlab/token/ship-bot   # dynamic, short-lived
  webhook:   file:///var/run/secrets/ship/webhook
  servicenow: env://SNOW_PASSWORD
```

```go
package secret

type Provider interface {
    Scheme() string // "env", "file", "vault", "exec", …
    Get(ctx context.Context, ref Ref) (Secret, error)
}

type Secret struct {
    Value   []byte
    Expires time.Time // zero = static; set for leases / installation tokens
}

type Resolver interface { // caches until Expires (minus a margin), re-reads on rotation
    Get(ctx context.Context, name string) (Secret, error)
}
```

All consumers receive the `Resolver`, never raw values. These include:

- forge clients
- Octopus and Kosli clients
- the ServiceNow gate
- the registry keychain (a custom `authn.Keychain` backed by the resolver)
- the controller's git credentials

Long-running services fetch a fresh secret before it expires, so a rotation
doesn't need a restart.

### 3.2 Providers

| Scheme | Purpose | Notes |
|---|---|---|
| `env://` | default; today's behaviour | GitLab/GitHub CI variables, or secrets injected by the CI's own vault integration |
| `file://` | k8s Secrets, Vault Agent, External Secrets Operator, CSI driver | re-read when the file changes |
| `vault://` | HashiCorp Vault KV v2 | auth via workload identity: GitLab `id_tokens` / GitHub OIDC → Vault JWT auth; k8s SA → Vault Kubernetes auth. **No bootstrap secret** |
| `vault-dyn://` | Vault dynamic secrets (e.g. short-lived GitLab project tokens, cloud creds) | lease-aware; revoked on exit |
| `exec://` | anything else (CyberArk, Azure Key Vault, AWS SM, in-house) | same plugin model as drivers: `ship-secret-<name>` gets a JSON request on stdin and returns a JSON secret. The bank can add a provider without changing ship |

Built in: `env`, `file`, `vault`, `exec`. Other vendors start as `exec`
plugins and become built-in only if they're widely used.

### 3.3 Rules

- **Secrets never go into the ledger, MR reports, logs or evidence.** The
  resolver wraps values in a type whose `String()`/`MarshalJSON` print
  `[redacted]`. A test in each package that logs checks this.
- **Fail closed**: if a secret can't be resolved, the command fails. There's no
  fallback to an empty value or to another provider.
- **Least privilege per command.** `validate` and `verify-env-mr --no-approve`
  only resolve read credentials. Only `deploy` resolves the driver secrets,
  matching today's protected-variable split.
- **Prefer workload identity.** With Vault JWT or Kubernetes auth, plus GitHub
  App or Vault-issued forge tokens, there are no long-lived secrets in CI
  variables at all. Each token is short-lived and traceable to one pipeline run.

### 3.4 Honest note

GitLab CI (`secrets:` with Vault, Azure KV or GCP SM), GitHub Actions and
Kubernetes (External Secrets Operator, Vault Agent) can already inject secrets
as env vars or files. With `env://` and `file://`, ship works with all of them
**today**, without code changes.

A native `vault://` provider is worth it for two things:

1. **Dynamic, short-lived forge and driver tokens** for the long-running
   approver and controller.
2. **One place** that states which secret each command uses, which is
   reviewable and auditable.

If neither matters to the bank, steps 5 and 6 below can wait.

## 4. Plan

1. **Forge interface + GitLab adapter**: move `internal/gitlab` behind
   `forge.Forge`, `CIContext` and `Webhook`. Behaviour doesn't change, and the
   e2e must still pass all 47 checks. About 2–3 days.
2. **Shared forge contract tests** (`forgetest`), run against the GitLab sandbox.
3. **Secret resolver** with `env` and `file`, wired into every consumer, plus
   redaction. Behaviour doesn't change; current env var names become default
   refs. About 2 days.
4. **GitHub adapter**: App auth, check runs, HMAC webhooks, spec-validated fake,
   `ship doctor` for rulesets, and `docs/setup-github.md`. About 1–1.5 weeks.
   Most of the remaining risk is the approval-vs-check question in §2.3.
5. **`vault://`** with JWT and Kubernetes auth, and the sandbox gets a Vault
   dev-mode container (Vault runs in docker, so no fake is needed). About 3 days.
6. **`exec://` secret plugins**, plus `vault-dyn://` if dynamic tokens are wanted.

Steps 1–3 are refactors with no behaviour change and are worth doing on their
own. Steps 4–6 depend on the answers below.

## 5. Open questions

1. **Which forges?** Only GitHub (Enterprise Cloud or Server?), or Bitbucket or
   Azure DevOps too? The interface allows them, but each needs its own
   governance mapping.
2. **Which secret stores?** HashiCorp Vault, CyberArk (Conjur / CCP), Azure Key
   Vault? And does the bank already use GitLab's `secrets:` keyword, or ESO in
   the clusters? If yes, `env`/`file` may be enough for now.
3. **Workload identity:** is Vault JWT auth from GitLab `id_tokens` allowed? It
   removes every long-lived secret from CI variables.
4. **Dynamic forge tokens:** is Vault issuing short-lived GitLab tokens for
   ship-bot and ship-approver acceptable, or must those stay as managed service
   accounts?

## 6. As built (steps 1–3)

- **`internal/forge`**: the `Forge` interface, neutral types (`ChangeRequest`,
  states `open`/`closed`/`merged`), `CI` context and `Webhook`. Callers branch on
  nothing forge-specific. `Caps` is not added yet: nothing needs it until GitHub.
- **`internal/gitlab`**: the GitLab adapter. Changes from the old client:
  - lists are paginated completely;
  - a 404 maps to `forge.ErrNotFound`;
  - the token is a `secret.Source`, resolved on every request;
  - `ReportCheck` takes the external status check id as `Check.Key`.

  It also holds `DetectCI` (GitLab CI variables) and `Webhook` (`X-Gitlab-Token`;
  returns the project *id*, never the path from the untrusted payload).
- **`internal/forge/forges`**: selects the adapter from `SHIP_FORGE` (default:
  detected from CI, else `gitlab`) and `SHIP_FORGE_URL` (`SHIP_GITLAB_URL` is
  still accepted).
- **`internal/forge/forgetest`**: the shared contract suite. `make forgetest`
  runs it against the sandbox GitLab. On its first run it found a real bug:
  committing to an existing branch sent an empty `start_branch`, which GitLab
  rejects.
- **`ship.yaml`**: the `gitlab:` section is now `forge:` (same keys, plus
  `type`). `gitlab:` is still accepted; setting both is an error.
- **`internal/secret`**:
  - **Resolver:** logical names default to the old environment variables, so
    behaviour is unchanged. Each name can be overridden with
    `SHIP_SECRET_<NAME>=<ref>`.
  - **Providers:** `env` and `file`. `file` is re-read on every use, so rotation
    needs no restart.
  - **Redaction:** `Value` redacts itself in every fmt verb, JSON, text and slog
    (tested).
  - **Fail closed:** a missing secret is an error, never an empty value.
  - **Change from §3.1:** secret locations are *not* in `ship.yaml`. They depend
    on where ship runs (a CI job or the approver pod), not on the ledger.
- **Consumers:**
  - every forge client takes a `secret.Source`;
  - the Octopus driver reads `octopus-api-key` from `driver.Deps.Secrets`;
  - the controller resolves its git clone credentials on every fetch, and git
    errors are redacted.
- **Gaps closed after steps 1–3:**
  - `ship promote --push --open-via auto|push-options|api`. `auto` keeps GitLab
    push options on GitLab (only the person's git credentials needed) and uses
    the forge API (`OpenCR`, `forge-token`) on any other forge. With `api`, the
    repo path comes from `--forge-repo`, the CI context, or the remote URL. If the
    push succeeds but opening the CR fails, the error says the branch is
    already pushed.
  - Registry credentials: a new optional secret `registry-auth` (default
    `env://SHIP_REGISTRY_AUTH`) holds a Docker config JSON, e.g. a mounted
    `kubernetes.io/dockerconfigjson` Secret via `file://`. It is consulted
    first and re-read on every lookup; when it is unset, or has no entry for
    the registry, ship falls through to Docker's keychain as before. A
    resolver error fails closed instead of falling through.
- **Verification:**
  - unit tests;
  - the contract suite against the sandbox GitLab;
  - the full e2e scenario (47/47 checks), with the existing environment
    variables unchanged.
