#!/usr/bin/env bash
# Bootstraps the sandbox GitLab: users + tokens, groups, ledger and env repos,
# CI variables, and a docker runner that uses the local ship:dev image.
# Idempotent enough to re-run; writes tokens to .state.env (gitignored).
set -euo pipefail
cd "$(dirname "$0")"

GL=http://localhost:8929
API=$GL/api/v4
STATE=.state.env
NET=ship-e2e

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

# ---------- root token (via rails, once) ----------
if [[ -f $STATE ]]; then source "$STATE"; fi
if [[ -z "${ROOT_TOKEN:-}" ]] || ! curl -fsS -H "PRIVATE-TOKEN: $ROOT_TOKEN" "$API/user" >/dev/null 2>&1; then
  log "creating root API token (gitlab-rails runner, ~1 min)"
  ROOT_TOKEN="glpat-$(openssl rand -hex 16)"
  docker compose exec -T gitlab gitlab-rails runner "
    u = User.find_by_username('root')
    t = u.personal_access_tokens.create!(name: 'ship-e2e-bootstrap', scopes: %w[api admin_mode sudo], expires_at: 30.days.from_now)
    t.set_token('$ROOT_TOKEN'); t.save!
  " >/dev/null
  echo "ROOT_TOKEN=$ROOT_TOKEN" > "$STATE"
fi

api() { # api METHOD PATH [JSON]
  local m=$1 p=$2 d=${3:-}
  if [[ -n $d ]]; then
    curl -fsS -X "$m" -H "PRIVATE-TOKEN: $ROOT_TOKEN" -H 'Content-Type: application/json' -d "$d" "$API$p"
  else
    curl -fsS -X "$m" -H "PRIVATE-TOKEN: $ROOT_TOKEN" "$API$p"
  fi
}
enc() { jq -rn --arg s "$1" '$s|@uri'; }

# Faster CI feedback for the sandbox.
api PUT "/application/settings?auto_devops_enabled=false&signup_enabled=false&allow_local_requests_from_web_hooks_and_services=true" >/dev/null

# ---------- users + tokens ----------
user_id() { api GET "/users?username=$1" | jq -r '.[0].id // empty'; }
ensure_user() {
  local u=$1 id
  id=$(user_id "$u")
  if [[ -z $id ]]; then
    id=$(api POST /users "$(jq -n --arg u "$u" --arg p "$(openssl rand -base64 18)Aa1!" \
      '{username:$u, name:$u, email:($u+"@ship.example.org"), password:$p, skip_confirmation:true}')" | jq -r .id)
  fi
  echo "$id"
}
token_for() { # user var [scopes-json]
  local u=$1 var=$2 scopes=${3:-'["api","write_repository"]'} id
  if [[ -n "${!var:-}" ]] && curl -fsS -H "PRIVATE-TOKEN: ${!var}" "$API/user" >/dev/null 2>&1; then return; fi
  id=$(ensure_user "$u")
  local t
  t=$(api POST "/users/$id/personal_access_tokens" "$(jq -n --arg n "ship-e2e" --argjson s "$scopes" \
      --arg e "$(date -v+30d +%F 2>/dev/null || date -d '+30 days' +%F)" \
      '{name:$n, scopes:$s, expires_at:$e}')" | jq -r .token)
  printf -v "$var" '%s' "$t"
  echo "$var=$t" >> "$STATE"
}
log "users: ship-bot, ship-approver, promoter (person/agent), reviewer (release approver)"
token_for ship-bot BOT_TOKEN
token_for ship-approver APPROVER_TOKEN
token_for promoter PROMOTER_TOKEN
token_for reviewer REVIEWER_TOKEN
token_for ship-promoter SHIPPROMOTER_TOKEN
token_for ship-approver APPROVER_READ_TOKEN '["read_api"]'   # env-repo CI: check only, cannot approve
# Credentials for the Octopus and Kosli fakes (e2e/tools/fakes).
if [[ -z "${OCTOPUS_API_KEY:-}" ]]; then
  OCTOPUS_API_KEY="API-$(openssl rand -hex 12 | tr a-f A-F)"; echo "OCTOPUS_API_KEY=$OCTOPUS_API_KEY" >> "$STATE"
fi
if [[ -z "${KOSLI_TOKEN:-}" ]]; then
  KOSLI_TOKEN=$(openssl rand -hex 24); echo "KOSLI_TOKEN=$KOSLI_TOKEN" >> "$STATE"
fi
if [[ -z "${WEBHOOK_SECRET:-}" ]]; then
  WEBHOOK_SECRET=$(openssl rand -hex 24); echo "WEBHOOK_SECRET=$WEBHOOK_SECRET" >> "$STATE"
fi

