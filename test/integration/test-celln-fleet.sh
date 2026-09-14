#!/usr/bin/env bash
# Multi-node fleet proof on Kind: label N KVM nodes, every node prepares the
# reviewed starter package and sizes its own capacity, one router discovers
# them, enduring parents are issued through the gateway (several on one node),
# a follow-up turn keeps live context, any ordinary namespace runs with
# wrappers created on first use while an excluded one is refused, and removing
# a node's label drains its owner honestly.
#
# Requires: kind, docker, kubectl, helm, /dev/kvm, a readable host kernel in
# /boot, a model backend (FLEET_MODEL_PROVIDER=deepseek|openai|anthropic|llama-server
# with DEEPSEEK_API_KEY, OPENAI_API_KEY or ANTHROPIC_API_KEY, or
# FLEET_MODEL_ENDPOINT for llama-server; FLEET_MODEL names the model), a celln bundle (bin/celln + share/celln with pilot
# binaries, scripts, guest) matching config/celln/release.json plus #109/#110,
# and the sympozium controller/apiserver/webhook/celln-installer images tagged
# $SYMPOZIUM_IMAGE_TAG together with the celln image $CELLN_IMAGE. See
# docs/guides/celln-fleet-installation.md.
set -euo pipefail

: "${CELLN_BUNDLE:?bundle directory with bin/celln and share/celln}"
MODEL_PROVIDER="${FLEET_MODEL_PROVIDER:-deepseek}"
MODEL_ARGS=(--celln-fleet-model-provider "$MODEL_PROVIDER")
[ -n "${FLEET_MODEL:-}" ] && MODEL_ARGS+=(--celln-fleet-model "$FLEET_MODEL")
[ -n "${FLEET_MODEL_ENDPOINT:-}" ] && MODEL_ARGS+=(--celln-fleet-model-endpoint "$FLEET_MODEL_ENDPOINT")
[ "${FLEET_MODEL_ALLOW_INSECURE:-}" = 1 ] && MODEL_ARGS+=(--celln-fleet-model-allow-insecure)
case "$MODEL_PROVIDER" in
deepseek) MODEL_KEY="${DEEPSEEK_API_KEY:?DEEPSEEK_API_KEY for the deepseek backend}" ;;
openai) MODEL_KEY="${OPENAI_API_KEY:?OPENAI_API_KEY for the openai backend}" ;;
anthropic) MODEL_KEY="${ANTHROPIC_API_KEY:?ANTHROPIC_API_KEY for the anthropic backend}" ;;
*) MODEL_KEY="${FLEET_MODEL_KEY:-}" ;;
esac
CLUSTER="${FLEET_CLUSTER:-fleet}"
SCOPE="${FLEET_SCOPE:-trial}"
TAG="${SYMPOZIUM_IMAGE_TAG:-fleet-trial}"
CELLN_IMAGE="${CELLN_IMAGE:-celln:$TAG}"
REGISTRY_NAME="${FLEET_REGISTRY_NAME:-kind-registry}"
WORK="${FLEET_WORK:-$(mktemp -d /tmp/celln-fleet.XXXXXX)}"
SYMPOZIUM="${SYMPOZIUM_BIN:-$WORK/sympozium}"
NAMESPACE="${FLEET_NAMESPACE:-celln-agents}"
REPO="$(cd "$(dirname "$0")/../.." && pwd)"
log() { printf '\033[1;33m---- %s\033[0m\n' "$*"; }
pass() { printf '\033[0;32mPASS %s\033[0m\n' "$*"; }
fail() { printf '\033[0;31mFAIL %s\033[0m\n' "$*" >&2; exit 1; }
kc() { kubectl --context "kind-$CLUSTER" "$@"; }
wait_for() { # description seconds command...
	local what="$1" limit="$2" i=0
	shift 2
	until "$@" >/dev/null 2>&1; do
		[ "$i" -lt "$limit" ] || fail "$what did not happen within ${limit}s"
		sleep 5
		i=$((i + 5))
	done
}

log "Cluster and registry"
if ! docker inspect "$REGISTRY_NAME" >/dev/null 2>&1; then
	docker run -d --restart=always -p 127.0.0.1:5001:5000 --name "$REGISTRY_NAME" registry:2 >/dev/null
fi
if ! kind get clusters | grep -qx "$CLUSTER"; then
	printf 'kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnodes:\n  - role: control-plane\n  - role: worker\n  - role: worker\n' >"$WORK/kind.yaml"
	kind create cluster --name "$CLUSTER" --config "$WORK/kind.yaml" --wait 120s
