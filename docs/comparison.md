# ship compared with Kargo and other promotion tools

Status: **analysis + proposals, for review.** Nothing here is implemented yet.
Companion to [design.md](design.md).

## 0. One-page summary

ship's core bet is different from every tool below: **GitLab is the only workflow
engine**. A promotion is an MR, the compliance framework enforces it, and ship
only checks and executes. No other tool does exactly this. The closest are
Telefonistka and GitOps Promoter (promotion = PR, gates = SCM checks), but they
promote *config directories/branches*, not immutable artifacts.

From Kargo, Google Cloud Deploy, GitOps Promoter, Telefonistka and Octopus, the
ideas worth taking, ranked by value for a bank:

| # | Idea | Taken from | ship today | Proposal | Size |
|---|---|---|---|---|---|
| P1 | **Re-validate at deploy time** | Cloud Deploy (snapshot / mismatch check) | `ship deploy` re-checks only V8 | Re-run V1–V7 against the merged state before any driver runs | S |
| P2 | **External gates** (change windows, change tickets, CMDB…) | Kargo promotion windows, Cloud Deploy deploy policies, Spinnaker execution windows | none | Pluggable gate checks (ServiceNow first) run at validate *and* deploy; see §6 | M |
| P3 | **Rollback as a first-class promotion** | Cloud Deploy rollback, Kargo "promote older Freight" | possible only if V5 happens to pass | Freight already deployed in *this* stage is eligible again; `ship rollback` | S |
| P4 | **Verification in the stage (verified ≠ deployed)** | Kargo verification (AnalysisTemplates), Cloud Deploy `verify` | V5 needs `deployed/<upstream>` | Optional per-stage verification; record `verified/<stage>`; V5 uses it | M |
| P5 | **Continuous-occupancy soak** | Kargo `requiredSoakTime` | soak counts from *first* deploy, even if replaced since | Count only time the Freight was actually the live pin upstream | S |
| P6 | **Fan-in: several upstream stages** | Kargo `sources.stages` + `availabilityStrategy: All/OneOf`; Telefonistka fan-in paths | exactly one upstream | `upstream: [QA, UAT]`, `upstreamPolicy: all \| any` | S |
| P7 | **Per-stage approval rules** | Cloud Deploy approval per target, Octopus lifecycles | one global approval count | `stages[].approvals`; DEV 0 (decided), PROD 2 | S |
| P8 | **Emergency path (governed bypass)** | Kargo "approve Freight for stage", Octopus channels | none | `Emergency:` trailer, skips V5/soak/windows only, needs a break-glass group, always visible | M |
| P9 | **Explain eligibility ("why not?")** | Kargo UI (verifiedIn / approvedFor per Freight) | `ship status` shows pins only | `ship freight <digest>`: per stage, eligible or the exact blocking rule | S |
| P10 | **Drift visible in status** | Kargo stage health, Telefonistka drift warnings | pin shown even if never deployed | `ship status` marks pins without `deployed/<stage>` evidence | S |
| P11 | **Signed, standard evidence** | SLSA / in-toto / Sigstore (all modern supply-chain tools) | unsigned custom referrers | in-toto attestations, cosign-signed, identity policy in ship.yaml | L (own design) |

Deliberately **not** adopted (§4): an auto-promotion engine, a promotion-step
DSL, environment branches, an Argo CD dependency, and CRDs as the source of truth.

Suggested order: **P1, P3, P9, P10** (small, close real gaps, help people understand
the system), then **P2, P7** (bank governance), then **P5, P6, P4, P8**, and
**P11** as its own design.

---

## 1. The landscape in one table

The GitOps Promoter project frames every tool by three questions:
*what* is promoted, *how* is the change held, and *when* does it move.

| Tool | What is promoted | How it's held | When it moves | Source of truth |
|---|---|---|---|---|
| **ship** | immutable OCI digest per app/stage | GitLab MR on the ledger | GitLab approval policy + pipeline (V1–V8) | ledger git history |
| **Kargo** | Freight: a content-hashed bundle of image digests, git commits, Helm charts | Promotion CR, PRs, commits | stage DAG + rules (verification, soak, manual approval, auto-promotion) | Kubernetes CRs (Freight, Stage, Promotion) |
| **GitOps Promoter** (argoproj-labs) | hydrated manifests per environment | automated PR from a staging branch to the live branch | commit statuses + branch protection | environment branches |
| **Telefonistka** | environment directory contents | automated PR, optional auto-merge | branch protection; promotion paths in `telefonistka.yaml` | env directories in the repo |
| **Google Cloud Deploy** | release: immutable snapshot of rendered manifests + pipeline config | rollout pending approval | approval, verify job, automation rules (wait, cron) | Cloud Deploy API |
| **Octopus Deploy** | release: package versions + variable snapshot | deployment in a lifecycle phase | lifecycle phases, manual interventions, channels | Octopus database |
| **Codefresh GitOps** | JSON paths in manifests | PR or commit | promotion policies (deprecated) | git |

