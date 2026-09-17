#!/usr/bin/env bash
# Publishes this node's Celln cells and parents (fleet-cells.py) to the
# fleet's cells ConfigMap under data.<node>, so the console can show `celln ps`
# per node without exec access or a new listener. It writes only when the
# report changed, and at least every heartbeat so a silent node shows as stale.
set -uo pipefail

: "${FLEET_STATE:?}" "${FLEET_NAMESPACE:?}" "${NODE_NAME:?}" "${FLEET_CELLS_CONFIGMAP:?}"
interval="${FLEET_CELLS_INTERVAL_SECONDS:-2}"
heartbeat="${FLEET_CELLS_HEARTBEAT_SECONDS:-30}"
root="$FLEET_STATE/authority"
api=https://kubernetes.default.svc
credentials=/var/run/secrets/celln-fleet
resource="$api/api/v1/namespaces/$FLEET_NAMESPACE/configmaps"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

request() { # CONTENT_TYPE selects the body type
	curl --silent --show-error --cacert "$credentials/ca.crt" \
		--header "Authorization: Bearer $(cat "$credentials/token")" \
		--header "Content-Type: ${CONTENT_TYPE:-application/json}" --output "$work/response" --write-out '%{http_code}' "$@"
}

last="" published=0
while true; do
	if python3 /etc/celln-fleet/fleet-cells.py snapshot "$root" "$NODE_NAME" >"$work/report" 2>"$work/error"; then
		digest="$(python3 -c 'import json,sys,hashlib; r=json.load(open(sys.argv[1])); r.pop("reportedMs",None); print(hashlib.sha256(json.dumps(r,sort_keys=True).encode()).hexdigest())' "$work/report")"
		now="$(date +%s)"
		if [ "$digest" != "$last" ] || [ $((now - published)) -ge "$heartbeat" ]; then
			python3 - "$NODE_NAME" "$work/report" >"$work/patch" <<'PY'
import json, sys
node, report = sys.argv[1:]
print(json.dumps({"data": {node: open(report).read().strip()}}))
PY
			status="$(CONTENT_TYPE=application/merge-patch+json request --request PATCH --data-binary @"$work/patch" "$resource/$FLEET_CELLS_CONFIGMAP")"
			if [ "$status" = 404 ]; then
				python3 - "$FLEET_CELLS_CONFIGMAP" "$FLEET_NAMESPACE" "$work/patch" >"$work/create" <<'PY'
import json, sys
name, namespace, patch = sys.argv[1:]
body = {"apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"name": name, "namespace": namespace, "labels": {"app.kubernetes.io/part-of": "sympozium"}}}
body.update(json.load(open(patch)))
print(json.dumps(body))
PY
				status="$(request --request POST --data-binary @"$work/create" "$resource")"
			fi
			case "$status" in
			200 | 201)
				last="$digest"
				published="$now"
				;;
			*) echo "publishing cells for $NODE_NAME failed: HTTP $status $(head -c 200 "$work/response")" >&2 ;;
			esac
		fi
	else
		echo "reading cells on $NODE_NAME failed: $(head -c 300 "$work/error")" >&2
	fi
	sleep "$interval"
done
