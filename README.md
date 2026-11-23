# ship

Promotion as merge requests. A release (**Freight**) is an immutable OCI digest.
Promoting it to a stage is a merge request that changes one pin file in a central
**ledger** repo. The forge's own controls (protected branches, approval rules,
pipeline policies, audit events) govern every step. ship adds checks and drivers;
it holds no state of its own.

## Benefits

- **One audit trail.** The ledger's git history records what should run where,
  who proposed it, who approved it and why.
- **Compliance by the forge.** Approvals, separation of duties and pipeline
  policies are the forge's, not a parallel system to certify.
- **Fail-closed checks.** Digest-only pins, ordered stages, required evidence and
  independent approval are enforced in CI before and after merge.
- **Artifact and target agnostic.** Any OCI artifact; drivers deploy through
  Crossplane, Octopus or your own plugin.
- **Portable.** Forges and secret stores sit behind adapters (GitLab, `env`,
  `file` today).

## Architecture

```
          person / agent / Promotion CR
                       │  ship promote
                       ▼
 ┌──────────── ledger repo (forge) ────────────┐
 │ ledger MR ─► ship validate, ship plan       │
 │           ─► independent + human approval   │
 │ merge     ─► ship deploy                    │
 └──────────────────────┬──────────────────────┘
                        │ drivers
        ┌───────────────┼───────────────┐
        ▼               ▼               ▼
   crossplane        octopus      exec / plugin
   (env repo MR ─► ship approver ─► GitOps)

 OCI registry: Freight = digest; evidence = OCI referrers
```

Details: [design](docs/design.md) · [setup](docs/setup.md) ·
[adapters](docs/adapters.md) · [approver service](docs/approver-service.md) ·
[comparison](docs/comparison.md) · [reference](docs/reference.md). Development tasks: `mise tasks`.