fi
docker network connect kind "$REGISTRY_NAME" 2>/dev/null || true
registry="$(docker inspect -f '{{(index .NetworkSettings.Networks "kind").IPAddress}}' "$REGISTRY_NAME"):5000"
# Kind nodes carry no kernel; the dispatcher's readiness needs one in /boot.
kernel="/boot/vmlinuz-$(uname -r)"
[ -r "$kernel" ] || fail "readable host kernel required at $kernel"
for node in $(kind get nodes --name "$CLUSTER" | grep worker); do
	docker exec "$node" test -f "/boot/$(basename "$kernel")" || docker cp "$kernel" "$node:/boot/"
done
pass "cluster $CLUSTER with registry $registry"

log "Starter package (built once per bundle)"
package="$WORK/package"
if [ ! -f "$package/package.json" ]; then
	[ -f "$WORK/publisher.seed" ] || { head -c 32 /dev/urandom >"$WORK/publisher.seed"; chmod 600 "$WORK/publisher.seed"; }
	"$CELLN_BUNDLE/bin/celln" starter-package --runtime-dir "$CELLN_BUNDLE/share/celln" --guest-dir "$CELLN_BUNDLE/share/celln/pilot" \
		--kernel "$kernel" --signing-key "$WORK/publisher.seed" --output "$package" >"$WORK/starter-package.log" 2>&1
fi
"$CELLN_BUNDLE/bin/celln" starter-inspect "$package" >"$WORK/inspect.json"
package_hash="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["packageHash"])' "$WORK/inspect.json")"
publisher="$(python3 -c '
import json, sys
def find(o):
    if isinstance(o, dict):
        for k, v in o.items():
            if k == "publisher" and isinstance(v, str): yield v
            yield from find(v)
    elif isinstance(o, list):
        for i in o: yield from find(i)
print(sorted(set(find(json.load(open(sys.argv[1])))))[0])' "$WORK/inspect.json")"
rm -rf "$WORK/package-image" && mkdir -p "$WORK/package-image" && cp -r "$package" "$WORK/package-image/package"
printf 'FROM scratch\nCOPY package /package\n' >"$WORK/package-image/Dockerfile"
docker build -q -t "localhost:5001/celln/starter:$SCOPE" "$WORK/package-image" >/dev/null
docker push -q "localhost:5001/celln/starter:$SCOPE"
digest="$(docker inspect --format '{{index .RepoDigests 0}}' "localhost:5001/celln/starter:$SCOPE" | sed 's/.*@//')"
pass "package $package_hash by $publisher at $registry/celln/starter@$digest"

log "Images and CLI"
for image in "$CELLN_IMAGE" "ghcr.io/sympozium-ai/sympozium/controller:$TAG" "ghcr.io/sympozium-ai/sympozium/apiserver:$TAG" "ghcr.io/sympozium-ai/sympozium/webhook:$TAG" "ghcr.io/sympozium-ai/sympozium/celln-installer:$TAG"; do
	kind load docker-image --name "$CLUSTER" "$image" >/dev/null
done
[ -x "$SYMPOZIUM" ] || (cd "$REPO" && go build -o "$SYMPOZIUM" ./cmd/sympozium)
umask 077
rm -f "$WORK/model-token"
CREDENTIAL_ARGS=()
if [ -n "$MODEL_KEY" ]; then
	printf '%s\n' "$MODEL_KEY" >"$WORK/model-token"
	CREDENTIAL_ARGS=(--celln-fleet-model-credential-file "$WORK/model-token")
fi
kc create namespace "$NAMESPACE" 2>/dev/null || true
kc label node --overwrite -l '!node-role.kubernetes.io/control-plane' celln.dev/kvm=true >/dev/null
pass "images loaded; workers labeled celln.dev/kvm=true"

