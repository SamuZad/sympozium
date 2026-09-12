#!/usr/bin/env bash
set -euo pipefail
umask 077

usage() {
  cat <<'EOF'
Usage: hack/generate-celln-framework-review.sh \
  --review 495 \
  --image registry.example/sympozium-review@sha256:<64 hex> \
  --postgres-image registry.example/postgres@sha256:<64 hex> \
  --package /tmp/celln-framework-495.BgIn4ega/native-package-v2 \
  --private-state /tmp/celln-framework-495.BgIn4ega \
  --output /absolute/new/public-output

Generates private keys/files and public, secret-free manifests. It performs no
Kubernetes API call and refuses an existing output or native state directory.
EOF
}

review= image= postgres_image= package= private_state= output=
while (($#)); do
  case "$1" in
    --review) review=${2-}; shift 2 ;;
    --image) image=${2-}; shift 2 ;;
    --postgres-image) postgres_image=${2-}; shift 2 ;;
    --package) package=${2-}; shift 2 ;;
    --private-state) private_state=${2-}; shift 2 ;;
    --output) output=${2-}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ $review =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || { echo "invalid DNS-safe review id" >&2; exit 2; }
(( ${#review} <= 40 )) || { echo "review id is too long for generated DNS names" >&2; exit 2; }
digest_re='^[A-Za-z0-9._/:+-]+@sha256:[0-9a-f]{64}$'
[[ $image =~ $digest_re ]] || { echo "--image must be an immutable sha256 digest reference" >&2; exit 2; }
[[ $postgres_image =~ $digest_re ]] || { echo "--postgres-image must be an immutable sha256 digest reference" >&2; exit 2; }
[[ $package = /* && -d $package ]] || { echo "--package must be an absolute existing directory" >&2; exit 2; }
[[ $private_state = /* && -d $private_state ]] || { echo "--private-state must be an absolute existing directory" >&2; exit 2; }
[[ $output = /* && ! -e $output ]] || { echo "--output must be an absolute nonexistent path" >&2; exit 2; }
for path in "$package" "$private_state" "$output"; do
  [[ $path =~ ^/[A-Za-z0-9._/-]+$ ]] || { echo "paths must use only slash, letters, digits, dot, underscore, and hyphen" >&2; exit 2; }
done
for tool in jq kubectl openssl base64 install cp awk sed; do
  command -v "$tool" >/dev/null || { echo "required tool missing: $tool" >&2; exit 2; }
done

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
native_root="$private_state/native-root-$review"
private="$private_state/deployment-private-$review"
[[ ! -e $native_root && ! -e $private ]] || { echo "refusing existing private review state" >&2; exit 1; }
for f in package.json resources.yaml parent-request.json MANIFEST.blake3 authority/trusted-motes.json authority/trusted-closures.json; do
  [[ -f $package/$f && ! -L $package/$f ]] || { echo "package member missing or symlinked: $f" >&2; exit 1; }
done
if command -v b3sum >/dev/null; then
  (cd "$package" && b3sum --quiet -c MANIFEST.blake3) || { echo "package MANIFEST.blake3 verification failed" >&2; exit 1; }
else
  command -v go >/dev/null || { echo "package verification requires b3sum or go" >&2; exit 2; }
  GOCACHE=${GOCACHE:-/tmp/sympozium-review-go-cache} go run "$repo/hack/celln-framework-package-verify" "$package" || { echo "package MANIFEST.blake3 verification failed" >&2; exit 1; }
fi
jq -e '.apiVersion == "celln.framework-native-package/v1" and .credentialsIncluded == false and .privateSigningMaterialExported == false and .resources.file == "resources.yaml" and .resources.parentRequest == "parent-request.json" and (.resources.profiles|length) == 2 and (.resources.tools|length) == 1' "$package/package.json" >/dev/null
jq -e '.apiVersion == "celln.native-scoped-parent-artifact/v1" and .operatorMetadataOnly == true and .runAuthority == false and .artifact.entryPoint == "/parent" and .artifact.platform == "linux/amd64" and .artifact.lane == "agent" and .limits.workspace == "none" and (.limits.egress|length) == 0 and .limits.memoryBytes > 0' "$package/parent-request.json" >/dev/null

mkdir -m 0700 "$output" "$output/runs" "$private" "$private/ca-signing" "$private/leaf" "$native_root"
cp -a "$package/authority/." "$native_root/"
chmod 0700 "$native_root"

# Parse each public package resource using kubectl's local YAML decoder only.
split_dir="$private/package-resources"
mkdir -m 0700 "$split_dir"
awk -v d="$split_dir" 'BEGIN{n=0} /^---[[:space:]]*$/{n++;next} {print > (d "/" n ".yaml")}' "$package/resources.yaml"
for f in "$split_dir"/*.yaml; do
  kubectl create --dry-run=client --validate=false -f "$f" -o json >"$f.json"
done
one_json=$(jq -c 'select(.kind=="CellnRuntimeProfile" and .metadata.name=="celln-json-one-shot")' "$split_dir"/*.json)
end_json=$(jq -c 'select(.kind=="CellnRuntimeProfile" and .metadata.name=="celln-json-enduring")' "$split_dir"/*.json)
tool_json=$(jq -c 'select(.kind=="ClusterCellnTool" and .metadata.name=="uppercase")' "$split_dir"/*.json)
[[ -n $one_json && -n $end_json && -n $tool_json ]] || { echo "package lacks the fixed runtime/tool catalogue" >&2; exit 1; }
one_spec=$(jq -c '.spec' <<<"$one_json"); end_spec=$(jq -c '.spec' <<<"$end_json"); tool_spec=$(jq -c '.spec' <<<"$tool_json")
one_revision=$(jq -r '.spec.revision' <<<"$one_json"); end_revision=$(jq -r '.spec.revision' <<<"$end_json"); tool_revision=$(jq -r '.spec.revision' <<<"$tool_json")
one_profile="celln-json-one-shot-review-$review"
end_profile="celln-json-enduring-review-$review"
tool_name="uppercase-review-$review"

# Derive exactly the same bounded wrapper as the live helper, using only the
# package's actual parent artifact fields. No descriptor is synthesized.
parent_template=$(jq -c '{apiVersion:"celln.scoped-parent-template/v1",request:{apiVersion:"celln.dev/v1alpha1",id:"$parent",workload:{id:"$parent",caller:"$principal"},mote:.artifact.mote,tools:[{alias:.artifact.entryPoint,hash:.artifact.executable.hash,closure:.artifact.closure}],invocation:{alias:.artifact.entryPoint,args:[]},capabilities:{workspace:.limits.workspace,egress:.limits.egress,timeoutMs:120000,memoryBytes:.limits.memoryBytes,outputBytes:65536},execution:{lane:.artifact.lane,requireHardwareIsolation:true}},reservedMemoryBytes:(.limits.memoryBytes*4)}' "$package/parent-request.json")

random_token() { openssl rand -hex 32; }
make_ca() {
  local name=$1 dir="$private/ca-signing/$1"
  mkdir -m 0700 "$dir"
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$dir/ca.key" >/dev/null 2>&1
  openssl req -new -x509 -sha256 -days 30 -key "$dir/ca.key" -subj "/CN=celln-review-$review-$name-ca" -out "$dir/ca.crt" >/dev/null 2>&1
}
make_leaf() {
  local domain=$1 name=$2 dns=$3 dir="$private/leaf/$2"
  mkdir -m 0700 "$dir"
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$dir/tls.key" >/dev/null 2>&1
  openssl req -new -key "$dir/tls.key" -subj "/CN=$dns" -out "$dir/tls.csr" >/dev/null 2>&1
  printf 'subjectAltName=DNS:%s\nextendedKeyUsage=serverAuth\n' "$dns" >"$dir/ext.cnf"
  openssl x509 -req -sha256 -days 14 -in "$dir/tls.csr" -CA "$private/ca-signing/$domain/ca.crt" -CAkey "$private/ca-signing/$domain/ca.key" -CAcreateserial -extfile "$dir/ext.cnf" -out "$dir/tls.crt" >/dev/null 2>&1
  rm -f "$dir/tls.csr" "$dir/ext.cnf"
}
for domain in receiver gateway postgres provider-a provider-b; do make_ca "$domain"; done
system_ns="celln-review-system-$review"; a_ns="celln-review-a-$review"; b_ns="celln-review-b-$review"; denied_ns="celln-review-denied-$review"
make_leaf receiver receiver "celln-receiver-$review.$system_ns.svc"
make_leaf gateway gateway "celln-model-gateway-$review.$system_ns.svc"
make_leaf postgres postgres "celln-postgres-$review.$system_ns.svc"
make_leaf provider-a provider-a "review-provider-$review.$a_ns.svc"
make_leaf provider-b provider-b "review-provider-$review.$b_ns.svc"

openssl genpkey -algorithm ED25519 -out "$private/issuer-key.pem" >/dev/null 2>&1
issuer_x=$(openssl pkey -in "$private/issuer-key.pem" -pubout -outform DER 2>/dev/null | tail -c 32 | base64 | tr -d '\n=' | tr '+/' '-_')
jwks=$(jq -cn --arg x "$issuer_x" --arg kid "celln-review-$review-v1" '{keys:[{kty:"OKP",crv:"Ed25519",use:"sig",alg:"EdDSA",kid:$kid,x:$x}]}')
printf '%s\n' "$(random_token)" >"$private/native-token"
printf '%s\n' "$(random_token)" >"$private/scoped-token"
printf '%s\n' "$(random_token)" >"$private/gateway-token"
printf '%s\n' "$(random_token)" >"$private/provider-a-token"
printf '%s\n' "$(random_token)" >"$private/provider-b-token"
printf '%s\n' "$(random_token)" >"$private/postgres-password"
db_password=$(tr -d '\n' <"$private/postgres-password")
printf 'postgresql://celln_review:%s@celln-postgres-%s.%s.svc:5432/celln_review?sslmode=verify-full&sslrootcert=/public/postgres-ca.pem\n' "$db_password" "$review" "$system_ns" >"$private/database-url"

receiver_url="https://celln-receiver-$review.$system_ns.svc:8443"
gateway_url="https://celln-model-gateway-$review.$system_ns.svc:8443"
controller_config=$(jq -cn --arg cluster "celln-review-$review" --arg prep "$system_ns" --arg receiver "$receiver_url" --arg gateway "$gateway_url" --arg kid "celln-review-$review-v1" '{clusterId:$cluster,preparationNamespace:$prep,receiver:{url:$receiver,caFile:"/operator/receiver-ca.pem",tokenFile:"/operator/scoped-token"},gateway:{url:$gateway,caFile:"/operator/gateway-ca.pem",tokenFile:"/operator/gateway-token"},issuer:{name:"sympozium-control-plane",keyId:$kid,privateKeyFile:"/operator/issuer-key.pem"}}')
printf '%s\n' "$controller_config" >"$private/controller.json"
provider_a="https://review-provider-$review.$a_ns.svc:8443"
provider_b="https://review-provider-$review.$b_ns.svc:8443"
gateway_config=$(jq -cn --arg cluster "celln-review-$review" --arg a "$a_ns" --arg b "$b_ns" --arg pa "$provider_a" --arg pb "$provider_b" '{clusterId:$cluster,issuer:"sympozium-control-plane",listen:":8443",tlsCertificateFile:"/tls/tls.crt",tlsKeyFile:"/tls/tls.key",verificationKeysFile:"/public/issuer-jwks.json",registrationTokenFile:"/operator/gateway-token",databaseUrlFile:"/operator/database-url",privateOrigins:[$pa,$pb],providerCaFile:"/public/provider-ca.pem",readinessNamespaces:[$a,$b]}')

sed_value() { printf '%s' "$1" | sed 's/[\\&|]/\\&/g'; }
render_simple() {
  local src=$1 dst=$2
  cp "$src" "$dst"
  shift 2
  while (($#)); do
    local token=$1 value=$2; shift 2
    sed -i "s|@@$token@@|$(sed_value "$value")|g" "$dst"
  done
}
common=(REVIEW "$review" ONE_PROFILE "$one_profile" END_PROFILE "$end_profile" TOOL "$tool_name" ONE_REVISION "$one_revision" END_REVISION "$end_revision" TOOL_REVISION "$tool_revision")
render_simple "$repo/config/manual-review/celln-framework/platform.yaml.tmpl" "$output/20-platform.yaml" "${common[@]}" ONE_SPEC "$one_spec" END_SPEC "$end_spec" TOOL_SPEC "$tool_spec"
render_simple "$repo/config/manual-review/celln-framework/tenants.yaml.tmpl" "$output/40-tenants.yaml" "${common[@]}"
for name in direct model-one-shot enduring denied turn third-turn-denied; do render_simple "$repo/config/manual-review/celln-framework/runs/$name.yaml.tmpl" "$output/runs/$name.yaml" "${common[@]}"; done

controller_roles="$private/controller-roles.yaml"; : >"$controller_roles"
for ns in "$a_ns" "$b_ns" "$denied_ns"; do
  cat >>"$controller_roles" <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: celln-review-controller-$review, namespace: $ns, labels: {sympozium.ai/celln-review: "$review"}}
rules:
  - apiGroups: [sympozium.ai]
    resources: [agents, agentruntimes]
    verbs: [get, list, watch]
  - apiGroups: [sympozium.ai]
    resources: [agentruns, agentrunturns]
    verbs: [get, list, watch, update, patch]
  - apiGroups: [sympozium.ai]
    resources: [modelconnections]
    verbs: [get]
  - apiGroups: [sympozium.ai]
    resources: [agentruns/status, agentrunturns/status]
    verbs: [get, update, patch]
  - apiGroups: [sympozium.ai]
    resources: [agentruns/finalizers, agentrunturns/finalizers]
    verbs: [update, patch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: celln-review-controller-$review, namespace: $ns, labels: {sympozium.ai/celln-review: "$review"}}
subjects: [{kind: ServiceAccount, name: celln-review-controller-$review, namespace: $system_ns}]
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: celln-review-controller-$review}
---
EOF
done
gateway_roles="$private/gateway-roles.yaml"; : >"$gateway_roles"
for ns in "$a_ns" "$b_ns"; do
  cat >>"$gateway_roles" <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: celln-review-gateway-$review, namespace: $ns, labels: {sympozium.ai/celln-review: "$review"}}
rules:
  - apiGroups: [sympozium.ai]
    resources: [modelconnections]
    verbs: [get]
  - apiGroups: [""]
    resources: [secrets]
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: celln-review-gateway-$review, namespace: $ns, labels: {sympozium.ai/celln-review: "$review"}}
subjects: [{kind: ServiceAccount, name: celln-review-gateway-$review, namespace: $system_ns}]
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: celln-review-gateway-$review}
---
EOF
done
render_simple "$repo/config/manual-review/celln-framework/rbac.yaml.tmpl" "$output/10-rbac.stage" "${common[@]}"
awk -v cr="$controller_roles" -v gr="$gateway_roles" '{if($0=="@@CONTROLLER_ROLES@@"){while((getline x < cr)>0)print x;close(cr)}else if($0=="@@GATEWAY_ROLES@@"){while((getline x < gr)>0)print x;close(gr)}else print}' "$output/10-rbac.stage" >"$output/10-rbac.yaml"
rm "$output/10-rbac.stage"

providers="$private/providers.yaml"; : >"$providers"
for side in a b; do
  ns_var="${side}_ns"; ns=${!ns_var}
  cat >>"$providers" <<EOF
apiVersion: v1
kind: Service
metadata: {name: review-provider-$review, namespace: $ns, labels: {sympozium.ai/celln-review: "$review"}}
spec: {selector: {app: review-provider-$review}, ports: [{name: https, port: 8443, targetPort: 8443}]}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: review-provider-$review, namespace: $ns, labels: {sympozium.ai/celln-review: "$review"}}
spec:
  replicas: 1
  selector: {matchLabels: {app: review-provider-$review}}
  template:
    metadata: {labels: {app: review-provider-$review, sympozium.ai/celln-review: "$review"}}
    spec:
      automountServiceAccountToken: false
      securityContext: {runAsUser: 65532, runAsGroup: 65532, fsGroup: 65532}
      containers:
        - name: fixture
          image: $image
          command: ["/usr/local/bin/review-provider"]
          args: ["--listen", ":8443", "--cert", "/tls/tls.crt", "--key", "/tls/tls.key", "--token-file", "/credential/OPENAI_API_KEY"]
          ports: [{name: https, containerPort: 8443}]
          securityContext: {runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
          volumeMounts: [{name: tls, mountPath: /tls, readOnly: true}, {name: credential, mountPath: /credential, readOnly: true}]
      volumes: [{name: tls, secret: {secretName: review-provider-tls-$review, defaultMode: 288}}, {name: credential, secret: {secretName: review-provider-credential, defaultMode: 288}}]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: review-provider-isolation-$review, namespace: $ns, labels: {sympozium.ai/celln-review: "$review"}}
spec:
  podSelector: {matchLabels: {app: review-provider-$review}}
  policyTypes: [Ingress, Egress]
  ingress:
    - from:
        - namespaceSelector: {matchLabels: {sympozium.ai/celln-review: "$review"}}
          podSelector: {matchLabels: {app: celln-review-gateway-$review}}
      ports: [{protocol: TCP, port: 8443}]
  egress: []
---
EOF
done

render_simple "$repo/config/manual-review/celln-framework/components.yaml.tmpl" "$output/30-components.stage" REVIEW "$review" IMAGE "$image" POSTGRES_IMAGE "$postgres_image" NATIVE_ROOT "$native_root" JWKS "$jwks" PARENT_TEMPLATE "$parent_template" GATEWAY_CONFIG "$gateway_config"
expand="$private/expand.awk"
cat >"$expand" <<'AWK'
function emit(path,indent, line) { while ((getline line < path)>0) print indent line; close(path) }
$0=="@@RECEIVER_CA@@" {emit(receiver,"    ");next}
$0=="@@GATEWAY_CA@@" {emit(gateway,"    ");next}
$0=="@@PROVIDER_CA@@" {emit(providers_ca,"    ");next}
$0=="@@POSTGRES_CA@@" {emit(postgres,"    ");next}
$0=="@@MIGRATION_002@@" {emit(migration2,"    ");next}
$0=="@@MIGRATION_003@@" {emit(migration3,"    ");next}
$0=="@@PROVIDERS@@" {emit(providers,"");next}
{print}
AWK
cat "$private/ca-signing/provider-a/ca.crt" "$private/ca-signing/provider-b/ca.crt" >"$private/provider-ca.pem"
awk -f "$expand" -v receiver="$private/ca-signing/receiver/ca.crt" -v gateway="$private/ca-signing/gateway/ca.crt" -v providers_ca="$private/provider-ca.pem" -v postgres="$private/ca-signing/postgres/ca.crt" -v migration2="$repo/migrations/002_celln_model_budget.sql" -v migration3="$repo/migrations/003_celln_model_gateway.sql" -v providers="$providers" "$output/30-components.stage" >"$output/30-components.yaml"
rm "$output/30-components.stage"

cat >"$private/create-secrets.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail
umask 077
kubectl create secret generic celln-review-native-$review -n $system_ns --from-file=native-token='$private/native-token' --from-file=scoped-token='$private/scoped-token'
kubectl create secret tls celln-review-receiver-tls-$review -n $system_ns --cert='$private/leaf/receiver/tls.crt' --key='$private/leaf/receiver/tls.key'
kubectl create secret generic celln-review-controller-$review -n $system_ns --from-file=controller.json='$private/controller.json' --from-file=issuer-key.pem='$private/issuer-key.pem' --from-file=scoped-token='$private/scoped-token' --from-file=gateway-token='$private/gateway-token' --from-file=receiver-ca.pem='$private/ca-signing/receiver/ca.crt' --from-file=gateway-ca.pem='$private/ca-signing/gateway/ca.crt'
kubectl create secret tls celln-review-gateway-tls-$review -n $system_ns --cert='$private/leaf/gateway/tls.crt' --key='$private/leaf/gateway/tls.key'
kubectl create secret generic celln-review-gateway-$review -n $system_ns --from-file=gateway-token='$private/gateway-token' --from-file=database-url='$private/database-url'
kubectl create secret tls celln-review-postgres-tls-$review -n $system_ns --cert='$private/leaf/postgres/tls.crt' --key='$private/leaf/postgres/tls.key'
kubectl create secret generic celln-review-postgres-$review -n $system_ns --from-file=postgres-password='$private/postgres-password'
kubectl create secret tls review-provider-tls-$review -n $a_ns --cert='$private/leaf/provider-a/tls.crt' --key='$private/leaf/provider-a/tls.key'
kubectl create secret generic review-provider-credential -n $a_ns --from-file=OPENAI_API_KEY='$private/provider-a-token'
kubectl create secret tls review-provider-tls-$review -n $b_ns --cert='$private/leaf/provider-b/tls.crt' --key='$private/leaf/provider-b/tls.key'
kubectl create secret generic review-provider-credential -n $b_ns --from-file=OPENAI_API_KEY='$private/provider-b-token'
EOF
chmod 0700 "$private/create-secrets.sh"

cat >"$output/deploy.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail
for ns in $a_ns $b_ns $denied_ns $system_ns; do
  test "\$(kubectl get namespace "\$ns" -o jsonpath='{.metadata.labels.sympozium\.ai/celln-review}')" = '$review' || { echo "namespace \$ns lacks exact review ownership" >&2; exit 1; }
done
test "\$(kubectl get namespace $a_ns -o jsonpath='{.metadata.labels.sympozium\.ai/celln-review-tenant}')" = enabled
test "\$(kubectl get namespace $b_ns -o jsonpath='{.metadata.labels.sympozium\.ai/celln-review-tenant}')" = enabled
test -z "\$(kubectl get namespace $denied_ns -o jsonpath='{.metadata.labels.sympozium\.ai/celln-review-tenant}')"
test -z "\$(kubectl get namespace $system_ns -o jsonpath='{.metadata.labels.sympozium\.ai/celln-review-tenant}')"
for file in 10-rbac.yaml 20-platform.yaml 30-components.yaml 40-tenants.yaml; do
  if kubectl get -f '$output/'"\$file" --ignore-not-found -o name | grep -q .; then
    echo "refusing pre-existing generated resource from \$file (no apply/replace/force)" >&2; exit 1
  fi
done
# Exact-name Secret preflight; no Secret list/watch operation is used.
for item in '$system_ns/celln-review-native-$review' '$system_ns/celln-review-receiver-tls-$review' '$system_ns/celln-review-controller-$review' '$system_ns/celln-review-gateway-tls-$review' '$system_ns/celln-review-gateway-$review' '$system_ns/celln-review-postgres-tls-$review' '$system_ns/celln-review-postgres-$review' '$a_ns/review-provider-tls-$review' '$a_ns/review-provider-credential' '$b_ns/review-provider-tls-$review' '$b_ns/review-provider-credential'; do
  ns=\${item%%/*}; name=\${item#*/}; ! kubectl get secret "\$name" -n "\$ns" >/dev/null 2>&1 || { echo "refusing pre-existing Secret \$item" >&2; exit 1; }
done
kubectl create -f '$output/10-rbac.yaml'
'$private/create-secrets.sh'
kubectl create -f '$output/20-platform.yaml'
kubectl create -f '$output/30-components.yaml'
kubectl create -f '$output/40-tenants.yaml'
EOF
chmod 0755 "$output/deploy.sh"

cat >"$output/GENERATED.txt" <<EOF
review=$review
image=$image
postgresImage=$postgres_image
package=$package
nativeRoot=$native_root
privateFiles=$private
oneShot=$one_profile@$one_revision
enduring=$end_profile@$end_revision
tool=$tool_name@$tool_revision
scope=fake deterministic TLS provider; not a real LLM or tenant/CNI acceptance
EOF
chmod -R go-rwx "$private" "$native_root"
chmod 0755 "$output/deploy.sh"
echo "Generated secret-free manifests in $output"
echo "Private material retained at $private; CA signing keys are never referenced by a pod"
echo "Native authority root created at $native_root"
