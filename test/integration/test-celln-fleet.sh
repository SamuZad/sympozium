#!/usr/bin/env bash
# Multi-node fleet proof on Kind: label N KVM nodes, every node prepares the
# reviewed starter package, one router discovers them, enduring parents are
# issued through the gateway on distinct owners, a follow-up turn keeps live
# context, and removing a node's label drains its owner honestly.
#
# Requires: kind, docker, kubectl, helm, /dev/kvm, a readable host kernel in
# /boot, DEEPSEEK_API_KEY, a celln bundle (bin/celln + share/celln with pilot
# binaries, scripts, guest) matching config/celln/release.json plus #109/#110,
# and the sympozium controller/apiserver/webhook/celln-installer images tagged
# $SYMPOZIUM_IMAGE_TAG together with the celln image $CELLN_IMAGE. See
# docs/guides/celln-fleet-installation.md.
set -euo pipefail

: "${CELLN_BUNDLE:?bundle directory with bin/celln and share/celln}"
: "${DEEPSEEK_API_KEY:?}"
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
printf '%s\n' "$DEEPSEEK_API_KEY" >"$WORK/model-token"
kc create namespace "$NAMESPACE" 2>/dev/null || true
kc label node --overwrite -l '!node-role.kubernetes.io/control-plane' celln.dev/kvm=true >/dev/null
pass "images loaded; workers labeled celln.dev/kvm=true"

log "sympozium install --celln-fleet"
KUBECONFIG="$WORK/kubeconfig" kind export kubeconfig --name "$CLUSTER" --kubeconfig "$WORK/kubeconfig" >/dev/null
KUBECONFIG="$WORK/kubeconfig" "$SYMPOZIUM" install -n "$NAMESPACE" --celln-fleet \
	--celln-fleet-scope "$SCOPE" \
	--celln-fleet-package-image "$registry/celln/starter@$digest" \
	--celln-fleet-package-hash "$package_hash" \
	--celln-fleet-publisher "$publisher" \
	--celln-fleet-model-credential-file "$WORK/model-token" \
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

log "Enduring runs are issued through the gateway on distinct owners"
run_ready() { [ "$(kc -n "$NAMESPACE" get agentrun "$1" -o jsonpath='{.status.conditions[?(@.type=="CellnParentReady")].status}')" = True ]; }
first="$(kc -n "$NAMESPACE" create -f "$WORK/fleet-out/installation/run.json" -o jsonpath='{.metadata.name}')"
second="$(kc -n "$NAMESPACE" create -f "$WORK/fleet-out/installation/run.json" -o jsonpath='{.metadata.name}')"
for run in "$first" "$second"; do
	wait_for "parent $run ready" 240 run_ready "$run"
	[ "$(kc -n "$NAMESPACE" get agentrun "$run" -o jsonpath='{.status.cellnParent.binding.target}')" = "http://celln-router.celln-system.svc.cluster.local:8787" ] || fail "$run not issued through the gateway"
	wait_for "initial turn of $run" 240 bash -c "kc() { kubectl --context kind-$CLUSTER \"\$@\"; }; kc -n $NAMESPACE get agentrun $run -o jsonpath='{.status.cellnParent.initialTurn.result.succeeded}' | grep -q true"
done
node_of() { # incarnation -> node holding its journal
	for pod in $(kc -n celln-system get pods -l app.kubernetes.io/name=celln-node -o name); do
		if kc -n celln-system exec "$pod" -c dispatcher -- test -e "/var/lib/sympozium-celln/$SCOPE/authority/parent-journal/${1#blake3:}" 2>/dev/null; then
			kc -n celln-system get "$pod" -o jsonpath='{.spec.nodeName}'
		fi
	done
}
first_node="$(node_of "$(kc -n "$NAMESPACE" get agentrun "$first" -o jsonpath='{.status.cellnParent.binding.incarnation}')")"
second_node="$(node_of "$(kc -n "$NAMESPACE" get agentrun "$second" -o jsonpath='{.status.cellnParent.binding.incarnation}')")"
[ -n "$first_node" ] && [ -n "$second_node" ] || fail "owner journals not found"
[ "$first_node" != "$second_node" ] || fail "both parents landed on $first_node; expected the gateway to spread owners"
pass "$first on $first_node and $second on $second_node completed real model turns"

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

log "Removing a node's label drains its owner and reports context loss"
kc label node "$second_node" celln.dev/kvm- >/dev/null
wait_for "owner drain on $second_node" 120 bash -c "[ \$(kubectl --context kind-$CLUSTER -n celln-system get pods -l app.kubernetes.io/name=celln-node --field-selector spec.nodeName=$second_node -o name | wc -l) = 0 ]"
wait_for "context loss report for $second" 120 bash -c "kubectl --context kind-$CLUSTER -n $NAMESPACE get agentrun $second -o jsonpath='{.status.error}' | grep -q 'context lost or stopped'"
run_ready "$first" || fail "$first on $first_node was affected by draining $second_node"
pass "$second reports ContextLost with owner outcome; $first still Ready"

kc label node --overwrite "$second_node" celln.dev/kvm=true >/dev/null
pass "celln fleet integration complete (work dir: $WORK)"