log "sympozium install --celln-fleet"
rm -rf "$WORK/fleet-out" # the installer refuses an existing private output directory
KUBECONFIG="$WORK/kubeconfig" kind export kubeconfig --name "$CLUSTER" --kubeconfig "$WORK/kubeconfig" >/dev/null
KUBECONFIG="$WORK/kubeconfig" "$SYMPOZIUM" install -n "$NAMESPACE" --celln-fleet \
	--celln-fleet-scope "$SCOPE" \
	--celln-fleet-package-image "$registry/celln/starter@$digest" \
	--celln-fleet-package-hash "$package_hash" \
	--celln-fleet-publisher "$publisher" \
	"${CREDENTIAL_ARGS[@]}" \
	"${MODEL_ARGS[@]}" \
	--celln-fleet-output-dir "$WORK/fleet-out" \
	--celln-fleet-wait 20m \
	--celln-native-approve-starter-tools \
	--celln-router-image "$CELLN_IMAGE" \
	--celln-installer-image "ghcr.io/sympozium-ai/sympozium/celln-installer:$TAG" \
	--set celln.fleet.package.insecureRegistry=true \
	--set "controller.image.tag=$TAG" --set "apiserver.image.tag=$TAG" --set "webhook.image.tag=$TAG" >"$WORK/install.log" 2>&1 || { tail -20 "$WORK/install.log"; fail "fleet install"; }
owners="$(kc -n celln-system get pods -l app.kubernetes.io/name=celln-node --field-selector status.phase=Running -o name | wc -l)"
[ "$owners" -eq 2 ] || fail "expected 2 running owners, got $owners"
kc -n sympozium-system get deploy sympozium-controller-manager -o jsonpath='{.spec.template.spec.nodeSelector}' | grep -q hostname && fail "controller pinned to a node"
pass "two owners prepared from one package; controller unpinned; catalogue installed in $NAMESPACE"

log "Enduring runs are issued through the gateway to fleet owners"
run_ready() { [ "$(kc -n "${2:-$NAMESPACE}" get agentrun "$1" -o jsonpath='{.status.conditions[?(@.type=="CellnParentReady")].status}')" = True ]; }
node_of() { # launch profile -> node whose owner issued it
	for pod in $(kc -n celln-system get pods -l app.kubernetes.io/name=celln-node -o name); do
		if kc -n celln-system exec "$pod" -c dispatcher -- test -e "/var/lib/sympozium-celln/$SCOPE/authority/trusted-parent-launches/${1#blake3:}.json" 2>/dev/null; then
			kc -n celln-system get "$pod" -o jsonpath='{.spec.nodeName}'
		fi
	done
}
launch_of() { kc -n "$NAMESPACE" get agentrun "$1" -o jsonpath='{.status.cellnParent.binding.launchProfile}'; }
first="$(kc -n "$NAMESPACE" create -f "$WORK/fleet-out/installation/run.json" -o jsonpath='{.metadata.name}')"
wait_for "parent $first ready" 240 run_ready "$first"
[ "$(kc -n "$NAMESPACE" get agentrun "$first" -o jsonpath='{.status.cellnParent.binding.target}')" = "http://celln-router.celln-system.svc.cluster.local:8787" ] || fail "$first not issued through the gateway"
wait_for "initial turn of $first" 240 bash -c "kubectl --context kind-$CLUSTER -n $NAMESPACE get agentrun $first -o jsonpath='{.status.cellnParent.initialTurn.result.succeeded}' | grep -q true"
first_node="$(node_of "$(launch_of "$first")")"
[ -n "$first_node" ] || fail "owner of $first not found"
pass "$first issued through the gateway to $first_node and completed a real model turn"

# The gateway places by incarnation hash, not by load. With per-parent broker
# charging (celln#112) and node-sized capacity a node holds many parents, so
# keep creating runs until one lands on the first owner and prove both live
# there. FLEET_EXPECT_COLOCATION=0 restores the one-parent-per-node assertion
# for Celln bundles without it.
extra=()
colocated=""
for attempt in 1 2 3 4 5 6; do
	run="$(kc -n "$NAMESPACE" create -f "$WORK/fleet-out/installation/run.json" -o jsonpath='{.metadata.name}')"
	extra+=("$run")
	wait_for "issuance of $run" 120 bash -c "[ -n \"\$(kubectl --context kind-$CLUSTER -n $NAMESPACE get agentrun $run -o jsonpath='{.status.cellnParent.binding.launchProfile}')\" ]"
	node="$(node_of "$(launch_of "$run")")"
	[ -n "$node" ] || fail "owner of $run not found"
	if [ "$node" != "$first_node" ]; then
		wait_for "parent $run ready" 240 run_ready "$run"
		pass "$run issued to the other owner $node"
		continue
	fi
	colocated="$run"
	break
done
[ -n "$colocated" ] || fail "no run hashed to $first_node in ${#extra[@]} attempts"
if [ "${FLEET_EXPECT_COLOCATION:-1}" = 1 ]; then
	wait_for "parent $colocated ready beside $first on $first_node" 240 run_ready "$colocated"
	wait_for "initial turn of $colocated" 240 bash -c "kubectl --context kind-$CLUSTER -n $NAMESPACE get agentrun $colocated -o jsonpath='{.status.cellnParent.initialTurn.result.succeeded}' | grep -q true"
	run_ready "$first" || fail "$first lost readiness when $colocated joined $first_node"
	pass "$colocated and $first are both live on $first_node and $colocated completed a real model turn"
