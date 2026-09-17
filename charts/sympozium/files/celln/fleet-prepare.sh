#!/usr/bin/env bash
# Prepares one KVM node's native Celln authority root from an operator-signed
# starter package and publishes the resulting starter configuration once per
# scope: one configuration per model backend, all from the same package.
# Every step is idempotent for the same package hash. Nothing here issues a
# run, reads a model credential or starts a guest outside the package's own
# admission checks.
set -euo pipefail

: "${FLEET_STATE:?}" "${FLEET_PACKAGE_IMAGE:?}" "${FLEET_PACKAGE_HASH:?}" "${FLEET_PUBLISHER:?}"
: "${FLEET_PRINCIPAL:?}" "${FLEET_SCOPE:?}" "${FLEET_BACKENDS:?}" "${FLEET_PARENT_CLIENTS:?}"
: "${FLEET_CONFIGURATION_CONFIGMAP:?}" "${FLEET_NAMESPACE:?}" "${NODE_NAME:?}"

# Backends added after the install arrive through a ConfigMap the API server
# writes (the same shape as the install-time list); they join it here so the
# rest of this script sees one list. A name the chart already carries wins.
extra=/etc/celln-fleet/backends-extra/backends.json
if [ -s "$extra" ]; then
	FLEET_BACKENDS="$(python3 -c 'import json, sys
base = json.loads(sys.argv[1])
names = {b["name"] for b in base}
print(json.dumps(base + [b for b in json.load(open(sys.argv[2])) if b["name"] not in names]))' "$FLEET_BACKENDS" "$extra")"
	export FLEET_BACKENDS
fi

celln=/usr/local/bin/celln
root="$FLEET_STATE/authority"
hex="${FLEET_PACKAGE_HASH#blake3:}"
if [ "${#hex}" -ne 64 ] || [ -n "${hex//[0-9a-f]/}" ]; then
	echo "invalid package hash: $FLEET_PACKAGE_HASH" >&2
	exit 1
fi
umask 077
install -d -m 0700 "$FLEET_STATE" "$root" "$root/motes" "$root/tools" \
	"$root/parent-issuance" "$root/trusted-parent-permits" "$root/trusted-parent-launches"

# Publisher and parent-principal trust come from operator configuration only;
# the package cannot install its own trust policy.
python3 - "$FLEET_PUBLISHER" >"$root/.trusted-closures.json" <<'PY'
import json, sys
print(json.dumps({"apiVersion": "celln.dev/closure-policy-v1", "publishers": [sys.argv[1]], "revoked": []}))
PY
mv -f "$root/.trusted-closures.json" "$root/trusted-closures.json"
cp "$FLEET_PARENT_CLIENTS" "$root/.trusted-parent-clients.json"
mv -f "$root/.trusted-parent-clients.json" "$root/trusted-parent-clients.json"

package="$FLEET_STATE/package-$hex"
if [ ! -f "$package/package.json" ]; then
	staging="$(mktemp -d "$FLEET_STATE/.package-XXXXXX")"
	trap 'rm -rf "$staging"' EXIT
	pull=()
	if [ -f /etc/celln-fleet/registry/.dockerconfigjson ]; then
		pull+=(--authfile /etc/celln-fleet/registry/.dockerconfigjson)
	fi
	if [ "${FLEET_PACKAGE_INSECURE:-false}" = true ]; then
		pull+=(--src-tls-verify=false)
	fi
	skopeo copy --override-os linux --override-arch amd64 "${pull[@]}" \
		"docker://$FLEET_PACKAGE_IMAGE" "dir:$staging/oci"
	mkdir "$staging/rootfs"
	python3 -c 'import json, sys
for layer in json.load(open(sys.argv[1]))["layers"]:
    print(layer["digest"].split(":", 1)[1])' "$staging/oci/manifest.json" | while read -r layer; do
		tar -xf "$staging/oci/$layer" -C "$staging/rootfs"
	done
	if [ ! -f "$staging/rootfs/package/package.json" ]; then
		echo "package image must carry the starter package at /package" >&2
		exit 1
	fi
	mv "$staging/rootfs/package" "$package"
