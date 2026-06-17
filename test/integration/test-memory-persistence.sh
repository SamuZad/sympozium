#!/usr/bin/env bash
# Integration test: central memory-server persistence.
#
# Proves that:
#   1. The central sympozium-memory-server in ${SYMPOZIUM_NAMESPACE} is healthy
#   2. Memories stored via the /v1/store API are listable and searchable
#   3. Hybrid (pgvector + tsvector) search returns relevant results
#   4. Memories survive a memory-server pod restart (PostgreSQL PVC persistence)
#
# Auth: uses an admin SA bearer token (kubectl create token sympozium-apiserver -n
# sympozium-system); the apiserver SA is in MEMORY_ADMIN_SAS so it bypasses
# membership and scope filters and may store/list/delete under any agentName.
#
# Does NOT require an LLM provider.

set -euo pipefail

NAMESPACE="${TEST_NAMESPACE:-default}"
SYSTEM_NS="${SYMPOZIUM_NAMESPACE:-sympozium-system}"
MEMORY_SVC="${MEMORY_SERVICE:-sympozium-memory-server}"
TIMEOUT="${TEST_TIMEOUT:-120}"
# Memory rows live under whichever namespace identifies the writer. When we
# write directly via the admin SA (sympozium-apiserver), the server pins
# row.namespace to the SA's namespace (SYSTEM_NS). We must therefore also
# read from SYSTEM_NS, not TEST_NAMESPACE.
MEM_NS="$SYSTEM_NS"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

pass() { echo -e "${GREEN}✓ $*${NC}"; }
fail() { echo -e "${RED}✗ $*${NC}"; EXIT_CODE=1; }
info() { echo -e "${YELLOW}● $*${NC}"; }

EXIT_CODE=0
SUFFIX="$(date +%s)"
# Synthetic agentName under which all test memories are stored. We don't need a
# real Agent CR — the agent_name column is just a scope tag from the admin SA's
# perspective. cleanup() wipes everything under this tag at the end.
AGENT_NAME="inttest-mempersist-${SUFFIX}"
MEM_PF_PID=""
MEM_PORT="${MEM_PORT:-19292}"
MEM_URL="http://127.0.0.1:${MEM_PORT}"
MEMORY_TOKEN=""