else
	wait_for "capacity refusal of $colocated" 120 bash -c "kubectl --context kind-$CLUSTER -n $NAMESPACE get agentrun $colocated -o jsonpath='{.status.conditions[?(@.type==\"CellnParentReady\")].reason}' | grep -qE 'CreateRefused|ReconciliationRequired'"
	pass "$colocated hashed to $first_node and was refused there (one parent per node); not re-placed"
fi

log "Follow-up turn keeps live context on the same owner"
uid="$(kc -n "$NAMESPACE" get agentrun "$first" -o jsonpath='{.metadata.uid}')"
kc -n "$NAMESPACE" create -f - >/dev/null <<EOF
apiVersion: sympozium.ai/v1alpha1
kind: AgentRunTurn
metadata:
  name: $first-turn-2
  ownerReferences:
    - {apiVersion: sympozium.ai/v1alpha1, kind: AgentRun, name: $first, uid: "$uid", controller: true, blockOwnerDeletion: true}
spec:
  runName: $first
  runUID: "$uid"
  message: Read notes.txt with workspace-read and reply with exactly its content and revision.
EOF
wait_for "follow-up turn" 240 bash -c "kubectl --context kind-$CLUSTER -n $NAMESPACE get agentrunturn $first-turn-2 -o jsonpath='{.status.conditions[?(@.type==\"CellnTurnComplete\")].status}' | grep -q True"
answer="$(kc -n "$NAMESPACE" get agentrunturn "$first-turn-2" -o jsonpath='{.status.execution.result.answer}')"
echo "$answer" | grep -qi violet || fail "follow-up turn lost context: $answer"
[ "$(kc -n "$NAMESPACE" get agentrunturn "$first-turn-2" -o jsonpath='{.status.execution.child}')" != "$(kc -n "$NAMESPACE" get agentrun "$first" -o jsonpath='{.status.cellnParent.initialTurn.child}')" ] || fail "turn reused the initial child"
pass "distinct child read back: $(echo "$answer" | tr '\n' ' ')"

# Every run deletes cleanly through the gateway, live or refused.
for run in "$first" "${extra[@]}"; do
	kc -n "$NAMESPACE" delete agentrun "$run" --timeout=180s >/dev/null || fail "$run could not be deleted"
done
pass "$first and ${#extra[@]} other runs deleted through the gateway"

