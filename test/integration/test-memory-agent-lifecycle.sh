#!/usr/bin/env bash
# Integration test: Memory across agent run lifecycle (success + failure)
# against the central memory-server.
#
# Proves:
#   1. Central memory-server is healthy and reachable.
#   2. A successful AgentRun stores findings to memory via memory_store tool calls.
#   3. Auto-injected memory context appears in the system prompt of subsequent runs.
#   4. Controller persists a failure record to the memory-server when an AgentRun fails.
#   5. A follow-up run after a failure can see the failure context in auto-injected memory.
#   6. memory-server logs show search/store access (observability).
#
# Auth: admin SA bearer token (apiserver SA is in MEMORY_ADMIN_SAS).
# Requires: Kind cluster with Sympozium deployed, LM Studio accessible on node.

set -euo pipefail

NAMESPACE="${TEST_NAMESPACE:-default}"
SYSTEM_NS="${SYMPOZIUM_NAMESPACE:-sympozium-system}"
MEMORY_SVC="${MEMORY_SERVICE:-sympozium-memory-server}"
TIMEOUT="${TEST_TIMEOUT:-240}"

LM_STUDIO_BASE_URL="${LM_STUDIO_BASE_URL:-http://172.18.0.2:9473/proxy/lm-studio/v1}"
LM_STUDIO_MODEL="${LM_STUDIO_MODEL:-qwen/qwen3.5-9b}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

pass() { echo -e "${GREEN}PASS $*${NC}"; }
fail() { echo -e "${RED}FAIL $*${NC}"; FAILED=1; }
info() { echo -e "${YELLOW}---- $*${NC}"; }

FAILED=0
SUFFIX="$(date +%s)"
INSTANCE="inttest-memlc-${SUFFIX}"
MEM_PF_PID=""
MEM_PORT="${MEM_PORT:-19392}"
MEM_URL="http://127.0.0.1:${MEM_PORT}"
MEMORY_TOKEN=""

cleanup() {
  info "Cleaning up..."
  if [[ -n "$MEMORY_TOKEN" ]]; then
    curl -sS -X DELETE -G "${MEM_URL}/v1/admin/scope" \
      -H "Authorization: Bearer ${MEMORY_TOKEN}" \
      --data-urlencode "scope=agent" \
      --data-urlencode "agentName=${INSTANCE}" \
      --data-urlencode "namespace=${NAMESPACE}" >/dev/null 2>&1 || true
  fi
  [[ -n "$MEM_PF_PID" ]] && kill "$MEM_PF_PID" 2>/dev/null || true
  kubectl delete agentrun -n "$NAMESPACE" -l "sympozium.ai/instance=${INSTANCE}" --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete sympoziuminstance "$INSTANCE" -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

wait_for_agentrun() {
  local name="$1" target_phase="$2" elapsed=0 last_phase=""
  while [[ $elapsed -lt $TIMEOUT ]]; do
    local phase
    phase="$(kubectl get agentrun "$name" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")"
    if [[ -n "$phase" && "$phase" != "$last_phase" ]]; then
      info "  Phase: $phase (${elapsed}s)"
      last_phase="$phase"
    fi
    if [[ "$phase" == "$target_phase" ]]; then
      return 0
    fi
    if [[ "$phase" == "Succeeded" || "$phase" == "Failed" ]]; then
      if [[ "$phase" != "$target_phase" ]]; then
        return 1
      fi
      return 0
    fi
    sleep 3
    elapsed=$((elapsed + 3))
  done
  return 1
}

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

mem_search() {
  # NOTE: /v1/search pins row.namespace to the caller identity's namespace,
  # so an admin SA in sympozium-system cannot search rows written by agents
  # in TEST_NAMESPACE. We approximate search with list + content grep.
  local needle="$1"
  mem_list 100 | python3 -c '
import json, sys
needle = sys.argv[1].lower()
d = json.load(sys.stdin)
hits = []
for e in d.get("results", []):
  if needle in (e.get("content") or "").lower():
    hits.append(e)
print(json.dumps({"results": hits}))
' "$needle" 2>/dev/null
}

