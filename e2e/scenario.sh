#!/usr/bin/env bash
# End-to-end scenario (docs/design.md §10). Runs inside the compose network
# (see run-scenario.sh) with tokens from .state.env.
set -euo pipefail

API=http://gitlab:8929/api/v4
REG=registry:5001
LEDGER=platform/ledger
GW=SV-API_GW_MGT
GW_REPO=$REG/deploy/sv-api-gw-mgt
export SHIP_INSECURE_REGISTRIES=$REG
PASS=0
RUN=$(date +%s)
export KUBECONFIG=/e2e/.kube/config
NS=team-api-gw

log()  { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m  ✔ %s\033[0m\n' "$*"; PASS=$((PASS+1)); }
die()  { printf '\033[1;31m  ✘ %s\033[0m\n' "$*"; exit 1; }
enc()  { jq -rn --arg s "$1" '$s|@uri'; }
api()  { # api TOKEN METHOD PATH [JSON]
  local t=$1 m=$2 p=$3 d=${4:-}
  if [[ -n $d ]]; then curl -fsS -X "$m" -H "PRIVATE-TOKEN: $t" -H 'Content-Type: application/json' -d "$d" "$API$p"
  else curl -fsS -X "$m" -H "PRIVATE-TOKEN: $t" "$API$p"; fi
}

# ---------- helpers ----------
wait_mr_pipeline() { # project iid -> prints final status
  local p; p=$(enc "$1")
  for _ in $(seq 1 120); do
    local s; s=$(api "$ROOT_TOKEN" GET "/projects/$p/merge_requests/$2" | jq -r '.head_pipeline.status // "none"')
    case $s in success|failed|canceled) echo "$s"; return;; esac
    sleep 3
  done
  echo timeout
}
wait_sha_pipeline() { # project sha -> final status of the newest pipeline for sha
  local p; p=$(enc "$1")
  for _ in $(seq 1 300); do
    local s; s=$(api "$ROOT_TOKEN" GET "/projects/$p/pipelines?sha=$2&order_by=id&sort=desc" | jq -r '.[0].status // "none"')
    case $s in success|failed|canceled) echo "$s"; return;; esac
    sleep 3
  done
  echo timeout
}
job_log() { # project pipeline_status_sha job_name
  local p jid; p=$(enc "$1")
  jid=$(api "$ROOT_TOKEN" GET "/projects/$p/pipelines?sha=$2&order_by=id&sort=desc" | jq -r '.[0].id')
  jid=$(api "$ROOT_TOKEN" GET "/projects/$p/pipelines/$jid/jobs" | jq -r --arg n "$3" '[.[]|select(.name==$n)][0].id')
  api "$ROOT_TOKEN" GET "/projects/$p/jobs/$jid/trace"
}
mr_by_branch() { api "$ROOT_TOKEN" GET "/projects/$(enc "$1")/merge_requests?source_branch=$(enc "$2")&order_by=created_at&sort=desc" | jq -r '.[0].iid // empty'; }
claim() { api "$ROOT_TOKEN" GET "/projects/$(enc "$1")/repository/files/$(enc "$2")/raw?ref=main"; }

WORK=$(mktemp -d)
git config --global user.name promoter
git config --global user.email promoter@ship.example.org
git config --global init.defaultBranch main
git clone -q "http://promoter:$PROMOTER_TOKEN@gitlab:8929/$LEDGER.git" "$WORK/ledger"
cd "$WORK/ledger"
sync() { git checkout -q main && git pull -q --ff-only; }

