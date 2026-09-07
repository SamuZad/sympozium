#!/usr/bin/env bash
# Integration test: verify the Claude Code harness runs an AgentRun end-to-end.
#
# What it does:
#   1. Creates a test Agent with `harness: claude-code` + an AgentRun
#   2. The task tells Claude Code to write a specific file in the workspace
#   3. Waits for the AgentRun to complete (Succeeded or Failed)
#   4. Checks pod logs for the harness shim + Claude Code stream-json evidence
#   5. Checks the AgentRun status.result for the expected content
#   6. Cleans up test resources
#
# Prerequisites:
#   - Kind cluster running with Sympozium installed
#   - The harness-claude-code image loaded into the cluster:
#       make docker-build-harness-claude-code TAG=<tag> && kind load docker-image ...
#   - An Anthropic API key in the environment (ANTHROPIC_API_KEY) or an existing
#     secret named "inttest-anthropic-key" in the default namespace
#
# Usage:
#   ANTHROPIC_API_KEY=sk-ant-... ./test/integration/test-claude-code-harness.sh
#   TEST_MODEL=claude-sonnet-5 ./test/integration/test-claude-code-harness.sh
#   TEST_TIMEOUT=300 ./test/integration/test-claude-code-harness.sh

set -euo pipefail

# --- Configuration ---
NAMESPACE="${TEST_NAMESPACE:-default}"
INSTANCE_NAME="inttest-claude-code"
RUN_NAME="inttest-claude-code-run"
SECRET_NAME="inttest-anthropic-key"
MODEL="${TEST_MODEL:-claude-sonnet-5}"
TIMEOUT="${TEST_TIMEOUT:-240}"             # seconds to wait for completion
MARKER_TEXT="sympozium-claude-code-ok"     # text the agent must write
TARGET_FILE="/workspace/claude-code-test.txt"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

pass() { echo -e "${GREEN}✓ $*${NC}"; }
fail() { echo -e "${RED}✗ $*${NC}"; }
info() { echo -e "${YELLOW}● $*${NC}"; }

cleanup() {
    info "Cleaning up test resources..."
    kubectl delete agentrun "$RUN_NAME" -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete agent "$INSTANCE_NAME" -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete jobs -n "$NAMESPACE" -l "sympozium.ai/agentrun=$RUN_NAME" --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete pods -n "$NAMESPACE" -l "sympozium.ai/agentrun=$RUN_NAME" --ignore-not-found >/dev/null 2>&1 || true
}

# --- Pre-flight checks ---
info "Running integration test: Claude Code harness"

if ! kubectl get crd agentruns.sympozium.ai >/dev/null 2>&1; then
    fail "Sympozium CRDs not installed. Is the cluster set up?"
    exit 1
fi

if ! kubectl get deployment sympozium-controller-manager -n sympozium-system >/dev/null 2>&1; then
    fail "Sympozium controller not running."
    exit 1
fi

# --- Ensure Anthropic secret exists ---
if ! kubectl get secret "$SECRET_NAME" -n "$NAMESPACE" >/dev/null 2>&1; then
    if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
        fail "No ANTHROPIC_API_KEY set and secret '$SECRET_NAME' not found."
        echo "  Either: export ANTHROPIC_API_KEY=sk-ant-..."
        echo "  Or:     kubectl create secret generic $SECRET_NAME --from-literal=ANTHROPIC_API_KEY=sk-ant-..."
        exit 1
    fi
    info "Creating secret $SECRET_NAME from ANTHROPIC_API_KEY env var"
    kubectl create secret generic "$SECRET_NAME" \
        --from-literal=ANTHROPIC_API_KEY="$ANTHROPIC_API_KEY" \
        -n "$NAMESPACE"
fi

# --- Clean up any previous test run ---
cleanup 2>/dev/null || true
sleep 2

# --- Create test Agent ---
info "Creating Agent: $INSTANCE_NAME (harness: claude-code)"
cat <<EOF | kubectl apply -f -
apiVersion: sympozium.ai/v1alpha1
kind: Agent
metadata:
  name: ${INSTANCE_NAME}
  namespace: ${NAMESPACE}
spec:
  harness: claude-code
  agents:
    default:
      model: ${MODEL}
  authRefs:
    - secret: ${SECRET_NAME}
EOF

# --- Create test AgentRun ---
info "Creating AgentRun: $RUN_NAME (harness: claude-code, model: $MODEL)"
cat <<EOF | kubectl apply -f -
apiVersion: sympozium.ai/v1alpha1
kind: AgentRun
metadata:
  name: ${RUN_NAME}
  namespace: ${NAMESPACE}
  labels:
    sympozium.ai/instance: ${INSTANCE_NAME}
spec:
  agentRef: ${INSTANCE_NAME}
  agentId: default
  harness: claude-code
  sessionKey: "inttest-claude-code-$(date +%s)"
  task: |
    Write the exact text "${MARKER_TEXT}" to the file ${TARGET_FILE}.
    Do not add any extra content, newlines, or formatting — just that exact string.
    After writing, reply with one sentence confirming the path you wrote to.
  model:
    provider: anthropic
    model: ${MODEL}
    authSecretRef: ${SECRET_NAME}
  timeout: "5m"
EOF

