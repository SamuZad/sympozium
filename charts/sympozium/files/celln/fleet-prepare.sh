#!/usr/bin/env bash
# Prepares one KVM node's native Celln authority root from an operator-signed
# starter package and publishes the resulting starter configuration once per
# scope. Every step is idempotent for the same package hash. Nothing here
# issues a run, reads the model credential or starts a guest outside the
# package's own admission checks.
set -euo pipefail

: "${FLEET_STATE:?}" "${FLEET_PACKAGE_IMAGE:?}" "${FLEET_PACKAGE_HASH:?}" "${FLEET_PUBLISHER:?}"
: "${FLEET_PRINCIPAL:?}" "${FLEET_MODEL_CREDENTIAL_FILE:?}" "${FLEET_PARENT_CLIENTS:?}"
: "${FLEET_CONFIGURATION_CONFIGMAP:?}" "${FLEET_NAMESPACE:?}" "${NODE_NAME:?}"

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

configuration="$FLEET_STATE/configuration-$hex"
if [ ! -f "$configuration/configured.json" ]; then
	rm -rf "$configuration"
	output="$FLEET_STATE/.configure-$hex-$$"
	rm -rf "$output"
	plan="$(mktemp "$FLEET_STATE/.plan-XXXXXX")"
	python3 - "$package" "$FLEET_PACKAGE_HASH" "$FLEET_PRINCIPAL" "$FLEET_MODEL_CREDENTIAL_FILE" "$output" >"$plan" <<'PY'
import json, os, sys
package, package_hash, principal, credential, output = sys.argv[1:]
plan = {"apiVersion": "celln.native-starter-config/v1", "package": package, "packageHash": package_hash,
        "principal": principal, "credentialFile": credential, "output": output}
endpoint = os.environ.get("FLEET_MODEL_ENDPOINT", "")
if endpoint:
    # The operator's model route; Celln validates it again before configuring.
    plan["modelConnection"] = {
        "provider": os.environ["FLEET_MODEL_PROVIDER"],
        "protocol": os.environ["FLEET_MODEL_PROTOCOL"],
        "endpoint": endpoint,
        "model": os.environ["FLEET_MODEL_NAME"],
        "credentialProfile": os.environ["FLEET_SCOPE"],
        "allowInsecure": os.environ.get("FLEET_MODEL_ALLOW_INSECURE", "false") == "true",
    }
limits = {}
for key, env in (("leaseSeconds", "FLEET_LIMIT_LEASE_SECONDS"), ("maxTurns", "FLEET_LIMIT_MAX_TURNS"),
                 ("maxModelRequests", "FLEET_LIMIT_MAX_MODEL_REQUESTS"), ("maxOutputTokens", "FLEET_LIMIT_MAX_OUTPUT_TOKENS")):
    value = int(os.environ.get(env, "0") or 0)
    if value > 0:
        limits[key] = value
if limits:
    plan["hostLimits"] = limits
print(json.dumps(plan))
PY
	"$celln" --root "$root" starter-configure "$plan" --approve-starter-effects
	rm -f "$plan"
	mv "$output" "$configuration"
fi

# Publish the scope's starter configuration exactly once. Every node derives
# identical files from the same package, so a second publisher only verifies.
api=https://kubernetes.default.svc
credentials=/var/run/secrets/celln-fleet
resource="$api/api/v1/namespaces/$FLEET_NAMESPACE/configmaps"
body="$(python3 - "$FLEET_CONFIGURATION_CONFIGMAP" "$FLEET_NAMESPACE" "$configuration" "$FLEET_PACKAGE_HASH" "$NODE_NAME" <<'PY'
import json, os, sys
name, namespace, directory, package_hash, node = sys.argv[1:]
data = {f: open(os.path.join(directory, f)).read() for f in ("catalogue.json", "configured.json", "native-template.json")}
print(json.dumps({"apiVersion": "v1", "kind": "ConfigMap",
                  "metadata": {"name": name, "namespace": namespace,
                               "labels": {"app.kubernetes.io/part-of": "sympozium"},
                               "annotations": {"celln.sympozium.ai/package": package_hash, "celln.sympozium.ai/published-by": node}},
                  "data": data}))
PY
)"
published="$(mktemp "$FLEET_STATE/.published-XXXXXX")"
trap 'rm -f "$published"' EXIT
request() {
	curl --silent --show-error --cacert "$credentials/ca.crt" \
		--header "Authorization: Bearer $(cat "$credentials/token")" \
		--header "Content-Type: application/json" --output "$published" --write-out '%{http_code}' "$@"
}
status="$(request "$resource/$FLEET_CONFIGURATION_CONFIGMAP")"
if [ "$status" = 404 ]; then
	status="$(request --request POST --data-binary "$body" "$resource")"
	case "$status" in
	201) ;;
	409) status="$(request "$resource/$FLEET_CONFIGURATION_CONFIGMAP")" ;;
	*)
		echo "publishing starter configuration failed: HTTP $status" >&2
		exit 1
		;;
	esac
fi
if [ "$status" != 200 ] && [ "$status" != 201 ]; then
	echo "reading published starter configuration failed: HTTP $status" >&2
	exit 1
fi
python3 - "$published" "$body" <<'PY'
import json, sys
existing = json.load(open(sys.argv[1])).get("data", {})
ours = json.loads(sys.argv[2])["data"]
if existing != ours:
    sys.exit("published starter configuration differs from this node's package; one scope carries exactly one package")
PY
echo "node $NODE_NAME prepared $root for package $FLEET_PACKAGE_HASH"
