#!/usr/bin/env bash
# Multi-node fleet proof on Kind: label N KVM nodes, every node prepares the
# reviewed starter package and sizes its own capacity, one router discovers
# them, enduring parents are issued through the gateway (several on one node),
# a follow-up turn keeps live context, any ordinary namespace runs with
# wrappers created on first use while an excluded one is refused, a second
# model backend (FLEET_SECOND_BACKEND, optional) serves the same namespace side
# by side, one-shot runs answer once on any backend as single-turn parents and
# give their cells back, a backend added to the running scope
# (FLEET_ADD_BACKEND, optional) serves runs without an owner restart, and
# removing a node's label drains its owner honestly.
#
# Requires: kind, docker, kubectl, helm, /dev/kvm, a readable host kernel in
# /boot, a model backend (FLEET_MODEL_PROVIDER=deepseek|openai|anthropic|llama-server
# with DEEPSEEK_API_KEY, OPENAI_API_KEY or ANTHROPIC_API_KEY, or
# FLEET_MODEL_ENDPOINT for llama-server; FLEET_MODEL names the model; an optional
# second backend as FLEET_SECOND_BACKEND="name=NAME,provider=..,model=..[,endpoint=..][,protocol=..][,allow-insecure=true]"
# with its key in FLEET_SECOND_MODEL_KEY), a celln bundle (bin/celln + share/celln with pilot
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
[ -n "${FLEET_MODEL_PROTOCOL:-}" ] && MODEL_ARGS+=(--celln-fleet-model-protocol "$FLEET_MODEL_PROTOCOL")
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
# With a second backend (or one added later) every backend is declared
# explicitly: the native backend from the FLEET_MODEL_* inputs, the second
# from FLEET_SECOND_BACKEND, and FLEET_ADD_BACKEND joins a running scope.
native_spec="name=native,provider=$MODEL_PROVIDER"
[ -n "${FLEET_MODEL:-}" ] && native_spec="$native_spec,model=$FLEET_MODEL"
[ -n "${FLEET_MODEL_ENDPOINT:-}" ] && native_spec="$native_spec,endpoint=$FLEET_MODEL_ENDPOINT"
[ -n "${FLEET_MODEL_PROTOCOL:-}" ] && native_spec="$native_spec,protocol=$FLEET_MODEL_PROTOCOL"
[ "${FLEET_MODEL_ALLOW_INSECURE:-}" = 1 ] && native_spec="$native_spec,allow-insecure=true"
[ -n "$MODEL_KEY" ] && native_spec="$native_spec,credential-file=$WORK/model-token"
BACKEND_ARGS=(--celln-fleet-backend "$native_spec")
SECOND_BACKEND=""
if [ -n "${FLEET_SECOND_BACKEND:-}" ]; then
	second_spec="$FLEET_SECOND_BACKEND"
	if [ -n "${FLEET_SECOND_MODEL_KEY:-}" ]; then
		printf '%s\n' "$FLEET_SECOND_MODEL_KEY" >"$WORK/second-token"
		second_spec="$second_spec,credential-file=$WORK/second-token"
	fi
	SECOND_BACKEND="$(printf '%s' "$FLEET_SECOND_BACKEND" | tr ',' '\n' | sed -n 's/^name=//p')"
	[ -n "$SECOND_BACKEND" ] || fail "FLEET_SECOND_BACKEND needs name=NAME"
	BACKEND_ARGS+=(--celln-fleet-backend "$second_spec")
fi
ADD_BACKEND=""
if [ -n "${FLEET_ADD_BACKEND:-}" ]; then
	ADD_BACKEND="$(printf '%s' "$FLEET_ADD_BACKEND" | tr ',' '\n' | sed -n 's/^name=//p')"
	[ -n "$ADD_BACKEND" ] || fail "FLEET_ADD_BACKEND needs name=NAME"
	add_spec="$FLEET_ADD_BACKEND"
	if [ -n "${FLEET_ADD_MODEL_KEY:-}" ]; then
		printf '%s\n' "$FLEET_ADD_MODEL_KEY" >"$WORK/add-token"
		add_spec="$add_spec,credential-file=$WORK/add-token"
	fi
fi
if [ -n "$SECOND_BACKEND" ] || [ -n "$ADD_BACKEND" ]; then
	CREDENTIAL_ARGS=()
	MODEL_ARGS=("${BACKEND_ARGS[@]}")
fi
kc create namespace "$NAMESPACE" 2>/dev/null || true
kc label node --overwrite -l '!node-role.kubernetes.io/control-plane' celln.dev/kvm=true >/dev/null
pass "images loaded; workers labeled celln.dev/kvm=true"