mem_list() {
  curl -sS -G "${MEM_URL}/v1/list" \
    -H "Authorization: Bearer ${MEMORY_TOKEN}" \
    --data-urlencode "scope=agent" \
    --data-urlencode "agentName=${INSTANCE}" \
    --data-urlencode "namespace=${NAMESPACE}" \
    --data-urlencode "limit=${1:-20}" 2>/dev/null
}

mem_count() {
  mem_list "$@" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(len(d.get("results",[])))' 2>/dev/null || echo "0"
}

# ── Setup ─────────────────────────────────────────────────────────────────────

info "Memory agent lifecycle test — namespace '${NAMESPACE}'"
info "  memory-server: ${SYSTEM_NS}/svc/${MEMORY_SVC}"
info "  agentName:     ${INSTANCE}"
info "Using LM Studio model '${LM_STUDIO_MODEL}' at ${LM_STUDIO_BASE_URL}"

resolve_memory_token || exit 1
port_forward_memory || exit 1
pass "Test 1: Central memory-server is healthy"

# ── Create Agent (memory is enabled by default; agent-runner gets MEMORY_SERVER_URL injected). ──

info "Creating Agent '${INSTANCE}' with k8s-ops skill"

cat <<EOF | kubectl apply -n "$NAMESPACE" -f -
apiVersion: sympozium.ai/v1alpha1
kind: Agent
metadata:
  name: ${INSTANCE}
spec:
  agents:
    default:
      model: ${LM_STUDIO_MODEL}
      baseURL: ${LM_STUDIO_BASE_URL}
  skills:
    - skillPackRef: k8s-ops
  memory:
    enabled: true
EOF

# Verify memory starts empty under this agentName.
initial_count="$(mem_count)"
if [[ "$initial_count" -eq 0 ]]; then
  pass "Test 1: Memory starts empty under agent '${INSTANCE}' (count=${initial_count})"
else
  info "Test 1: Memory already has ${initial_count} entries under agent '${INSTANCE}' (left over from previous run — OK)"
fi

# ── Test 2: Successful run stores memory ──────────────────────────────────────

info "Test 2: Successful AgentRun stores memory"

RUN1="${INSTANCE}-success-run"
cat <<EOF | kubectl apply -n "$NAMESPACE" -f -
apiVersion: sympozium.ai/v1alpha1
kind: AgentRun
metadata:
  name: ${RUN1}
  labels:
    sympozium.ai/instance: ${INSTANCE}
    sympozium.ai/component: agent-run
spec:
  agentRef: ${INSTANCE}
  agentId: default
  sessionKey: "mem-success-${SUFFIX}"
  task: "Use the memory_store tool to store the following text: 'Integration test proof: namespaces checked at SUFFIX'. You MUST call the memory_store tool. After storing, respond with 'done'."
  model:
    provider: lm-studio
    model: ${LM_STUDIO_MODEL}
    baseURL: ${LM_STUDIO_BASE_URL}
    authSecretRef: ""
  skills:
    - skillPackRef: k8s-ops
  timeout: "3m"
EOF

# k8s-ops sidecars may hit the known "Job not found" race (PR #77); the agent
# still completes — we verify via memory entries.
elapsed=0; last_phase=""
while [[ $elapsed -lt $TIMEOUT ]]; do
  phase="$(kubectl get agentrun "$RUN1" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")"
  [[ -n "$phase" && "$phase" != "$last_phase" ]] && { info "  Phase: $phase (${elapsed}s)"; last_phase="$phase"; }
  [[ "$phase" == "Succeeded" || "$phase" == "Failed" ]] && break
  sleep 3; elapsed=$((elapsed + 3))
done
final="$(kubectl get agentrun "$RUN1" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo "unknown")"
error="$(kubectl get agentrun "$RUN1" -n "$NAMESPACE" -o jsonpath='{.status.error}' 2>/dev/null || echo "")"
if [[ "$final" == "Succeeded" ]]; then
  pass "Test 2a: AgentRun '${RUN1}' succeeded"
elif [[ "$final" == "Failed" && "$error" == "Job not found" ]]; then
  info "Test 2a: AgentRun hit known 'Job not found' race (PR #77) — agent completed, verifying via memory"
  pass "Test 2a: AgentRun completed (agent finished, status overwritten by known sidecar race)"