# --- Wait for completion ---
info "Waiting up to ${TIMEOUT}s for AgentRun to complete..."
elapsed=0
phase=""
pod=""
while [[ $elapsed -lt $TIMEOUT ]]; do
    phase=$(kubectl get agentrun "$RUN_NAME" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    if [[ -z "$pod" ]]; then
        pod=$(kubectl get pods -n "$NAMESPACE" -l "sympozium.ai/agentrun=$RUN_NAME" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
        if [[ -n "$pod" ]]; then
            info "Pod found: $pod"
        fi
    fi
    if [[ "$phase" == "Succeeded" || "$phase" == "Failed" ]]; then
        break
    fi
    sleep 5
    elapsed=$((elapsed + 5))
    if (( elapsed % 15 == 0 )); then
        info "  ...${elapsed}s elapsed (phase: ${phase:-Pending})"
    fi
done

if [[ "$phase" != "Succeeded" && "$phase" != "Failed" ]]; then
    fail "AgentRun did not complete within ${TIMEOUT}s (last phase: ${phase:-unknown})"
    info "Debug: kubectl describe agentrun $RUN_NAME -n $NAMESPACE"
    if [[ -n "$pod" ]]; then
        info "Debug: kubectl logs $pod -c agent -n $NAMESPACE"
        kubectl logs "$pod" -c agent -n "$NAMESPACE" 2>/dev/null | tail -30 || true
    fi
    cleanup
    exit 1
fi

# --- Check result ---
echo ""
if [[ "$phase" == "Failed" ]]; then
    fail "AgentRun phase: Failed"
    kubectl get agentrun "$RUN_NAME" -n "$NAMESPACE" -o jsonpath='{.status}' | python3 -m json.tool 2>/dev/null || true
    if [[ -n "$pod" ]]; then
        info "Agent logs:"
        kubectl logs "$pod" -c agent -n "$NAMESPACE" 2>/dev/null | tail -30 || true
    fi
    cleanup
    exit 1
fi

pass "AgentRun phase: Succeeded"

result=$(kubectl get agentrun "$RUN_NAME" -n "$NAMESPACE" -o jsonpath='{.status.result}' 2>/dev/null || echo "")

failures=0
logs=""
image=""
if [[ -n "$pod" ]]; then
    logs=$(kubectl logs "$pod" -c agent -n "$NAMESPACE" 2>/dev/null || echo "")
    image=$(kubectl get pod "$pod" -n "$NAMESPACE" -o jsonpath='{.spec.containers[?(@.name=="agent")].image}' 2>/dev/null || echo "")
fi

# Validation 1: the agent container runs the harness-claude-code image
if [[ "$image" == *"harness-claude-code"* ]]; then
    pass "Agent container image is the Claude Code harness ($image)"
else
    if [[ -z "$pod" ]]; then
        info "Pod not found — cannot check image (job cleaned up too fast)"
    else
        fail "Agent container image is not harness-claude-code: '$image'"
        failures=$((failures + 1))
    fi
fi

# Validation 2: the shim ran and Claude Code produced a stream-json result event
if echo "$logs" | grep -q 'harness-claude-code: session .* finished' && echo "$logs" | grep -q '"type":"result"'; then
    pass "Pod logs show the shim summary and Claude Code's result event"
else
    if [[ -z "$pod" ]]; then
        info "Pod not found — cannot check logs"
    else
        fail "Pod logs lack harness-claude-code summary / stream-json result event"
        failures=$((failures + 1))
        if [[ -n "$logs" ]]; then
            info "Last 20 log lines:"
            echo "$logs" | tail -20
        fi
    fi
fi

# Validation 3: Claude Code used a tool (Write/Bash) to create the file
if echo "$logs" | grep -q 'harness-claude-code: tool_use'; then
    pass "Pod logs show tool invocations"
else
    if [[ -n "$pod" ]]; then
        fail "Pod logs show no tool_use events"
        failures=$((failures + 1))
    fi
fi

# Validation 4: the result text references the write
if echo "$result" | grep -qi "$MARKER_TEXT\|claude-code-test\|wrote\|written"; then
    pass "AgentRun result references the file write"
else
    fail "AgentRun result does not reference the file write"
    failures=$((failures + 1))
    info "Result (first 500 chars):"
    echo "$result" | head -c 500
    echo ""
fi

# Validation 5: best-effort file content check while the pod is still around
if [[ -n "$pod" ]]; then
    pod_phase=$(kubectl get pod "$pod" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    if [[ "$pod_phase" == "Running" || "$pod_phase" == "Succeeded" ]]; then
        file_content=$(kubectl exec "$pod" -c agent -n "$NAMESPACE" -- cat "$TARGET_FILE" 2>/dev/null || echo "")
        if [[ -n "$file_content" ]]; then
            if echo "$file_content" | grep -q "$MARKER_TEXT"; then
                pass "File content verified: contains '$MARKER_TEXT'"
            else
                fail "File exists but content doesn't match"
                failures=$((failures + 1))
                info "Got: $file_content"
            fi
        else
            info "Could not read file from pod (containers exited) — relying on log evidence"
        fi
    fi
fi

echo ""
echo "=============================="
echo " Claude Code Harness Test"
echo "=============================="
echo " AgentRun:  $RUN_NAME"
echo " Phase:     $phase"
echo " Harness:   claude-code"
echo " Model:     $MODEL"
if [[ -n "$pod" ]]; then
    echo " Pod:       $pod"
    echo " Image:     $image"
fi
echo " Failures:  $failures"
echo "=============================="
echo ""

cleanup

if [[ $failures -gt 0 ]]; then
    fail "Integration test finished with $failures failure(s)"
    exit 1
fi

pass "Claude Code harness integration test complete"