# A cached cert-manager manifest avoids depending on GitHub release downloads;
# the installer skips cert-manager when its namespace already exists.
if [ -n "${FLEET_CERT_MANAGER_MANIFEST:-}" ]; then
	kc apply -f "$FLEET_CERT_MANAGER_MANIFEST" >/dev/null
fi

log "sympozium install --celln-fleet"
rm -rf "$WORK/fleet-out" # a fresh scope starts from an empty private output directory
KUBECONFIG="$WORK/kubeconfig" kind export kubeconfig --name "$CLUSTER" --kubeconfig "$WORK/kubeconfig" >/dev/null
fleet_install() { # extra args... ; the same command is rerun to add nodes or backends
	KUBECONFIG="$WORK/kubeconfig" "$SYMPOZIUM" install -n "$NAMESPACE" --celln-fleet \
		--celln-fleet-scope "$SCOPE" \
		--celln-fleet-package-image "$registry/celln/starter@$digest" \
		--celln-fleet-package-hash "$package_hash" \
		--celln-fleet-publisher "$publisher" \
		"${CREDENTIAL_ARGS[@]}" \
		"${MODEL_ARGS[@]}" "$@" \
		--celln-fleet-output-dir "$WORK/fleet-out" \
		--celln-fleet-wait 20m \
		--celln-native-approve-starter-tools \
		--celln-router-image "$CELLN_IMAGE" \
		--celln-installer-image "ghcr.io/sympozium-ai/sympozium/celln-installer:$TAG" \
		--set celln.fleet.package.insecureRegistry=true \
		--set "controller.image.tag=$TAG" --set "apiserver.image.tag=$TAG" --set "webhook.image.tag=$TAG"
}
fleet_install >"$WORK/install.log" 2>&1 || { tail -20 "$WORK/install.log"; fail "fleet install"; }
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
# Run kubectl itself in the background (not the kc function, whose subshell
# would survive the kill) on a port no other journey holds.
api_port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')"
kubectl --context "kind-$CLUSTER" -n sympozium-system port-forward svc/sympozium-apiserver "$api_port:8080" >/dev/null 2>&1 &
api_pf=$!
trap 'kill "$api_pf" 2>/dev/null || true' EXIT
wait_for "apiserver port-forward" 60 curl -sf "${api_auth[@]}" "http://127.0.0.1:$api_port/api/v1/celln-platform/profiles?namespace=$tenant" -o /dev/null
curl -sf "${api_auth[@]}" "http://127.0.0.1:$api_port/api/v1/celln-platform/profiles?namespace=$tenant" | grep -q "\"name\":\"$profile\"" || fail "platform profile $profile not offered to $tenant"
curl -sf "${api_auth[@]}" -X POST -H 'Content-Type: application/json' -d "{\"profile\":\"$profile\"}" "http://127.0.0.1:$api_port/api/v1/celln-platform/wrappers?namespace=$tenant" | grep -q '"connection":"celln-native"' || fail "wrappers were not created in $tenant"
if [ -n "$SECOND_BACKEND" ]; then
	# The second backend is a second profile of the same scope with its own
	# wrapper names; the same namespace gets both.
	second_profile="$profile-$SECOND_BACKEND"
	curl -sf "${api_auth[@]}" "http://127.0.0.1:$api_port/api/v1/celln-platform/profiles?namespace=$tenant" | grep -q "\"name\":\"$second_profile\"" || fail "second backend profile $second_profile not offered to $tenant"
	curl -sf "${api_auth[@]}" -X POST -H 'Content-Type: application/json' -d "{\"profile\":\"$second_profile\"}" "http://127.0.0.1:$api_port/api/v1/celln-platform/wrappers?namespace=$tenant" | grep -q "\"connection\":\"celln-$SECOND_BACKEND\"" || fail "second backend wrappers were not created in $tenant"
	pass "$tenant offers both backends: $profile (celln-native) and $second_profile (celln-$SECOND_BACKEND)"