log "Any ordinary namespace runs on the fleet; wrappers are created on first use; an excluded one is refused"
tenant="$NAMESPACE-b"
denied="$NAMESPACE-denied"
profile="celln-native-$SCOPE"
kc create namespace "$tenant" >/dev/null 2>&1 || true
kc create namespace "$denied" >/dev/null 2>&1 || true
kc label namespace "$denied" --overwrite celln.sympozium.ai/excluded=true >/dev/null
# The tenant path is the API: list the profiles the namespace may run, then
# have the wrappers created. No label, no YAML.
api_token="$(kc -n sympozium-system get secret sympozium-ui-token -o jsonpath='{.data.token}' 2>/dev/null | base64 -d || true)"
[ -n "$api_token" ] || api_token="$(kc -n sympozium-system get deploy sympozium-apiserver -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="SYMPOZIUM_UI_TOKEN")].value}')"
api_auth=()
[ -n "$api_token" ] && api_auth=(-H "Authorization: Bearer $api_token")
kc -n sympozium-system port-forward svc/sympozium-apiserver 18080:8080 >/dev/null 2>&1 &
api_pf=$!
wait_for "apiserver port-forward" 60 curl -sf "${api_auth[@]}" "http://127.0.0.1:18080/api/v1/celln-platform/profiles?namespace=$tenant" -o /dev/null
curl -sf "${api_auth[@]}" "http://127.0.0.1:18080/api/v1/celln-platform/profiles?namespace=$tenant" | grep -q "\"name\":\"$profile\"" || fail "platform profile $profile not offered to $tenant"
curl -sf "${api_auth[@]}" -X POST -H 'Content-Type: application/json' -d "{\"profile\":\"$profile\"}" "http://127.0.0.1:18080/api/v1/celln-platform/wrappers?namespace=$tenant" | grep -q '"connection":"celln-native"' || fail "wrappers were not created in $tenant"
[ "$(curl -s "${api_auth[@]}" "http://127.0.0.1:18080/api/v1/celln-platform/profiles?namespace=$denied")" = "[]" ] || fail "excluded namespace $denied was offered a profile"
[ "$(curl -s -o /dev/null -w '%{http_code}' "${api_auth[@]}" -X POST -H 'Content-Type: application/json' -d "{\"profile\":\"$profile\"}" "http://127.0.0.1:18080/api/v1/celln-platform/wrappers?namespace=$denied")" = 403 ] || fail "excluded namespace $denied was prepared"
kill "$api_pf" >/dev/null 2>&1 || true
# The excluded namespace applies the same objects by hand so its refusal is the policy's, not a missing Agent.
for kind in modelconnection agentruntime agent; do
	kc -n "$NAMESPACE" get "$kind" -o json | python3 -c "
import json, sys
for item in json.load(sys.stdin)['items']:
    print(json.dumps({'apiVersion': item['apiVersion'], 'kind': item['kind'], 'metadata': {'name': item['metadata']['name'], 'namespace': sys.argv[1]}, 'spec': item['spec']}))" "$denied" | kc apply -f - >/dev/null
done
tenant_run="$(python3 -c "
import json, sys
r = json.load(open(sys.argv[1])); r['metadata']['namespace'] = sys.argv[2]; print(json.dumps(r))" "$WORK/fleet-out/installation/run.json" "$tenant" | kc create -f - -o jsonpath='{.metadata.name}')"
wait_for "parent $tenant_run ready in $tenant" 240 run_ready "$tenant_run" "$tenant"
wait_for "initial turn of $tenant_run" 240 bash -c "kubectl --context kind-$CLUSTER -n $tenant get agentrun $tenant_run -o jsonpath='{.status.cellnParent.initialTurn.result.succeeded}' | grep -q true"
[ "$(kc -n "$tenant" get configmap -o name | grep -c grant-)" = 0 ] || fail "grant ConfigMaps appeared in $tenant"
[ "$(kc -n "$tenant" get cellntool -o name | wc -l)" = 0 ] || fail "namespaced tools appeared in $tenant"
tenant_node="$(node_of "$(kc -n "$tenant" get agentrun "$tenant_run" -o jsonpath='{.status.cellnParent.binding.launchProfile}')")"
[ -n "$tenant_node" ] || fail "owner of $tenant_run not found"
pass "$tenant_run ran in $tenant on $tenant_node with wrappers created on first use (no label, no YAML, no grants, no copied tools)"
denied_run="$(python3 -c "
import json, sys
r = json.load(open(sys.argv[1])); r['metadata']['namespace'] = sys.argv[2]; print(json.dumps(r))" "$WORK/fleet-out/installation/run.json" "$denied" | kc create -f - -o jsonpath='{.metadata.name}')"
wait_for "policy refusal for $denied_run" 90 bash -c "kubectl --context kind-$CLUSTER -n $denied get agentrun $denied_run -o jsonpath='{.status.conditions[?(@.type==\"CellnParentReady\")].message}' | grep -q AUTH_POLICY_WITHDRAWN"
[ -z "$(kc -n "$denied" get agentrun "$denied_run" -o jsonpath='{.status.cellnParent}')" ] || fail "$denied_run was issued a parent without policy"
pass "$denied_run in excluded $denied refused with AUTH_POLICY_WITHDRAWN and no parent"

log "Removing a node's label drains its owner and reports context loss"
kc label node "$tenant_node" celln.dev/kvm- >/dev/null
wait_for "owner drain on $tenant_node" 120 bash -c "[ \$(kubectl --context kind-$CLUSTER -n celln-system get pods -l app.kubernetes.io/name=celln-node --field-selector spec.nodeName=$tenant_node -o name | wc -l) = 0 ]"
wait_for "context loss report for $tenant_run" 180 bash -c "kubectl --context kind-$CLUSTER -n $tenant get agentrun $tenant_run -o jsonpath='{.status.error}' | grep -q 'context lost or stopped'"
pass "$tenant_run reports ContextLost with owner outcome after its owner left"
kc -n "$tenant" delete agentrun "$tenant_run" --timeout=120s >/dev/null || fail "$tenant_run could not be deleted after its owner left"
pass "$tenant_run deleted; cleanup released after owner removal"

kc label node --overwrite "$tenant_node" celln.dev/kvm=true >/dev/null
pass "celln fleet integration complete (work dir: $WORK)"