# ---------- groups + projects ----------
group_id() { api GET "/groups/$(enc "$1")" 2>/dev/null | jq -r '.id // empty' || true; }
ensure_group() { # full_path
  local full=$1 id parent pid=null
  id=$(group_id "$full")
  [[ -n $id ]] && { echo "$id"; return; }
  parent=${full%/*}
  [[ $parent != "$full" ]] && pid=$(ensure_group "$parent")
  api POST /groups "$(jq -n --arg n "${full##*/}" --argjson p "$pid" '{name:$n, path:$n, parent_id:$p, visibility:"internal"}')" | jq -r .id
}
project_id() { api GET "/projects/$(enc "$1")" 2>/dev/null | jq -r '.id // empty' || true; }
ensure_project() { # full_path
  local full=$1 id ns
  id=$(project_id "$full")
  [[ -n $id ]] && { echo "$id"; return; }
  ns=$(ensure_group "${full%/*}")
  api POST /projects "$(jq -n --arg n "${full##*/}" --argjson ns "$ns" \
    '{name:$n, path:$n, namespace_id:$ns, initialize_with_readme:true, default_branch:"main",
      visibility:"internal", only_allow_merge_if_pipeline_succeeds:true,
      remove_source_branch_after_merge:true, auto_devops_enabled:false}')" | jq -r .id
}
# commit_files PROJECT_ID MESSAGE (path=localfile)...  — creates or updates
commit_files() {
  local pid=$1 msg=$2; shift 2
  local actions='[]' p f action
  for kv in "$@"; do
    p=${kv%%=*}; f=${kv#*=}
    if api GET "/projects/$pid/repository/files/$(enc "$p")?ref=main" >/dev/null 2>&1; then action=update; else action=create; fi
    actions=$(jq --arg a "$action" --arg p "$p" --rawfile c "$f" '. + [{action:$a, file_path:$p, content:$c}]' <<<"$actions")
  done
  api POST "/projects/$pid/repository/commits" "$(jq -n --arg m "$msg" --argjson a "$actions" '{branch:"main", commit_message:$m, actions:$a}')" >/dev/null
}
member() { # kind(groups|projects) id user_id access_level
  api POST "/$1/$2/members" "{\"user_id\":$3,\"access_level\":$4}" >/dev/null 2>&1 \
    || api PUT "/$1/$2/members/$3" "{\"access_level\":$4}" >/dev/null
}

log "ledger: platform/ledger"
LEDGER=$(ensure_project platform/ledger)
if ! api GET "/projects/$LEDGER/repository/files/ship.yaml?ref=main" >/dev/null 2>&1; then
  (cd fixtures/ledger && commit_files "$LEDGER" "Bootstrap ledger" \
    ship.yaml=ship.yaml .gitlab-ci.yml=.gitlab-ci.yml CODEOWNERS=CODEOWNERS \
    mappings/service.yaml=mappings/service.yaml mappings/application.yaml=mappings/application.yaml \
    ledger/.gitkeep=ledger/.gitkeep)
fi

# Keep the ledger policy in sync with the fixture (a policy change, committed directly in the sandbox).
if ! diff -q <(api GET "/projects/$LEDGER/repository/files/ship.yaml/raw?ref=main") fixtures/ledger/ship.yaml >/dev/null; then
  log "updating ledger ship.yaml"
  (cd fixtures/ledger && commit_files "$LEDGER" "Update ship.yaml" ship.yaml=ship.yaml)
fi

tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
seed_env() { # project_path claim_path claim_fixture name
  local pid
  pid=$(ensure_project "$1")
  if ! api GET "/projects/$pid/repository/files/.gitlab-ci.yml?ref=main" >/dev/null 2>&1; then
    sed -e "s/__NAME__/$4/g" -e "s/__NAMESPACE__/$(echo "${1##*/}" | tr 'A-Z' 'a-z')/g" "fixtures/claims/$3" > "$tmp/claim.yaml"
    commit_files "$pid" "Bootstrap env repo" .gitlab-ci.yml=fixtures/env/.gitlab-ci.yml "$2=$tmp/claim.yaml"
  elif ! diff -q <(api GET "/projects/$pid/repository/files/.gitlab-ci.yml/raw?ref=main") fixtures/env/.gitlab-ci.yml >/dev/null; then
    commit_files "$pid" "Update CI" .gitlab-ci.yml=fixtures/env/.gitlab-ci.yml
  fi
  ENV_PIDS+=("$pid")
}
ENV_PIDS=()
for st in DEV TEST PROD; do
  log "env repos: sda/services/SV-API_GW_MGT/$st, sda/applications/CUSTOMER_PORTAL/$st"
  seed_env "sda/services/SV-API_GW_MGT/$st" claim.yaml service.yaml sv-api-gw-mgt
  seed_env "sda/applications/CUSTOMER_PORTAL/$st" apps/portal/claim.yaml application.yaml portal
done

# ---------- permissions (30 dev, 40 maintainer, 20 reporter) ----------
log "memberships"
PLATFORM=$(group_id platform); SDA=$(group_id sda)
member projects "$LEDGER" "$(user_id promoter)" 30      # propose
member projects "$LEDGER" "$(user_id reviewer)" 40      # approve + merge promotions
member groups "$PLATFORM" "$(user_id ship-bot)" 20      # read ledger MRs
member groups "$PLATFORM" "$(user_id ship-approver)" 20 # read ledger for provenance
member groups "$SDA" "$(user_id ship-bot)" 40           # env MRs + auto-merge into protected main
member groups "$SDA" "$(user_id ship-approver)" 30      # approve env MRs
member projects "$LEDGER" "$(user_id ship-promoter)" 30 # ledger MRs for Promotion CRs
member groups "$SDA" "$(user_id ship-promoter)" 20      # observe env MRs for CR status

# ---------- CI variables ----------
setvar() { # kind id key value masked
  api POST "/$1/$2/variables" "$(jq -n --arg k "$3" --arg v "$4" --argjson m "$5" '{key:$k, value:$v, masked:$m}')" >/dev/null 2>&1 \
    || api PUT "/$1/$2/variables/$3" "$(jq -n --arg v "$4" --argjson m "$5" '{value:$v, masked:$m}')" >/dev/null
}
log "CI variables"
setvar projects "$LEDGER" SHIP_GITLAB_TOKEN "$BOT_TOKEN" true
setvar projects "$LEDGER" OCTOPUS_URL http://octopus:8081 false
setvar projects "$LEDGER" OCTOPUS_API_KEY "$OCTOPUS_API_KEY" true
# The approver token is NOT a CI variable: only the approver service holds it.
api DELETE "/groups/$SDA/variables/SHIP_APPROVER_TOKEN" >/dev/null 2>&1 || true
setvar groups "$SDA" SHIP_READ_TOKEN "$APPROVER_READ_TOKEN" true

# ---------- webhooks → approver service ----------
# Production: one group webhook per env group (Premium+). CE: project hooks.
log "env repo webhooks → http://approver:8080/webhook"
HOOK=http://approver:8080/webhook
for pid in "${ENV_PIDS[@]}"; do
  for hid in $(api GET "/projects/$pid/hooks" | jq -r --arg u "$HOOK" '.[] | select(.url==$u) | .id'); do
    api DELETE "/projects/$pid/hooks/$hid" >/dev/null
  done
  api POST "/projects/$pid/hooks" "$(jq -n --arg u "$HOOK" --arg t "$WEBHOOK_SECRET" \
    '{url:$u, token:$t, merge_requests_events:true, push_events:false, enable_ssl_verification:false}')" >/dev/null
done
setvar groups "$SDA" SHIP_LEDGER_PROJECT platform/ledger false

# ---------- runner ----------
if ! docker compose exec -T runner grep -q 'ship-e2e' /etc/gitlab-runner/config.toml 2>/dev/null; then
  log "registering runner"
  RUNNER_TOKEN=$(api POST /user/runners '{"runner_type":"instance_type","run_untagged":true,"description":"ship-e2e"}' | jq -r .token)
  docker compose exec -T runner gitlab-runner register --non-interactive \
    --url http://gitlab:8929 --clone-url http://gitlab:8929 --token "$RUNNER_TOKEN" \
    --name ship-e2e --executor docker --docker-image ship:dev \
    --docker-network-mode "$NET" --docker-pull-policy if-not-present \
    --env SHIP_INSECURE_REGISTRIES=registry:5001 >/dev/null
  docker compose exec -T runner sed -i 's/^concurrent = .*/concurrent = 4/' /etc/gitlab-runner/config.toml
fi

# ---------- management cluster: CRD, admission policy, RBAC, controller ----------
log "k3s: Promotion CRD, admission policy, team namespace"
for _ in $(seq 1 60); do [[ -s .kube/kubeconfig.yaml ]] && break; sleep 2; done
sed 's#https://127.0.0.1:6443#https://k3s:6443#' .kube/kubeconfig.yaml > .kube/config
kc() { docker compose exec -T k3s kubectl "$@"; }
for _ in $(seq 1 60); do kc get --raw /readyz >/dev/null 2>&1 && break; sleep 2; done
kc apply -f - < ../deploy/crd.yaml >/dev/null
kc wait --for=condition=Established crd/promotions.ship.dev --timeout=60s >/dev/null
kc apply -f - < ../deploy/admission-policy.yaml >/dev/null
kc create namespace ship-system --dry-run=client -o yaml | kc apply -f - >/dev/null
kc apply -f - < ../deploy/rbac.yaml >/dev/null
kc apply -f - >/dev/null <<'YAML'
apiVersion: v1
kind: Namespace
metadata:
  name: team-api-gw
  annotations: { ship.dev/apps: SV-API_GW_MGT }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: { name: promoters, namespace: team-api-gw }
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: ship-promoter }
subjects:
  - { apiGroup: rbac.authorization.k8s.io, kind: User, name: promoter }
  - { apiGroup: rbac.authorization.k8s.io, kind: User, name: reviewer }
YAML

log "starting ship-controller, ship-approver and the Octopus/Kosli fakes"
docker compose --profile controller up -d --force-recreate controller approver fakes >/dev/null

log "done. tokens in e2e/$STATE"