fi
[ "$(curl -s "${api_auth[@]}" "http://127.0.0.1:$api_port/api/v1/celln-platform/profiles?namespace=$denied")" = "[]" ] || fail "excluded namespace $denied was offered a profile"
[ "$(curl -s -o /dev/null -w '%{http_code}' "${api_auth[@]}" -X POST -H 'Content-Type: application/json' -d "{\"profile\":\"$profile\"}" "http://127.0.0.1:$api_port/api/v1/celln-platform/wrappers?namespace=$denied")" = 403 ] || fail "excluded namespace $denied was prepared"
# The excluded namespace applies the same objects by hand so its refusal is the policy's, not a missing Agent.
for kind in modelconnection agentruntime agent; do
	kc -n "$NAMESPACE" get "$kind" -o json | python3 -c "
import json, sys
for item in json.load(sys.stdin)['items']:
    print(json.dumps({'apiVersion': item['apiVersion'], 'kind': item['kind'], 'metadata': {'name': item['metadata']['name'], 'namespace': sys.argv[1]}, 'spec': item['spec']}))" "$denied" | kc apply -f - >/dev/null
done
# Tenants start runs through the API, as the UI does, and one Agent may hold
# several enduring conversations at once.
api_run() { # task [backend] -> run name, via POST /api/v1/runs; a backend selects that backend's wrappers and model
	python3 -c '
import json, sys
r = json.load(open(sys.argv[1]))["spec"]
backend = sys.argv[3] if len(sys.argv) > 3 else ""
agent, runtime, connection, model = r["agentRef"], r["cellnSelection"]["runtimeRef"], r["model"]["connectionRef"], r["model"]["model"]
if backend:
    agent, runtime, connection = f"celln-agent-{backend}", f"celln-{backend}", f"celln-{backend}"
    model = json.load(open(sys.argv[4]))["model"]["model"]
print(json.dumps({"agentRef": agent, "task": sys.argv[2], "systemPrompt": r["systemPrompt"], "backend": "celln",
  "executionLifecycle": "enduring", "enduring": r["enduring"], "model": model, "modelConnectionRef": connection,
  "cellnSelection": {"runtimeRef": runtime, "clusterToolRefs": r["cellnSelection"]["clusterToolRefs"], "toolRefs": []}}))' "$WORK/fleet-out/installation/run.json" "$1" ${2:+"$2" "$WORK/fleet-out/configuration/$2/configured.json"} |
		curl -sf "${api_auth[@]}" -X POST -H 'Content-Type: application/json' --data-binary @- "http://127.0.0.1:$api_port/api/v1/runs?namespace=$tenant" |
		python3 -c 'import json,sys; print(json.load(sys.stdin)["metadata"]["name"])'
}
tenant_task="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["spec"]["task"])' "$WORK/fleet-out/installation/run.json")"
tenant_run="$(api_run "$tenant_task")" || fail "API refused an enduring run in $tenant"
tenant_run2="$(api_run "Remember the word saffron. Reply with one short sentence; do not use tools.")" || fail "API refused a second conversation for the same Agent in $tenant"
tenant_runs=("$tenant_run" "$tenant_run2")
second_run=""
if [ -n "$SECOND_BACKEND" ]; then
	second_run="$(api_run "Where is Botswana? Reply with one short sentence; do not use tools." "$SECOND_BACKEND")" || fail "API refused a conversation on backend $SECOND_BACKEND in $tenant"
	tenant_runs+=("$second_run")
fi
kill "$api_pf" >/dev/null 2>&1 || true
for run in "${tenant_runs[@]}"; do
	wait_for "parent $run ready in $tenant" 240 run_ready "$run" "$tenant"
	wait_for "initial turn of $run" 300 bash -c "kubectl --context kind-$CLUSTER -n $tenant get agentrun $run -o jsonpath='{.status.cellnParent.initialTurn.result.succeeded}' | grep -q true"
done
[ "$(kc -n "$tenant" get agentrun "$tenant_run2" -o jsonpath='{.status.cellnParent.binding.incarnation}')" != "$(kc -n "$tenant" get agentrun "$tenant_run" -o jsonpath='{.status.cellnParent.binding.incarnation}')" ] || fail "two conversations shared a parent"
pass "two enduring conversations of one Agent started through the API in $tenant, each with its own parent"
if [ -n "$second_run" ]; then
	[ "$(kc -n "$tenant" get agentrun "$second_run" -o jsonpath='{.spec.cellnSelection.runtimeRef}{" "}{.spec.model.connectionRef}')" = "celln-$SECOND_BACKEND celln-$SECOND_BACKEND" ] || fail "$second_run did not run on backend $SECOND_BACKEND"
	second_answer="$(kc -n "$tenant" get agentrun "$second_run" -o jsonpath='{.status.cellnParent.initialTurn.result.answer}' 2>/dev/null || true)"
	pass "$second_run answered on backend $SECOND_BACKEND in the same namespace as the native runs: ${second_answer:0:160}"
fi

log "One-shot runs on the fleet: a single-turn parent per run, any backend, finished with its answer"
# The same API call as an enduring conversation minus the lifecycle and lease:
# the platform admits it, one parent answers once, the run succeeds with the
# answer and its parent is stopped so the node's cells come back.
live_cells() { # sum of live cells across every owner
	local total=0 n
	for pod in $(kubectl --context "kind-$CLUSTER" -n celln-system get pods -l app.kubernetes.io/name=celln-node --field-selector status.phase=Running -o name); do
		n="$(kubectl --context "kind-$CLUSTER" -n celln-system exec "$pod" -c dispatcher -- curl -s http://127.0.0.1:8787/v1/health | python3 -c 'import json,sys; print(json.load(sys.stdin)["node"]["live_cells"])')" || n=0
		total=$((total + n))
	done
	echo "$total"
}
cells_before="$(live_cells)"
kubectl --context "kind-$CLUSTER" -n sympozium-system port-forward svc/sympozium-apiserver "$api_port:8080" >/dev/null 2>&1 &
api_pf=$!
wait_for "apiserver port-forward" 60 curl -sf "${api_auth[@]}" "http://127.0.0.1:$api_port/api/v1/celln-platform/profiles?namespace=$tenant" -o /dev/null
api_one_shot() { # task [backend] -> run name, via POST /api/v1/runs with the one-shot lifecycle
	python3 -c '
import json, sys
r = json.load(open(sys.argv[1]))["spec"]
backend = sys.argv[3] if len(sys.argv) > 3 else ""
agent, runtime, connection, model = r["agentRef"], r["cellnSelection"]["runtimeRef"], r["model"]["connectionRef"], r["model"]["model"]
if backend:
    agent, runtime, connection = f"celln-agent-{backend}", f"celln-{backend}", f"celln-{backend}"
    model = json.load(open(sys.argv[4]))["model"]["model"]
print(json.dumps({"agentRef": agent, "task": sys.argv[2], "systemPrompt": r["systemPrompt"], "backend": "celln",
  "executionLifecycle": "one-shot", "model": model, "modelConnectionRef": connection,
  "cellnSelection": {"runtimeRef": runtime, "clusterToolRefs": r["cellnSelection"]["clusterToolRefs"], "toolRefs": []}}))' "$WORK/fleet-out/installation/run.json" "$1" ${2:+"$2" "$WORK/fleet-out/configuration/$2/configured.json"} |
		curl -sf "${api_auth[@]}" -X POST -H 'Content-Type: application/json' --data-binary @- "http://127.0.0.1:$api_port/api/v1/runs?namespace=$tenant" |
		python3 -c 'import json,sys; print(json.load(sys.stdin)["metadata"]["name"])'
}
one_shot_runs=()
one_shot_native="$(api_one_shot "Where is Botswana? Reply with one short sentence; do not use tools.")" || fail "API refused a one-shot run on the native backend in $tenant"
one_shot_runs+=("$one_shot_native")
if [ -n "$SECOND_BACKEND" ]; then
	one_shot_second="$(api_one_shot "What is the capital of Botswana? Reply with one short sentence; do not use tools." "$SECOND_BACKEND")" || fail "API refused a one-shot run on backend $SECOND_BACKEND in $tenant"
	one_shot_runs+=("$one_shot_second")
fi
kill "$api_pf" >/dev/null 2>&1 || true
for run in "${one_shot_runs[@]}"; do
	wait_for "one-shot $run to finish" 300 bash -c "kubectl --context kind-$CLUSTER -n $tenant get agentrun $run -o jsonpath='{.status.phase}' | grep -qE 'Succeeded|Failed'"
	[ "$(kc -n "$tenant" get agentrun "$run" -o jsonpath='{.status.phase}')" = Succeeded ] || fail "one-shot $run failed: $(kc -n "$tenant" get agentrun "$run" -o jsonpath='{.status.error}')"
	[ -n "$(kc -n "$tenant" get agentrun "$run" -o jsonpath='{.status.cellnParent.binding.incarnation}')" ] || fail "one-shot $run did not run as a native parent"
	answer="$(kc -n "$tenant" get agentrun "$run" -o jsonpath='{.status.result}')"
	[ -n "$answer" ] || fail "one-shot $run finished without a result"
	pass "one-shot $run (runtime $(kc -n "$tenant" get agentrun "$run" -o jsonpath='{.spec.cellnSelection.runtimeRef}')) succeeded: ${answer:0:160}"
done
wait_for "one-shot parents released (live cells back to $cells_before)" 120 bash -c "[ \"\$(
	total=0; for pod in \$(kubectl --context kind-$CLUSTER -n celln-system get pods -l app.kubernetes.io/name=celln-node --field-selector status.phase=Running -o name); do
		n=\$(kubectl --context kind-$CLUSTER -n celln-system exec \$pod -c dispatcher -- curl -s http://127.0.0.1:8787/v1/health | python3 -c 'import json,sys; print(json.load(sys.stdin)[\"node\"][\"live_cells\"])'); total=\$((total + n)); done; echo \$total)\" = $cells_before ]"
pass "one-shot parents stopped after answering; ${#one_shot_runs[@]} run(s) left the node's cells as they were ($cells_before live)"
if [ -n "$ADD_BACKEND" ]; then
	log "Adding backend $ADD_BACKEND to the running scope: owners untouched, conversations keep going"
	owners_before="$(kc -n celln-system get pods -l app.kubernetes.io/name=celln-node -o jsonpath='{.items[*].metadata.uid}' | tr ' ' '\n' | sort)"
	fleet_install --celln-fleet-backend "$add_spec" >"$WORK/install-add.log" 2>&1 || { tail -20 "$WORK/install-add.log"; fail "adding backend $ADD_BACKEND"; }
	owners_after="$(kc -n celln-system get pods -l app.kubernetes.io/name=celln-node -o jsonpath='{.items[*].metadata.uid}' | tr ' ' '\n' | sort)"
	[ "$owners_before" = "$owners_after" ] || fail "adding a backend restarted the owners"
	kc get cellnruntimeprofile "$profile-$ADD_BACKEND" >/dev/null || fail "profile $profile-$ADD_BACKEND not installed"
	[ "$(kc get cellnexecutionpolicy "celln-fleet-$SCOPE" -o jsonpath='{.spec.runtimeProfiles[*].ref.name}' | tr ' ' '\n' | grep -c .)" -ge 2 ] || fail "policy did not grow"
	kc -n celln-system get configmap celln-fleet-configuration -o jsonpath='{.data}' | grep -q "$ADD_BACKEND.configured.json" || fail "nodes did not publish $ADD_BACKEND"
	[ "$(kc -n "$tenant" get agentrun "$tenant_run" -o jsonpath='{.status.phase}')" = Running ] || fail "$tenant_run did not survive the backend addition"
	[ -f "$WORK/fleet-out/configuration/$ADD_BACKEND/configured.json" ] || fail "installer did not materialize $ADD_BACKEND"
	pass "backend $ADD_BACKEND joined scope $SCOPE without an owner restart; $tenant_run still running"
	kubectl --context "kind-$CLUSTER" -n sympozium-system port-forward svc/sympozium-apiserver "$api_port:8080" >/dev/null 2>&1 &
	api_pf=$!
	wait_for "apiserver port-forward" 60 curl -sf "${api_auth[@]}" "http://127.0.0.1:$api_port/api/v1/celln-platform/profiles?namespace=$tenant" -o /dev/null
	curl -sf "${api_auth[@]}" "http://127.0.0.1:$api_port/api/v1/celln-platform/profiles?namespace=$tenant" | grep -q "\"name\":\"$profile-$ADD_BACKEND\"" || fail "added backend not offered to $tenant"
	curl -sf "${api_auth[@]}" -X POST -H 'Content-Type: application/json' -d "{\"profile\":\"$profile-$ADD_BACKEND\"}" "http://127.0.0.1:$api_port/api/v1/celln-platform/wrappers?namespace=$tenant" | grep -q "\"connection\":\"celln-$ADD_BACKEND\"" || fail "added backend wrappers were not created in $tenant"
	added_run="$(api_one_shot "Name one river in Botswana. Reply with one short sentence; do not use tools." "$ADD_BACKEND")" || fail "API refused a one-shot on added backend $ADD_BACKEND"
	kill "$api_pf" >/dev/null 2>&1 || true
	wait_for "one-shot $added_run on added backend" 300 bash -c "kubectl --context kind-$CLUSTER -n $tenant get agentrun $added_run -o jsonpath='{.status.phase}' | grep -qE 'Succeeded|Failed'"
	[ "$(kc -n "$tenant" get agentrun "$added_run" -o jsonpath='{.status.phase}')" = Succeeded ] || fail "one-shot on added backend failed: $(kc -n "$tenant" get agentrun "$added_run" -o jsonpath='{.status.error}')"
	pass "one-shot $added_run on added backend $ADD_BACKEND succeeded: $(kc -n "$tenant" get agentrun "$added_run" -o jsonpath='{.status.result}' | cut -c1-160)"
fi
if [ -n "$second_run" ]; then
	kc -n "$tenant" delete agentrun "$second_run" --timeout=180s >/dev/null || fail "$second_run could not be deleted"
fi
kc -n "$tenant" delete agentrun "$tenant_run2" --timeout=180s >/dev/null || fail "$tenant_run2 could not be deleted"
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