fi

admitted="$FLEET_STATE/admitted-$hex"
if [ ! -f "$admitted" ]; then
	"$celln" --root "$root" starter-admit "$package" --package-hash "$FLEET_PACKAGE_HASH"
	: >"$admitted"
fi

# Configure every backend from the same admitted package. Each backend gets
# its own model connection, credential profile (scope for native, scope-name
# otherwise) and configuration directory; the host limits are shared.
backend_names="$(python3 -c 'import json, sys
for b in json.loads(sys.argv[1]):
    print(b["name"])' "$FLEET_BACKENDS")"
for backend in $backend_names; do
	configuration="$FLEET_STATE/configuration-$hex-$backend"
	if [ -f "$configuration/configured.json" ]; then
		continue
	fi
	rm -rf "$configuration"
	output="$FLEET_STATE/.configure-$hex-$backend-$$"
	rm -rf "$output"
	plan="$(mktemp "$FLEET_STATE/.plan-XXXXXX")"
	python3 - "$package" "$FLEET_PACKAGE_HASH" "$FLEET_PRINCIPAL" "$backend" "$output" >"$plan" <<'PY'
import json, os, sys
package, package_hash, principal, name, output = sys.argv[1:]
backend = next(b for b in json.loads(os.environ["FLEET_BACKENDS"]) if b["name"] == name)
scope = os.environ["FLEET_SCOPE"]
plan = {"apiVersion": "celln.native-starter-config/v1", "package": package, "packageHash": package_hash,
        "principal": principal, "credentialFile": backend["credentialFile"], "output": output}
if backend.get("endpoint"):
    # The operator's model route; Celln validates it again before configuring.
    plan["modelConnection"] = {
        "provider": backend["provider"],
        "protocol": backend["protocol"],
        "endpoint": backend["endpoint"],
        "model": backend["model"],
        "credentialProfile": scope if name == "native" else f"{scope}-{name}",
        "allowInsecure": bool(backend.get("allowInsecure", False)),
    }
limits = {}
for key, env in (("leaseSeconds", "FLEET_LIMIT_LEASE_SECONDS"), ("maxTurns", "FLEET_LIMIT_MAX_TURNS"),
                 ("maxModelRequests", "FLEET_LIMIT_MAX_MODEL_REQUESTS"), ("maxOutputTokens", "FLEET_LIMIT_MAX_OUTPUT_TOKENS")):
    value = int(os.environ.get(env, "0") or 0)
    if value > 0:
        limits[key] = value
if limits:
    plan["hostLimits"] = limits
hosts = json.loads(os.environ.get("FLEET_HTTPS_HOSTS") or "[]")
if hosts:
    plan["httpsHosts"] = hosts
print(json.dumps(plan))
PY
	"$celln" --root "$root" starter-configure "$plan" --approve-starter-effects
	rm -f "$plan"
	mv "$output" "$configuration"
done

# A backend is published only once its key has reached this node's kubelet
# (the owner's mount of the same Secret follows within the kubelet's sync
# period), so a parent is never issued on a backend whose key is not there.
for backend in $backend_names; do
	credential="$(python3 -c 'import json, sys
print(next(b["credentialFile"] for b in json.loads(sys.argv[1]) if b["name"] == sys.argv[2]))' "$FLEET_BACKENDS" "$backend")"
	waited=0
	until [ -s "$credential" ]; do
		[ "$waited" -lt 600 ] || { echo "key for backend $backend never reached $credential; publish it with sympozium install" >&2; exit 1; }
		[ $((waited % 60)) -ne 0 ] || echo "waiting for backend $backend key at $credential" >&2
		sleep 5
		waited=$((waited + 5))
	done
done