## 2. Concept by concept

### 2.1 The unit of promotion

- **Kargo Freight** bundles several artifacts (images, commits, charts). Its name is
  a SHA-1 of their canonical form, and it has a mutable human alias.
- **Cloud Deploy releases** go further: manifests are rendered **once** at release
  time and stored. Every target, every rollback and every redeploy applies those
  stored manifests.
- **ship** pins one OCI digest per app. That's already content-addressed, and the
  digest *is* the identity. Multi-artifact bundles fit without changes: the build
  publishes one OCI artifact or index that references the rest, and ship stays
  agnostic about it.

**Verdict:** keep it. Document the "bundle = one OCI index/artifact" pattern. Show
the `org.opencontainers.image.version` annotation as the human-readable alias in
`status`, `history` and the MR title (cheap; part of P9).

**Not taken:** render-once. ship renders the claim per stage at deploy time, from
the current env repo, because teams own the non-mapped fields. Render-once would
take that ownership away. P1 covers the safety aspect instead.

### 2.2 Ordered progression and fan-in

- **Kargo:** a Stage requests Freight from origins (`sources.direct`) or from
  upstream Stages (`sources.stages`). With several upstreams, `availabilityStrategy`
  is `OneOf` (default) or `All`. Freight counts as available upstream only once it
  is **verified** there, not merely deployed.
- **Telefonistka:** supports fan-out and fan-in promotion paths.
- **ship:** a single `upstream`. V5 now requires the upstream pin **and**
  `deployed/<upstream>` evidence.

**P6 — fan-in.** `upstream: [QA, UAT]` with `upstreamPolicy: all` (default) or
`any`. V5 is checked per upstream and combined. Config validation still requires
upstreams to be declared earlier, which keeps the graph acyclic. ~50 lines.

**P4 — verification.** Kargo's strongest idea: *deployed* is not *verified*.

- After a successful deploy, the stage's verification runs.
- Kargo uses Argo Rollouts AnalysisTemplates, and Argo CD health when none is
  defined. Cloud Deploy runs `skaffold verify` and metric analysis.
- Proposal: `stages[].verification` names an exec command or external driver
  (the same plugin contract as drivers). `ship deploy` runs it after Wait and
  records `verified/<stage>`.
- If a stage declares verification, V5 for the next stage requires `verified`,
  otherwise `deployed`.
- `ship verify --app --stage` re-runs it, like Kargo's `kargo verify stage`.
- It runs in the ledger pipeline, so GitLab still owns execution.

### 2.3 Soak time

- **Kargo `requiredSoakTime`:** the Freight must have **continuously occupied** the
  upstream Stage for the duration. It blocks manual promotions too.
- **Cloud Deploy:** a `wait` on the promote automation rule.
- **ship:** counts from the *first* `deployed/<upstream>` evidence, even if the
  Freight was replaced upstream an hour later.

**P5.** Measure occupancy. The soak interval runs from the deploy time to the
moment the upstream pin moved away from this digest, or now. The ledger's git
history already has the replacement time. If it was replaced, only the time it was
live counts. Small change in `policy.checkDeployed`, plus `ledger.PinIntervals`.

### 2.4 Approvals