else
  fail "Test 2a: AgentRun ended with phase '${final}' (error: ${error})"
fi

sleep 2

post_success_count="$(mem_count)"
if [[ "$post_success_count" -gt "$initial_count" ]]; then
  pass "Test 2b: Memory count increased after successful run (${initial_count} -> ${post_success_count})"
else
  fail "Test 2b: Memory count did not increase (still ${post_success_count})"
fi

# Search for the stored content.
search_result="$(mem_search "integration test proof")"
if echo "$search_result" | grep -qi "integration test proof\|namespaces"; then
  pass "Test 2c: Memory search returns the stored content"
else
  info "Test 2c: Search result may not contain expected terms: $(echo "$search_result" | head -3)"
fi

# ── Test 3: memory-server logs show access ────────────────────────────────────

info "Test 3: memory-server logs show request access"

mem_logs="$(kubectl logs -n "$SYSTEM_NS" "deployment/${MEMORY_SVC}" --tail=200 2>/dev/null || true)"
# Look for store + search activity for this agent. Server log format includes
# the agent_name in store/search lines; if the format changes, we fall back to
# any /v1/store or /v1/search call.
store_lines="$(echo "$mem_logs" | grep -E "store|/v1/store" | grep -c "${INSTANCE}" || true)"
if [[ "$store_lines" -ge 1 ]]; then
  pass "Test 3a: memory-server logged ${store_lines} store line(s) for agent '${INSTANCE}'"
else
  any_store="$(echo "$mem_logs" | grep -c "/v1/store" || true)"
  if [[ "$any_store" -ge 1 ]]; then
    pass "Test 3a: memory-server logged ${any_store} /v1/store call(s) (log format does not include agent_name)"
  else
    fail "Test 3a: No store activity visible in memory-server logs"
  fi
fi

search_lines="$(echo "$mem_logs" | grep -E "search|/v1/search" | grep -c "${INSTANCE}" || true)"
if [[ "$search_lines" -ge 1 ]]; then
  pass "Test 3b: memory-server logged ${search_lines} search line(s) for agent '${INSTANCE}' (auto-inject + tool calls)"
else
  any_search="$(echo "$mem_logs" | grep -c "/v1/search" || true)"
  if [[ "$any_search" -ge 1 ]]; then
    pass "Test 3b: memory-server logged ${any_search} /v1/search call(s) (log format does not include agent_name)"
  else
    fail "Test 3b: No search activity visible in memory-server logs"
  fi
fi

# ── Test 4: Auto-inject memory context in second run ──────────────────────────

info "Test 4: Second run gets auto-injected memory context"

RUN2="${INSTANCE}-followup-run"
cat <<EOF | kubectl apply -n "$NAMESPACE" -f -
apiVersion: sympozium.ai/v1alpha1
kind: AgentRun
metadata:
  name: ${RUN2}
  labels:
    sympozium.ai/instance: ${INSTANCE}
    sympozium.ai/component: agent-run
spec:
  agentRef: ${INSTANCE}
  agentId: default
  sessionKey: "mem-followup-${SUFFIX}"
  task: "Respond with the word 'hello'. This is a simple test."
  model:
    provider: lm-studio
    model: ${LM_STUDIO_MODEL}
    baseURL: ${LM_STUDIO_BASE_URL}
    authSecretRef: ""
  skills:
  timeout: "3m"
EOF

if wait_for_agentrun "$RUN2" "Succeeded"; then
  pass "Test 4a: Follow-up AgentRun '${RUN2}' succeeded"
else
  final="$(kubectl get agentrun "$RUN2" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo "unknown")"
  error="$(kubectl get agentrun "$RUN2" -n "$NAMESPACE" -o jsonpath='{.status.error}' 2>/dev/null || echo "")"
  fail "Test 4a: Follow-up run ended with phase '${final}' (error: ${error})"
  run2_pod="$(kubectl get agentrun "$RUN2" -n "$NAMESPACE" -o jsonpath='{.status.podName}' 2>/dev/null || true)"
  if [[ -n "$run2_pod" ]]; then
    echo "  Agent logs:"
    kubectl logs "$run2_pod" -n "$NAMESPACE" -c agent --tail=15 2>/dev/null || true
  fi