# Publish the scope's starter configuration, keyed <backend>.<file>. Every
# node derives identical files from the same package, so a later publisher
# only verifies what is there and adds the backends it lacks (a backend added
# to a running scope); an existing backend's files are never rewritten. A
# different package or scope replaces the configuration only once the
# installer has approved exactly this node's package and scope
# (fleet-publish.py decides).
api=https://kubernetes.default.svc
credentials=/var/run/secrets/celln-fleet
resource="$api/api/v1/namespaces/$FLEET_NAMESPACE/configmaps"
# Bodies travel through files: with several backends the configuration
# exceeds what one command-line argument may carry.
body="$(mktemp "$FLEET_STATE/.body-XXXXXX")"
python3 - "$FLEET_CONFIGURATION_CONFIGMAP" "$FLEET_NAMESPACE" "$FLEET_STATE/configuration-$hex" "$FLEET_PACKAGE_HASH" "$NODE_NAME" >"$body" <<'PY'
import json, os, sys
name, namespace, prefix, package_hash, node = sys.argv[1:]
data = {}
for backend in [b["name"] for b in json.loads(os.environ["FLEET_BACKENDS"])]:
    for f in ("catalogue.json", "configured.json", "native-template.json"):
        data[f"{backend}.{f}"] = open(os.path.join(f"{prefix}-{backend}", f)).read()
print(json.dumps({"apiVersion": "v1", "kind": "ConfigMap",
                  "metadata": {"name": name, "namespace": namespace,
                               "labels": {"app.kubernetes.io/part-of": "sympozium"},
                               "annotations": {"celln.sympozium.ai/package": package_hash,
                                               "celln.sympozium.ai/scope": os.environ["FLEET_SCOPE"],
                                               "celln.sympozium.ai/published-by": node}},
                  "data": data}))
PY
published="$(mktemp "$FLEET_STATE/.published-XXXXXX")"
patch="$(mktemp "$FLEET_STATE/.patch-XXXXXX")"
trap 'rm -f "$published" "$body" "$patch"' EXIT
request() { # CONTENT_TYPE selects the body type (a merge patch when extending)
	curl --silent --show-error --cacert "$credentials/ca.crt" \
		--header "Authorization: Bearer $(cat "$credentials/token")" \
		--header "Content-Type: ${CONTENT_TYPE:-application/json}" --output "$published" --write-out '%{http_code}' "$@"
}
# Another node may publish or replace concurrently; a conflict rereads.
for attempt in 1 2 3 4 5; do
	status="$(request "$resource/$FLEET_CONFIGURATION_CONFIGMAP")"
	if [ "$status" = 404 ]; then
		status="$(request --request POST --data-binary @"$body" "$resource")"
		case "$status" in
		201) break ;;
		409) continue ;;
		*)
			echo "publishing starter configuration failed: HTTP $status" >&2
			exit 1
			;;
		esac
	fi
	if [ "$status" != 200 ]; then
		echo "reading published starter configuration failed: HTTP $status" >&2
		exit 1
	fi
	python3 /etc/celln-fleet/fleet-publish.py "$published" "$body" "$patch"
	[ -s "$patch" ] || break
	status="$(CONTENT_TYPE=application/merge-patch+json request --request PATCH --data-binary @"$patch" "$resource/$FLEET_CONFIGURATION_CONFIGMAP")"
	case "$status" in
	200)
		echo "node $NODE_NAME published $(python3 -c 'import json,sys; print(",".join(sorted({k.split(".")[0] for k, v in (json.load(open(sys.argv[1])).get("data") or {}).items() if v is not None})) or "no new backend")' "$patch") for package $FLEET_PACKAGE_HASH"
		break
		;;
	409) continue ;;
	*)
		echo "updating published starter configuration failed: HTTP $status" >&2
		exit 1
		;;
	esac
done
[ "$attempt" -lt 5 ] || [ "$status" = 200 ] || [ "$status" = 201 ] || { echo "published starter configuration kept changing; retrying later" >&2; exit 1; }
echo "node $NODE_NAME prepared $root for package $FLEET_PACKAGE_HASH"