cleanup() {
  info "Cleaning up memory persistence test resources..."
  if [[ -n "$MEMORY_TOKEN" ]]; then
    curl -sS -X DELETE -G "${MEM_URL}/v1/admin/scope" \
      -H "Authorization: Bearer ${MEMORY_TOKEN}" \
      --data-urlencode "scope=agent" \
      --data-urlencode "agentName=${AGENT_NAME}" \
      --data-urlencode "namespace=${MEM_NS}" >/dev/null 2>&1 || true
  fi
  [[ -n "$MEM_PF_PID" ]] && kill "$MEM_PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

resolve_memory_token() {
  MEMORY_TOKEN="$(kubectl create token sympozium-apiserver -n "$SYSTEM_NS" --duration=1h 2>/dev/null || true)"
  if [[ -z "$MEMORY_TOKEN" ]]; then
    fail "Could not obtain admin SA token (kubectl create token sympozium-apiserver -n $SYSTEM_NS)"
    return 1
  fi
}

port_forward_memory() {
  [[ -n "$MEM_PF_PID" ]] && kill "$MEM_PF_PID" 2>/dev/null || true
  kubectl port-forward -n "$SYSTEM_NS" "svc/${MEMORY_SVC}" "${MEM_PORT}:8080" &>/dev/null &
  MEM_PF_PID=$!
  local elapsed=0
  while [[ "$elapsed" -lt 15 ]]; do
    curl -fsS "${MEM_URL}/healthz" >/dev/null 2>&1 && return 0
    sleep 1
    elapsed=$((elapsed + 1))
  done
  fail "Port-forward to memory server timed out"
  return 1
}

mem_store() {
  local content="$1" tags_json="${2:-[]}"
  curl -sS -X POST "${MEM_URL}/v1/store" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${MEMORY_TOKEN}" \
    -d "{\"scope\":\"agent\",\"agentName\":\"${AGENT_NAME}\",\"content\":${content},\"tags\":${tags_json}}" 2>/dev/null
}

mem_search() {
  curl -sS -X POST "${MEM_URL}/v1/search" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${MEMORY_TOKEN}" \
    -d "{\"scope\":\"agent\",\"agentName\":\"${AGENT_NAME}\",\"query\":\"$1\",\"topK\":${2:-5}}" 2>/dev/null
}

mem_list() {
  curl -sS -G "${MEM_URL}/v1/list" \
    -H "Authorization: Bearer ${MEMORY_TOKEN}" \
    --data-urlencode "scope=agent" \
    --data-urlencode "agentName=${AGENT_NAME}" \
    --data-urlencode "namespace=${MEM_NS}" \
    --data-urlencode "limit=${1:-50}" 2>/dev/null
}

mem_count() {
  mem_list "$@" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(len(d.get("results",[])))' 2>/dev/null || echo "0"
}

# ── Setup ─────────────────────────────────────────────────────────────────────

info "Running central memory-server persistence test"
info "  memory-server: ${SYSTEM_NS}/svc/${MEMORY_SVC}"
info "  agentName (synthetic): ${AGENT_NAME}"

resolve_memory_token || exit 1
port_forward_memory || exit 1

# ── Test 1: Memory server is healthy ─────────────────────────────────────────

info "Test 1: Memory server health check"

if curl -fsS "${MEM_URL}/healthz" >/dev/null 2>&1; then
  pass "Test 1: /healthz responds OK"
else
  fail "Test 1: /healthz did not respond"
  exit 1
fi

if curl -fsS "${MEM_URL}/readyz" >/dev/null 2>&1; then
  pass "Test 1: /readyz responds OK (Postgres reachable)"
else
  fail "Test 1: /readyz did not respond (Postgres may be down)"
fi

# ── Test 2: Store and list memories ──────────────────────────────────────────

info "Test 2: Store and list memories"

initial_count="$(mem_count)"
info "  Initial entry count under agent '${AGENT_NAME}': ${initial_count}"

store1="$(mem_store '"The production database is PostgreSQL 15 running on db-prod-01."' '["infrastructure","database"]')"
if echo "$store1" | grep -qi '"id"'; then
  pass "Test 2a: Memory 1 stored"
else
  fail "Test 2a: Failed to store memory 1 (got: ${store1})"
fi

store2="$(mem_store '"Alert escalation: page oncall if P1 latency exceeds 500ms for 5 minutes."' '["runbook","alerting"]')"
if echo "$store2" | grep -qi '"id"'; then
  pass "Test 2b: Memory 2 stored"
else
  fail "Test 2b: Failed to store memory 2 (got: ${store2})"
fi

mem_store '"Deploy cadence: releases happen every Tuesday at 10am UTC."' '["process","releases"]' >/dev/null

post_count="$(mem_count)"
if [[ "$post_count" -ge $((initial_count + 3)) ]]; then
  pass "Test 2c: List shows ${post_count} entries (initial ${initial_count} + 3 new)"
else
  fail "Test 2c: List shows ${post_count} entries (expected >= $((initial_count + 3)))"
  mem_list | head -10
fi

# ── Test 3: Hybrid search returns relevant results ───────────────────────────

info "Test 3: Hybrid (pgvector + tsvector) search"

search_resp="$(mem_search "database postgresql")"
if echo "$search_resp" | grep -q "PostgreSQL"; then
  pass "Test 3a: Search for 'database postgresql' found the infrastructure memory"
else
  fail "Test 3a: Search did not find 'PostgreSQL' (got: ${search_resp:0:200})"
fi

search_resp2="$(mem_search "alert oncall escalation")"
if echo "$search_resp2" | grep -q "oncall"; then
  pass "Test 3b: Search for 'alert oncall escalation' found the runbook memory"
else
  fail "Test 3b: Search did not find 'oncall' (got: ${search_resp2:0:200})"
fi

# ── Test 4: Memories persist after memory-server pod restart ─────────────────

info "Test 4: Memories persist after memory-server pod restart"

# Stop our port-forward; restart the central deployment; re-forward.
kill "$MEM_PF_PID" 2>/dev/null || true; MEM_PF_PID=""

kubectl rollout restart deployment "${MEMORY_SVC}" -n "$SYSTEM_NS" >/dev/null 2>&1
kubectl rollout status deployment "${MEMORY_SVC}" -n "$SYSTEM_NS" --timeout=90s >/dev/null 2>&1 || {
  fail "Test 4: memory-server rollout did not become ready"
  exit 1
}

port_forward_memory || exit 1

search_after_restart="$(mem_search "database postgresql")"
if echo "$search_after_restart" | grep -q "PostgreSQL"; then
  pass "Test 4: Memories survived memory-server pod restart (Postgres PVC verified)"
else
  fail "Test 4: Memories lost after restart (got: ${search_after_restart:0:200})"
fi

# ── Summary ──────────────────────────────────────────────────────────────────

echo ""
if [[ "$EXIT_CODE" -eq 0 ]]; then
  pass "All central memory-server persistence tests passed"
else
  fail "Some memory persistence tests failed"
fi
exit "$EXIT_CODE"