- **Cloud Deploy:** approval is a per-target switch.
- **Octopus:** lifecycles and manual interventions per phase.
- **Kargo:** relies on Kubernetes RBAC per Stage, plus "approve Freight for stage".
- **ship:** one global `ledgerMinApprovals`, plus GitLab approval policies (which
  can't distinguish stages). CODEOWNERS sections on `ledger/*/PROD.yaml` can.

**P7.** `stages[].approvals: {min: 2, groups: [prod-approvers]}`.

- V8 checks it per changed pin, and the strictest pin in an MR wins.
- `docs/setup.md` gets the matching CODEOWNERS section, so GitLab enforces it
  natively and V8 is the cross-check.
- DEV can go down to 0 for agents, if the bank's policy allows.

### 2.5 Promotion windows / change freeze

- **Kargo:** promotion windows. **Cloud Deploy:** deploy policies (time windows).
  **Spinnaker:** restricted execution windows.
- Banks have change freezes (quarter-end, holidays) and approved change windows for
  PROD.
- **ship:** nothing.

**P2.**

```yaml
stages:
  - name: PROD
    windows:
      timeZone: Europe/Zurich
      allow: ["Mon-Thu 08:00-16:00"]
      deny:  [{ from: 2026-12-20, to: 2027-01-05, reason: "year-end freeze" }]
```

- Checked in `ship validate` (fails fast; the MR shows when it can go) and again in
  `ship deploy`. That second check is the one that matters, because an MR approved
  inside the window can be merged outside it.
- A denied deploy is a clean refusal: the pin stays, and the next pipeline run
  inside the window deploys it.
- The emergency path (P8) is the only override.

### 2.6 Re-validation at deploy time (TOCTOU)

- **Cloud Deploy:** snapshots the pipeline config into each release and warns on
  "pipeline mismatch" before promotion or rollback.
- **ship:** `ship validate` runs on the MR. `ship deploy` re-checks only V8. Between
  approval and merge (hours, sometimes days):
  - evidence can be deleted,
  - the upstream pin can be reverted,
  - `ship.yaml` can tighten rules,
  - a freeze can start.

**P1.** `ship deploy` re-runs `Checker.Validate(pre-merge main, merged main)` with
the policy at pre-merge main (same rule as today: policy never from the change
itself) before any driver runs. A violation means refusal, reported on the ledger
MR and the Promotion CR, exactly like the V8 refusal. ~30 lines; it reuses
everything. **The most important gap found.**

### 2.7 Rollback

- **Cloud Deploy:** rollback is a new rollout of the target's last successful
  release, and can be automated via `repairRolloutRule`.
- **Kargo:** promote older Freight. If auto-promotion is on, that places a *hold*.
- **ship:** a rollback is a normal promotion of an older digest. It only passes if
  V5 still holds, which it usually does (the old Freight was deployed upstream long
  ago). But it's accidental, and soak/windows would block an urgent rollback.

**P3.**

- V5 accepts a digest that has `deployed/<this stage>` evidence. It already ran
  here, so ordered progression is not weakened.
- New `ship rollback --app X --stage PROD [--to <digest>]` defaults to the
  previous pin that has deployed evidence in this stage, and opens a normal MR with
  a `Rollback: true` trailer.
- Approval (V8) still applies. Whether windows apply to rollbacks is a policy
  choice; default yes, overridable via P8.

### 2.8 Emergency / hotfix path

- **Kargo:** `kargo approve --freight f --stage prod` makes Freight eligible
  without upstream verification or soak (same permission as promoting).
- **Octopus:** channels let a hotfix release stream skip lifecycle phases.
- Banks call this an *emergency change*: allowed, but with stronger approval and
  mandatory follow-up.

**P8.**

- An `Emergency: <ticket>` trailer is honored only if `ship.yaml` has
  `emergency: {skip: [V5, soak, windows], approvals: {min: 2, groups: [break-glass]}}`.
- It never skips V1–V4, V6 (unless listed), V7 or V8.
- Recorded as `emergency/<stage>` evidence, flagged in `status` and `history`, and
  optionally opens a follow-up issue.
- The GitLab side needs a CODEOWNERS or approval-policy rule for the break-glass
  group.

### 2.9 Visibility: why can't this Freight go further?

- **Kargo's UI:** per Freight, shows the stages it's verified in or approved for,
  and what each Stage is waiting on. People understand the pipeline by looking at
  it.
- **ship today:** answers "what is pinned where" (`status`) and "what changed"
  (`history`), but not "can Freight X go to PROD, and if not, why".

**P9.** `ship freight <digest|version>`:

```
SV-API_GW_MGT 1.4.2 (sha256:4f1c…)
  DEV   deployed 2026-11-20 10:02   verified ✔
  TEST  deployed 2026-11-21 09:15   verified ✔   soak 14h/24h
  PROD  not eligible: V5 soak 14h of 24h in TEST (eligible after 2026-11-22 09:15)
        would also need: integration-test evidence (V6)
```

It's `CheckPin` run against every stage with the reasons printed. Mostly a
presentation layer, but the most useful thing for people new to the system.

**P10.** `ship status` adds a marker on pins that have no `deployed/<stage>` evidence
(the failed/refused-deploy drift from design §11 Q7), and optionally on pins whose
env MR is still open.

### 2.10 Evidence and supply chain

- None of the promotion tools solve evidence trust themselves. They rely on
  whatever the CI/analysis reports.
- The industry standard is in-toto attestations (SLSA provenance, test results,
  VSA), signed with Sigstore/cosign, stored as OCI referrers. Exactly where ship
  already puts evidence.

**P11.** Switch the evidence format to in-toto statements, signed (keyless with
GitLab OIDC, or a bank KMS key). ship.yaml declares which signer identities count
per evidence kind, e.g. `deployed/*` only from the ledger's `ship-deploy` job.
This closes "anyone with registry write can fake evidence". Needs its own design
(identity claims, key management, offline verification).

## 3. Where ship is already stronger

| Topic | ship | Others |
|---|---|---|
| Who decides | GitLab compliance framework; policies owned by a separate project | Kargo: its own controller and RBAC; Cloud Deploy: GCP IAM |
| Audit trail | git history + MR approvals + GitLab audit events | CR status (Kargo, garbage-collected), proprietary DB (Octopus) |
| Requester ≠ approver | V8 pre- and post-merge, identity mapping, service accounts excluded | Kargo: RBAC only; Cloud Deploy: IAM role |
| Artifact identity | the OCI digest itself, artifact-agnostic | Kargo: derived hash over images/commits/charts |
| Deployment targets | driver interface: Crossplane MR, Octopus, exec, plugins | Kargo: Argo CD-centric steps; GitOps Promoter: Argo CD hydrator |
| Env-repo provenance | every bot MR re-derived from the ledger before approval | Telefonistka/Promoter: trust the bot |

## 4. Deliberately not adopted

- **Auto-promotion engine** (Kargo `autoPromotionOptions`, Cloud Deploy automation
  rules). It conflicts with "people or agents construct promotions". An agent can
  already open MRs; per-stage approval rules (P7) make low-risk stages cheap. If
  wanted later, a small "propose newest eligible Freight" bot that opens normal MRs
  fits without changing the model.
- **Promotion-step DSL** (Kargo `promotionTemplate.steps`). It makes the deploy
  procedure code inside the promotion tool. ship keeps the procedure in drivers plus
  GitLab CI owned by policy, which is what the compliance framework can govern.
- **Environment branches** (GitOps Promoter). ship pins digests in files on one
  branch. Branches per environment make history and review harder and are widely
  discouraged.
- **Argo CD dependency.** The bank uses Octopus and Crossplane; health comes from
  drivers' Wait.
- **CRDs as the source of truth** (Kargo). ship's Promotion CR is a front-end; the
  ledger stays authoritative. Stage/Freight CRDs (design §12.9) would only mirror
  state if ever built.

## 5. Open questions for review

1. Which proposals to take, and in what order? (Recommendation in §0.)
2. P2: are change windows defined somewhere already (ServiceNow change calendar)?
   If so, ship should read that rather than duplicate it in ship.yaml.
3. P8: does the bank's emergency-change process require a ticket reference format
   that ship should validate?
4. P4: what would verification run in practice: smoke tests as a container, or
   metric checks against the monitoring system?
5. P7: should DEV ever allow zero human approvals (agents promoting freely)?

## Sources

- Kargo: [Working with Stages](https://docs.kargo.io/user-guide/how-to-guides/working-with-stages),
  [Working with Freight](https://docs.kargo.io/user-guide/how-to-guides/working-with-freight),
  [Verification](https://docs.kargo.io/user-guide/how-to-guides/verification)
- [GitOps Promoter](https://gitops-promoter.readthedocs.io/en/latest/) and its
  [tool comparison](https://gitops-promoter.readthedocs.io/en/latest/tool-comparison/)
- [Telefonistka](https://github.com/commercetools/telefonistka)
- Google Cloud Deploy: [terminology](https://docs.cloud.google.com/deploy/docs/terminology),
  [architecture](https://docs.cloud.google.com/deploy/docs/architecture),
  [automation rules](https://docs.cloud.google.com/deploy/docs/automation-rules)
- Octopus lifecycles/channels and Spinnaker execution windows: from general product
  knowledge, not re-checked for this document.

## 6. Decisions from review (2026-11-22)

**Most checks live in external systems** (change calendar, change tickets, …).
That reshapes P2: instead of `windows:` in ship.yaml, ship gets a **gate**
contract, the same idea as drivers.

- `stages[].gates: [{name: servicenow-change, config: {...}}]`; built-in or an external
  `ship-gate-<name>` speaking JSON over stdin/stdout.
- Input: app, stage, Freight (digest, version, annotations), the MR (requester,
  trailers such as `Change:`), the phase (`validate` or `deploy`).
- Output: `pass` / `fail` / `pending`, with a reason, and optionally an "eligible after" time.
- Run in `ship validate` (fail fast, shown in the MR) **and** in `ship deploy` (P1:
  the decision that counts is at deploy time, because the window or ticket state may
  have changed since approval). Fail closed: an unreachable system is a failure.
- The verdict is recorded as evidence (`gate/<name>/<stage>`) and in the MR note,
  so the audit trail shows which external decision allowed the deploy.

**Change tickets (P8 and normal changes).** What "check the ticket" can mean, from
weakest to strongest:

1. Format only: `Change: CHG0012345` matches `^CHG\d{7}$`. Cheap, proves nothing.
2. Exists, via the ServiceNow API.
3. Is in the right state: approved / "Implement", not draft, not closed.
4. Is the right type: `emergency` for the emergency path, `standard`/`normal` otherwise.
5. Is about this change: the ticket's configuration item (CI) maps to this app, and
   ideally the ticket mentions this Freight's version or the ledger MR URL.
6. Is valid now: the current time is inside the ticket's planned start/end.

Recommendation: a `servicenow-change` gate doing 2–6, required for PROD (all
changes) and for every emergency promotion. The emergency path (P8) then means
"skip V5/soak, but require an approved emergency ticket plus break-glass approval".
That's what makes it auditable, not the trailer itself. The app→CI mapping goes in
ship.yaml (`apps.<app>.vars.cmdbCI`) or is looked up in the CMDB.

**Verification (P4): what Kargo does.** After a successful promotion the Stage
enters `Verifying` and Kargo creates an Argo Rollouts `AnalysisRun` from the Stage's
`AnalysisTemplate`s. An AnalysisTemplate holds metrics, each with a provider:

- **Job:** runs a Kubernetes Job (smoke tests, end-to-end checks); success = exit 0.
- **Web:** calls an HTTP endpoint and evaluates a JSON field.
- **Metric providers:** Prometheus, Datadog, New Relic, CloudWatch, etc., with
  success/failure conditions, intervals and counts ("error rate < 1% for 10 minutes").

With no templates, Argo CD health counts as verified. So the answer is **both**,
and the template decides. For ship, verification is a **post-deploy gate** with the
same contract: an exec gate for a smoke-test container in the ledger pipeline, an
HTTP gate, or a monitoring gate (Dynatrace/Prometheus/Splunk, whatever the bank
runs). Success records `verified/<stage>`, which V5 requires for the next stage.

**DEV may have zero human approvals.** P7: `stages[].approvals.min: 0` is allowed,
so an agent can promote to DEV on its own. All other rules (V1–V7, gates) still
apply. GitLab side: the approval policy needs a matching exception for
`ledger/*/DEV.yaml` (CODEOWNERS sections or an approval rule scoped to the other
stages). Note: GitLab approval policies apply per MR, so a DEV-only MR with 0
approvals must not be mixable with a PROD pin. V7 already requires pin-only MRs;
P7 makes the strictest stage in the MR decide.

**Revised order:** P1 → P3, P9, P10 → **gate contract + ServiceNow gate (P2)** →
P7 → P4 (as a post-deploy gate) → P8 (on top of the ServiceNow gate) → P5, P6 → P11.

## 7. Kosli (2026-11-22)

The bank runs Kosli as its evidence and runtime-compliance backend. Design:
[kosli.md](kosli.md). Effect on the proposals:

- **V6 / required evidence:** asserted against Kosli policies, owned by compliance.
- **P5 soak and V5 "deployed upstream":** from Kosli environment snapshots (what is
  *observed* running and for how long) instead of ship's self-reported evidence.
- **P10 drift:** pinned (ledger) vs running (Kosli) in `ship status`.
- **P11 signed evidence:** mostly replaced by Kosli service-account identity
  (`reported_by`); signing only where Kosli verifies it (in-toto / SBOM).
- **Gates:** Kosli assert is the first built-in gate; ServiceNow too, unless the
  ticket is already attested into Kosli.
- **New:** ship reports each promotion (approvals, V8, ledger MR) into Kosli, so
  a Kosli policy can flag production artifacts that bypassed ship.
