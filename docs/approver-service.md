# Design: ship approver service

Status: **implemented** (`ship approver`, `internal/approver`, `deploy/approver.yaml`). See §10.
Resolves [design.md §11 Q11](design.md) and [setup.md §4](setup.md#4--protecting-the-ship-approver-credential), option A.

## 1. Problem

Env-repo MRs opened by `ship-bot` (the Crossplane driver) are approved by
`ship-approver`, but only after `ship verify-env-mr` proves that the MR is the
exact output of a merged, independently approved ledger promotion.

Today that check runs **inside the env repo's MR pipeline** and approves with a
token held in a CI variable. That pipeline also runs jobs written by the app team.
Any of those jobs could read the token and approve anything, in any env repo.

```
 today                                           proposed
 ─────                                           ────────
 env MR pipeline                                 env MR pipeline
 ├── team-checks      ← can read the token       ├── team-checks
 └── ship-verify-env-mr                          └── ship-verify-env-mr --no-approve   (report only)
       └── approves as ship-approver
                                                 ship approver service  (token lives only here)
                                                   ◄── webhook: MR opened / updated
                                                   └── same Verify() code → approves as ship-approver
```

**Goal:** the approver token never enters CI. Nothing about *what* is verified changes.

## 2. What it is

A new subcommand of the existing binary, `ship approver`, which runs as a
long-lived service next to `ship controller`. It contains no new verification
logic. It calls the same `envmr.Verifier.Verify(project, iid, approve=true)`
that the CLI uses today.

```
 GitLab ──webhook (MR event)──► ship approver ──► queue (dedup by project+MR)
                                     ▲                    │
                                     │                    ▼
                         periodic sweep of open    worker: Verify()
                         ship/* MRs (missed hooks)  ├─ all checks pass → approve (pinned to head SHA) + note
                                                    └─ any check fails → note listing failures, no approval
```

## 3. How it works

1. **Trigger.** GitLab sends merge-request webhooks for the env groups
   (`sda/services`, `sda/applications`) to the service. Group webhooks need
   Premium or above; a system hook also works.
2. **Authenticate the webhook.** Reject the request unless the `X-Gitlab-Token`
   header matches the shared secret (constant-time compare).
3. **Treat the payload as untrusted.** Only the project ID and MR number are taken
   from it. Everything else (author, branch, diff, ledger state, approvals) is
   re-read from the GitLab API, exactly as `Verify()` already does. A forged or
   replayed webhook can at most cause a re-check.
4. **Filter cheaply.** Ignore anything whose source branch is not `ship/*`, and
   events that are not open/reopen/update with new commits.
5. **Queue and dedupe.** One pending item per (project, MR). A burst of events
   becomes one verification.
6. **Verify and approve.** Run `Verify()`. On success it approves **pinned to the
   head SHA it verified**. GitLab's approve API takes `sha` and refuses if the MR
   moved in the meantime, so the service can't approve content it didn't check.
   It also posts a note listing every passed check.
7. **Push after approval.** `remove_approvals_with_new_commit` (already in the env
   approval policy) drops the approval. The push sends a new webhook and step 6
   runs again.
8. **Sweep.** Every few minutes, list open `ship/*` MRs in the env groups and queue
   any that ship-approver hasn't approved. This covers missed webhooks and service
   downtime.
9. **Idempotent.** If ship-approver has already approved the current head, nothing
   happens (existing behaviour of `Verify()`).

The CI job stays, as `ship verify-env-mr --no-approve`, for visibility: a failing
check shows red in the MR. It needs only a read token. The service is the only
thing that approves.

## 4. Credentials

| Credential | Where it lives | Can do |
|---|---|---|
| `ship-approver` token | Kubernetes Secret (or Vault) mounted **only** into the approver Deployment | read env repos and the ledger, approve, comment |
| webhook secret | same Secret, and the GitLab hook config | authenticates webhooks |
| registry read | the approver's pull credentials | resolve Freight (re-render the claim) |

`ship-approver` stays a member of the `env-approvers` group and authors nothing.
The CI variable `SHIP_APPROVER_TOKEN` is removed from env repos.

## 5. Threats

| Threat | Mitigation |
|---|---|
| Team CI job steals the approver token | Token is not in CI any more (the point of this design) |
| Forged or replayed webhook | Secret header, and the payload is never trusted (§3.3). Worst case: a needless re-check |
| MR changes between verify and approve | Approval is pinned to the verified SHA, and new commits reset approvals |
| `ship-bot` compromised, opens a malicious env MR | Unchanged from today: `Verify()` requires a merged, V8-approved ledger promotion and an exact re-render of the claim |
| Approver service compromised | It can approve env MRs, the same power the CI token has today, but now confined to one Deployment. Limit network access, rotate the token, alert on approvals with no matching verify log line |
| Service down | Ship MRs wait (auto-merge doesn't fire). Fails closed; the sweep catches up after restart |

## 6. Deployment

- `deploy/approver.yaml`: a Deployment in `ship-system` (1 replica; approvals are
  idempotent, so a brief overlap during rollout is harmless), a Service, and
  an Ingress reachable from GitLab only.
- Endpoints: `POST /webhook`, `/healthz`, `/metrics` (verifications,
  approvals, failures by check, queue depth).
- Config: `--ledger-project`, `--groups sda/services,sda/applications`,
  `--sweep 5m`, `--listen :8080`; secrets from environment.

## 7. Sandbox and tests

- **Unit:** the webhook handler (rejects a bad secret, ignores non-`ship/*`
  branches, dedupes), and a worker test using the fake GitLab from the controller
  tests.
- **E2E:** add the approver to `e2e/compose.yaml`. GitLab CE has no group hooks,
  so the bootstrap registers project hooks on each env repo, plus "allow requests
  to the local network" so GitLab can reach the container. Change the env CI
  fixture to `--no-approve` and remove the approver token from CI. The scenario
  must still pass, and a new step checks that env-repo CI variables no longer
  contain an approver token.

## 8. Plan

1. Factor the approve step so `Verify()` is callable from a service with no CI
   environment variables (it already takes project and MR number as arguments).
2. `ship approver`: HTTP handler, queue, worker, sweep.
3. Manifests, setup.md §4 rewritten to "this is how it's done", and `env-verify.yml`
   switched to `--no-approve`.
4. Sandbox wiring and scenario step.

Roughly 400–600 lines of Go plus manifests. No change to the ledger flow, the
CRD or the drivers.

## 9. Open questions

1. **Humans approving `ship/*` MRs.** Today a human in `env-approvers` could approve
   a ship MR whose verification failed, bypassing provenance. Should ship MRs be
   approvable *only* by ship-approver? GitLab approval policies can't scope by
   source branch, so that would need e.g. a CODEOWNERS section on the claim file,
   or a policy job that fails ship MRs not approved by ship-approver.
2. **Separate service, or inside `ship controller`?** Separate is recommended:
   the controller holds the promoter token, so keeping the two apart means one
   compromise doesn't give both. One process is simpler to run.
3. **Group webhooks vs system hook.** Group webhooks are scoped to the env groups
   (preferred). A system hook needs instance-admin setup.
4. **Where it runs.** The same management cluster as the controller (design.md
   §12.9 Q1)?

## 10. As built

- `ship approver --listen :8080 --groups … --ledger-project … --sweep 5m`, with
  `SHIP_APPROVER_TOKEN` and `SHIP_WEBHOOK_SECRET` (at least 16 characters) from the
  environment. It refuses to start without them.
- One worker processes the queue serially, so two approvals of the same MR can't
  race. A failure note is posted once per head SHA, not once per event. Merged and
  closed MRs are skipped.
- `ship verify-env-mr --no-approve` uses `SHIP_READ_TOKEN` (a `read_api` token) when
  set, so env-repo CI holds no token that can approve.
- `/metrics`: plain-text counters (`ship_approver_{webhooks,ignored,verified,approved,rejected,errors}_total`,
  `ship_approver_queue_depth`).
- Sandbox: project hooks on every env repo → `http://approver:8080/webhook`; the
  approver token was removed from env CI and replaced by a `read_api` token. CE
  cannot *require* approvals, so the sandbox env CI job waits for ship-approver's
  approval before succeeding. On Premium/Ultimate the approval policy does this.
  The scenario checks that the token is absent from CI, that the approval came from
  the service, and that a tampered MR gets an explanatory note and no approval.
- Not done: §9 Q1 (restricting human approval of `ship/*` MRs) is still open.