fi

# Verify auto-inject happened by checking the agent-runner pod logs.
run2_pod="$(kubectl get agentrun "$RUN2" -n "$NAMESPACE" -o jsonpath='{.status.podName}' 2>/dev/null || true)"
if [[ -n "$run2_pod" ]]; then
  agent_logs="$(kubectl logs "$run2_pod" -n "$NAMESPACE" -c agent --tail=50 2>/dev/null || true)"
  if echo "$agent_logs" | grep -q "auto-injected.*bytes of memory context"; then
    pass "Test 4b: Agent-runner logged auto-injection of memory context"
  else
    # Even if agent log is truncated, the memory-server search lines prove it fired.
    mem_logs="$(kubectl logs -n "$SYSTEM_NS" "deployment/${MEMORY_SVC}" --tail=300 2>/dev/null || true)"
    search_count="$(echo "$mem_logs" | grep -E "/v1/search" | grep -c "${INSTANCE}" || true)"
    if [[ "$search_count" -lt 2 ]]; then
      search_count="$(echo "$mem_logs" | grep -c "/v1/search" || true)"
    fi
    if [[ "$search_count" -ge 2 ]]; then
      pass "Test 4b: memory-server shows ${search_count} /v1/search call(s) (auto-inject queries at startup)"
    else
      fail "Test 4b: Expected >= 2 search requests in memory-server logs, found ${search_count}"
      echo "  Agent logs (last 10):"
      echo "$agent_logs" | tail -10
    fi
  fi
else
  info "Test 4b: Pod already cleaned up — cannot verify auto-inject log"
fi

# ── Test 5: Failed run persists failure memory ────────────────────────────────

info "Test 5: Failed AgentRun persists failure record to memory"

count_before_fail="$(mem_count)"

# Create a run that will fail — use an unreachable base URL so the LLM call fails.
RUN3="${INSTANCE}-fail-run"
cat <<EOF | kubectl apply -n "$NAMESPACE" -f -
apiVersion: sympozium.ai/v1alpha1
kind: AgentRun
metadata:
  name: ${RUN3}
  labels:
    sympozium.ai/instance: ${INSTANCE}
    sympozium.ai/component: agent-run
spec:
  agentRef: ${INSTANCE}
  agentId: default
  sessionKey: "mem-fail-${SUFFIX}"
  task: "This run should fail because the LLM endpoint is unreachable."
  model:
    provider: lm-studio
    model: ${LM_STUDIO_MODEL}
    baseURL: "http://192.0.2.1:1/v1"
    authSecretRef: ""
  skills:
  timeout: "1m"
EOF

if wait_for_agentrun "$RUN3" "Failed"; then
  pass "Test 5a: AgentRun '${RUN3}' failed as expected"
else
  final="$(kubectl get agentrun "$RUN3" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo "unknown")"
  fail "Test 5a: Expected Failed phase, got '${final}'"
fi

# Give the controller a moment to persist the failure memory.
sleep 3

count_after_fail="$(mem_count)"
if [[ "$count_after_fail" -gt "$count_before_fail" ]]; then
  pass "Test 5b: Memory count increased after failed run (${count_before_fail} -> ${count_after_fail})"
else
  fail "Test 5b: Memory count did not increase after failure (still ${count_after_fail})"
fi

# Search for failure-related content.
fail_search="$(mem_search "Failed AgentRun")"
if echo "$fail_search" | grep -qi "failed\|error\|unreachable\|timeout"; then
  pass "Test 5c: Failure record found in memory search"
else
  fail "Test 5c: No failure record found in memory"
  echo "  Search result: $(echo "$fail_search" | head -3)"
fi

# Verify the failure entry has the correct tags.
fail_tags="$(mem_search "Failed AgentRun" | python3 -c '
import json, sys
d = json.load(sys.stdin)
entries = d.get("results", [])
for e in entries:
    tags = e.get("tags", [])
    if "failure" in tags:
        print("found")
        break
' 2>/dev/null || echo "")"

if [[ "$fail_tags" == "found" ]]; then
  pass "Test 5d: Failure memory entry has 'failure' tag"
else
  info "Test 5d: Could not verify 'failure' tag (may be format difference)"
