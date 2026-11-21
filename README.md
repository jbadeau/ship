# ship

Promotion as merge requests: Kargo's Freight/Stage/Promotion model with GitLab as
the only workflow engine, so the GitLab compliance framework governs every step.

- **Design:** [docs/design.md](docs/design.md)
- **Organization setup (compliance framework, policies):** [docs/setup.md](docs/setup.md)

```
ship promote   --app A --stage S --freight sha256:… --push   # person/agent proposes (ledger MR)
ship validate                                                  # MR pipeline, fail-closed (V1–V7)
ship plan                                                      # MR pipeline, driver dry-run
ship deploy                                                    # main pipeline, hands pins to drivers
ship verify-env-mr                                             # env-repo MR pipeline, provenance → ship-approver
ship status | history | evidence | attest
ship controller                                                # Promotion CRs -> ledger MRs (+ V8 status check)
```

```yaml
# kubectl create -f - (in a team namespace; requestedBy must be you)
apiVersion: ship.dev/v1alpha1
kind: Promotion
metadata: { name: gw-prod-1-4-2 }
spec: { app: SV-API_GW_MGT, stage: PROD, freight: "sha256:…", requestedBy: alice, reason: release 1.4.2 }
```

Drivers: `crossplane` (env-repo MR with a mapped claim), `octopus` (release + deployment),
`noop`, `exec`, or any `ship-driver-<name>` on PATH speaking JSON over stdin/stdout.
Repo layouts are `deployTemplates` in `ship.yaml`, not code.

## Develop

```
make test                 # unit tests (in-memory OCI registry, scratch git repos, fake Octopus)
make e2e-up               # GitLab CE + runner + registry + k3s (docker compose, ~2 min first boot)
make e2e-bootstrap        # users, tokens, groups, ledger + env repos, runner, CRD + controller
make e2e-run              # the scenario in e2e/scenario.sh (~6 min)
make e2e-down             # remove everything, including volumes
```

The sandbox approximates Ultimate-only controls (compliance frameworks, pipeline
execution and approval policies) with committed CI files and project settings.