# promote via the CLI; expects acceptance by CI, approval and deploy.
promote_ok() { # app stage freight
  sync
  SHIP_REQUESTED_BY="agent:e2e" ship promote --app "$1" --stage "$2" --freight "$3" --reason "e2e $1 to $2" --push >/dev/null
  local short=${3##*sha256:}; short=${short:0:12}
  local br="promote/$1/$2/$short" iid
  iid=$(mr_by_branch "$LEDGER" "$br"); [[ -n $iid ]] || die "no MR for $br"
  [[ $(wait_mr_pipeline "$LEDGER" "$iid") == success ]] || die "validate failed for $br"
  ok "ledger MR !$iid ($1 → $2): validate + plan passed"
  api "$REVIEWER_TOKEN" POST "/projects/$(enc $LEDGER)/merge_requests/$iid/approve" >/dev/null
  local merge; merge=$(api "$REVIEWER_TOKEN" PUT "/projects/$(enc $LEDGER)/merge_requests/$iid/merge" '{"should_remove_source_branch":true}' | jq -r '.merge_commit_sha')
  ok "approved and merged by reviewer ($merge)"
  LAST_MERGE=$merge
  local st; st=$(wait_sha_pipeline "$LEDGER" "$merge")
  if [[ $st != success ]]; then job_log "$LEDGER" "$merge" ship-deploy | tail -30; die "deploy pipeline: $st"; fi
  ok "deploy pipeline succeeded"
}

# push a pin commit directly (bypassing the CLI pre-check, like a rogue client) and expect CI to reject it.
propose_raw_expect_fail() { # name rule files...
  local name=$1 rule=$2; shift 2
  sync
  local br="raw/$name-$RUN"
  git checkout -q -b "$br"
  for kv in "$@"; do mkdir -p "$(dirname "${kv%%=*}")"; printf '%s' "${kv#*=}" > "${kv%%=*}"; git add "${kv%%=*}"; done
  git commit -q -m "raw: $name"
  git push -q -o merge_request.create -o merge_request.title="raw: $name" origin "$br" 2>/dev/null
  git checkout -q main
  local iid; iid=$(mr_by_branch "$LEDGER" "$br")
  [[ $(wait_mr_pipeline "$LEDGER" "$iid") == failed ]] || die "$name: expected validate to fail"
  local sha; sha=$(api "$ROOT_TOKEN" GET "/projects/$(enc $LEDGER)/merge_requests/$iid" | jq -r .sha)
  local logtxt; logtxt=$(job_log "$LEDGER" "$sha" ship-validate)
  [[ $logtxt == *"$rule"* ]] || die "$name: expected $rule in validate log"
  api "$ROOT_TOKEN" PUT "/projects/$(enc $LEDGER)/merge_requests/$iid" '{"state_event":"close"}' >/dev/null
  ok "$name: rejected by CI with $rule (MR !$iid closed)"
}

# ================================================================
log "1. build pipeline publishes Freight A (1.0.0)"
A=$(pushfreight --repo "$GW_REPO" --version 1.0.0)
ok "A = $A"

log "2. promote A → DEV (crossplane driver, env MR, ship-approver)"
promote_ok "$GW" DEV "$A"
[[ $(claim "sda/services/$GW/DEV" claim.yaml) == *"$A"* ]] || die "DEV claim not updated"
ok "sda/services/$GW/DEV claim.yaml now references A"
ENV_MR=$(mr_by_branch "sda/services/$GW/DEV" "ship/$GW/DEV/${A:${#A}-64:12}")
by=$(api "$ROOT_TOKEN" GET "/projects/$(enc "sda/services/$GW/DEV")/merge_requests/$ENV_MR/approvals" | jq -r '[.approved_by[].user.username]|join(",")')
[[ $by == ship-approver ]] || die "env MR approvals: $by"
ok "env MR !$ENV_MR authored by ship-bot, approved by ship-approver, merged"

log "3. skipping TEST is rejected"
if ship promote --app "$GW" --stage PROD --freight "$A" >/dev/null 2>&1; then die "CLI pre-check should refuse"; fi
ok "ship promote refuses locally (V5)"
propose_raw_expect_fail skip-test "V5" "ledger/$GW/PROD.yaml=freight: $A"$'\n'

log "4. promote A → TEST"
promote_ok "$GW" TEST "$A"

log "5. PROD without evidence is rejected"
propose_raw_expect_fail no-evidence "V6" "ledger/$GW/PROD.yaml=freight: $A"$'\n'

log "6. CI attaches integration-test evidence; promote A → PROD"
ship attest --freight "$A" --kind integration-test --stage TEST --result pass >/dev/null
ok "evidence attached: $(ship evidence "$A" | jq -r '[.[]|.annotations["ship.dev/kind"]+"/"+.annotations["ship.dev/stage"]]|join(", ")')"
promote_ok "$GW" PROD "$A"
[[ $(claim "sda/services/$GW/PROD" claim.yaml) == *"$A"* ]] || die "PROD claim not updated"
ok "PROD claim references A"
PROD_MERGE=$LAST_MERGE

log "7. retrying the PROD deploy job is idempotent"
p=$(enc $LEDGER)
pid=$(api "$ROOT_TOKEN" GET "/projects/$p/pipelines?sha=$PROD_MERGE" | jq -r '.[0].id')
jid=$(api "$ROOT_TOKEN" GET "/projects/$p/pipelines/$pid/jobs" | jq -r '.[]|select(.name=="ship-deploy").id')
api "$ROOT_TOKEN" POST "/projects/$p/jobs/$jid/retry" >/dev/null
[[ $(wait_sha_pipeline "$LEDGER" "$PROD_MERGE") == success ]] || die "retry failed"
n=$(api "$ROOT_TOKEN" GET "/projects/$(enc "sda/services/$GW/PROD")/merge_requests?state=all&source_branch=$(enc "ship/$GW/PROD/${A:${#A}-64:12}")" | jq length)
[[ $n == 1 ]] || die "expected 1 env MR in PROD, found $n"
ok "retry succeeded, still exactly 1 env MR in PROD"

log "8. a tampered bot MR is not approved; the next promotion supersedes it"
DEVP="sda/services/$GW/DEV"
bad=$(claim "$DEVP" claim.yaml | sed 's/replicas: 2/replicas: 0/')
STALE=ship/$GW/DEV/$(head -c6 /dev/urandom | od -An -tx1 | tr -d ' \n')
api "$BOT_TOKEN" POST "/projects/$(enc "$DEVP")/repository/commits" "$(jq -n --arg b "$STALE" --arg c "$bad" \
  '{branch:$b, start_branch:"main", commit_message:"tamper", actions:[{action:"update", file_path:"claim.yaml", content:$c}]}')" >/dev/null
siid=$(api "$BOT_TOKEN" POST "/projects/$(enc "$DEVP")/merge_requests" "$(jq -n --arg b "$STALE" \
  '{source_branch:$b, target_branch:"main", title:"tampered", labels:"ship", description:"Freight: x"}')" | jq -r .iid)
[[ $(wait_mr_pipeline "$DEVP" "$siid") == failed ]] || die "verify-env-mr should fail the tampered MR"
by=$(api "$ROOT_TOKEN" GET "/projects/$(enc "$DEVP")/merge_requests/$siid/approvals" | jq -r '.approved_by|length')
[[ $by == 0 ]] || die "tampered MR was approved"
ok "tampered bot MR !$siid: verify-env-mr failed, no approval"
B=$(pushfreight --repo "$GW_REPO" --version 1.1.0)
promote_ok "$GW" DEV "$B"
st=$(api "$ROOT_TOKEN" GET "/projects/$(enc "$DEVP")/merge_requests/$siid" | jq -r .state)
[[ $st == closed ]] || die "stale MR state: $st"
ok "promotion of B closed the stale env MR !$siid as superseded"

log "9. a promotion MR may not change policy (V7)"
propose_raw_expect_fail policy-in-promotion "V7" "ledger/$GW/TEST.yaml=freight: $B"$'\n' "ship.yaml=$(cat ship.yaml)"$'\n# weakened\n'

log "10. applications layout: portal → DEV"
P=$(pushfreight --repo "$REG/deploy/portal" --version 0.1.0)
promote_ok portal DEV "$P"
[[ $(claim "sda/applications/CUSTOMER_PORTAL/DEV" apps/portal/claim.yaml) == *"$P"* ]] || die "portal claim not updated"
ok "sda/applications/CUSTOMER_PORTAL/DEV apps/portal/claim.yaml references portal Freight"

# ================================================================ Promotion CRs
kget() { kubectl -n $NS get promotion "$1" -o jsonpath="$2"; }
wait_phase() { # name phase...
  local name=$1; shift
  for _ in $(seq 1 200); do
    local ph; ph=$(kget "$name" '{.status.phase}')
    for want in "$@"; do [[ $ph == "$want" ]] && { echo "$ph"; return; }; done
    case $ph in Succeeded|Failed|Rejected|Abandoned|Superseded) echo "$ph"; return;; esac
    sleep 3
  done
  echo timeout
}
wait_cond() { # name type status
  for _ in $(seq 1 60); do
    [[ $(kget "$1" "{.status.conditions[?(@.type==\"$2\")].status}") == "$3" ]] && return 0
    sleep 2
  done
  return 1
}
promotion() { # as name stage freight requestedBy [app]
  kubectl --as "$1" -n $NS create -f - <<YAML
apiVersion: ship.dev/v1alpha1
kind: Promotion
metadata: { name: $2 }
spec:
  app: ${6:-$GW}
  stage: $3
  freight: $4
  reason: e2e via Kubernetes
  requestedBy: $5
YAML
}
mr_iid() { kget "$1" '{.status.ledgerMR.iid}'; }

log "11. Promotion CR: promoter promotes C → DEV with kubectl; reviewer approves in GitLab"
C=$(pushfreight --repo "$GW_REPO" --version 1.2.0)
promotion promoter "gw-dev-$RUN" DEV "$C" promoter >/dev/null
[[ $(wait_phase "gw-dev-$RUN" AwaitingApproval) == AwaitingApproval ]] || die "expected AwaitingApproval, got $(kget "gw-dev-$RUN" '{.status}')"
iid=$(mr_iid "gw-dev-$RUN")
m=$(api "$ROOT_TOKEN" GET "/projects/$(enc $LEDGER)/merge_requests/$iid")
[[ $(jq -r .author.username <<<"$m") == ship-promoter && $(jq -r .description <<<"$m") == *"Requested-by: promoter"* ]] || die "ledger MR author/trailer"
ok "controller opened ledger MR !$iid as ship-promoter (Requested-by: promoter); validate passed"
api "$REVIEWER_TOKEN" POST "/projects/$(enc $LEDGER)/merge_requests/$iid/approve" >/dev/null
wait_cond "gw-dev-$RUN" Approved True || die "Approved condition not True"
ok "reviewer approved → Approved=True (V8: independent of requester)"
api "$REVIEWER_TOKEN" PUT "/projects/$(enc $LEDGER)/merge_requests/$iid/merge" '{"should_remove_source_branch":true}' >/dev/null
[[ $(wait_phase "gw-dev-$RUN" Succeeded) == Succeeded ]] || die "expected Succeeded, got $(kget "gw-dev-$RUN" '{.status.phase} {.status.message}')"
[[ $(claim "sda/services/$GW/DEV" claim.yaml) == *"$C"* ]] || die "DEV claim not updated to C"
ok "Promotion Succeeded; env MR $(kget "gw-dev-$RUN" '{.status.targets[0].state}') and approved by $(kget "gw-dev-$RUN" '{.status.targets[0].approvedBy[0]}')"

log "12. admission: requestedBy must be the creating user"
out=$(promotion promoter "spoof-$RUN" DEV "$C" reviewer 2>&1 || true)
[[ $out == *"must be the creating user"* ]] || die "spoofed requestedBy was not denied: $out"
ok "spoofed requestedBy denied"

log "13. admission: namespace may only promote its own apps"
out=$(promotion promoter "portal-$RUN" DEV "$P" promoter portal 2>&1 || true)
[[ $out == *"may not be promoted from namespace"* ]] || die "foreign app was not denied: $out"
ok "portal from team-api-gw denied"

log "14. spec is immutable"
out=$(kubectl -n $NS patch promotion "gw-dev-$RUN" --type merge -p '{"spec":{"stage":"PROD"}}' 2>&1 || true)
[[ $out == *"immutable"* ]] || die "spec change was not denied: $out"
ok "spec update denied"

log "15. V8 both ways: reviewer requests TEST and approves it themselves"
promotion reviewer "gw-test-$RUN" TEST "$C" reviewer >/dev/null
[[ $(wait_phase "gw-test-$RUN" AwaitingApproval) == AwaitingApproval ]] || die "expected AwaitingApproval"
iid=$(mr_iid "gw-test-$RUN")
api "$REVIEWER_TOKEN" POST "/projects/$(enc $LEDGER)/merge_requests/$iid/approve" >/dev/null
wait_cond "gw-test-$RUN" Approved False || die "self-approval should leave Approved=False"
ok "pre-merge: Approved=False ($(kget "gw-test-$RUN" '{.status.conditions[?(@.type=="Approved")].message}'))"
echo "     (on Ultimate the controller's external status check blocks this merge; CE cannot, so merge anyway)"
merge=$(api "$REVIEWER_TOKEN" PUT "/projects/$(enc $LEDGER)/merge_requests/$iid/merge" '{"should_remove_source_branch":true}' | jq -r .merge_commit_sha)
[[ $(wait_phase "gw-test-$RUN" Failed) == Failed ]] || die "expected Failed"
deploylog=$(job_log "$LEDGER" "$merge" ship-deploy)
[[ $deploylog == *"V8: 0 independent approval"* ]] || die "deploy log should show V8 refusal"
[[ $(claim "sda/services/$GW/TEST" claim.yaml) != *"$C"* ]] || die "TEST claim must not change"
ok "post-merge: ship deploy refused (V8), Promotion Failed, TEST env untouched"

log "15b. the refused TEST pin does not make C eligible for PROD (V5 needs deployed/TEST)"
ship attest --freight "$C" --kind integration-test --stage TEST --result pass >/dev/null   # rule out V6
propose_raw_expect_fail drift-to-prod "never successfully deployed there" "ledger/$GW/PROD.yaml=freight: $C"$'\n'

log "16. deleting a Promotion closes its open ledger MR"
D=$(pushfreight --repo "$GW_REPO" --version 1.3.0)
promotion promoter "gw-dev-d-$RUN" DEV "$D" promoter >/dev/null
[[ $(wait_phase "gw-dev-d-$RUN" AwaitingApproval) == AwaitingApproval ]] || die "expected AwaitingApproval"
iid=$(mr_iid "gw-dev-d-$RUN")
kubectl -n $NS delete promotion "gw-dev-d-$RUN" --wait=true >/dev/null
[[ $(api "$ROOT_TOKEN" GET "/projects/$(enc $LEDGER)/merge_requests/$iid" | jq -r .state) == closed ]] || die "MR not closed"
ok "Promotion deleted → ledger MR !$iid closed"

log "kubectl get promotions"
kubectl -n $NS get promotions

log "status"
sync
ship status
echo
ship history --app "$GW" --stage DEV

printf '\n\033[1;32mALL %d CHECKS PASSED\033[0m\n' "$PASS"