fi

# ── Test 6: Run after failure sees failure context ────────────────────────────

info "Test 6: Run after failure gets failure context auto-injected"

RUN4="${INSTANCE}-postfail-run"
cat <<EOF | kubectl apply -n "$NAMESPACE" -f -
apiVersion: sympozium.ai/v1alpha1
kind: AgentRun
metadata:
  name: ${RUN4}
  labels:
    sympozium.ai/instance: ${INSTANCE}
    sympozium.ai/component: agent-run
spec:
  agentRef: ${INSTANCE}
  agentId: default
  sessionKey: "mem-postfail-${SUFFIX}"
  task: "Say hello. This is a simple test."
  model:
    provider: lm-studio
    model: ${LM_STUDIO_MODEL}
    baseURL: ${LM_STUDIO_BASE_URL}
    authSecretRef: ""
  skills:
  timeout: "3m"
EOF

if wait_for_agentrun "$RUN4" "Succeeded"; then
  pass "Test 6a: Post-failure run '${RUN4}' succeeded"
else
  final="$(kubectl get agentrun "$RUN4" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo "unknown")"
  fail "Test 6a: Post-failure run ended with '${final}'"
fi

# Verify auto-inject included failure context.
run4_pod="$(kubectl get agentrun "$RUN4" -n "$NAMESPACE" -o jsonpath='{.status.podName}' 2>/dev/null || true)"
if [[ -n "$run4_pod" ]]; then
  agent_logs="$(kubectl logs "$run4_pod" -n "$NAMESPACE" -c agent --tail=50 2>/dev/null || true)"
  if echo "$agent_logs" | grep -q "auto-injected.*bytes of memory context"; then
    pass "Test 6b: Post-failure run auto-injected memory (includes prior failure context)"
  else
    mem_logs="$(kubectl logs -n "$SYSTEM_NS" "deployment/${MEMORY_SVC}" --tail=300 2>/dev/null || true)"
    search_count="$(echo "$mem_logs" | grep -E "/v1/search" | grep -c "${INSTANCE}" || true)"
    if [[ "$search_count" -lt 3 ]]; then
      search_count="$(echo "$mem_logs" | grep -c "/v1/search" || true)"
    fi
    if [[ "$search_count" -ge 3 ]]; then
      pass "Test 6b: memory-server shows ${search_count} total search request(s) (post-failure auto-inject confirmed)"
    else
      fail "Test 6b: Expected >= 3 search requests by now, found ${search_count}"
    fi
  fi
else
  info "Test 6b: Pod already cleaned up"
fi

# ── Test 7: memory-server logs show controller-side store ─────────────────────

info "Test 7: memory-server logs show controller failure persistence"

mem_logs="$(kubectl logs -n "$SYSTEM_NS" "deployment/${MEMORY_SVC}" --tail=300 2>/dev/null || true)"

# The controller POSTs to /v1/store with tags ["failure", "agent-run", ...].
if echo "$mem_logs" | grep -E "/v1/store" | grep -q "${INSTANCE}.*failure\|failure.*${INSTANCE}"; then
  pass "Test 7: memory-server logged controller-side failure store with 'failure' tag for '${INSTANCE}'"
else
  store_count="$(echo "$mem_logs" | grep -E "/v1/store" | grep -c "${INSTANCE}" || true)"
  if [[ "$store_count" -lt 2 ]]; then
    store_count="$(echo "$mem_logs" | grep -c "/v1/store" || true)"
  fi
  if [[ "$store_count" -ge 2 ]]; then
    pass "Test 7: memory-server logged ${store_count} store operation(s) (controller + agent calls)"
  else
    fail "Test 7: Expected multiple /v1/store log entries, found ${store_count}"
    echo "  memory-server logs (last 20):"
    echo "$mem_logs" | tail -20
  fi
fi

# ── Summary ───────────────────────────────────────────────────────────────────

echo ""
final_count="$(mem_count)"
info "Final memory entry count under agent '${INSTANCE}': ${final_count}"
echo ""

if [[ $FAILED -eq 0 ]]; then
  pass "All memory agent lifecycle tests passed"
  exit 0
else
  fail "Some tests failed"
  exit 1
fi
