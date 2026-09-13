#!/usr/bin/env bash
# Additive, isolated UI review installation. Never mounts execution/provider authority.
set -euo pipefail
umask 077
: "${KUBECONFIG:?explicit private kubeconfig required}"
: "${CELLN_REVIEW_KUBE_CONTEXT:?explicit context required}"
: "${CELLN_REVIEW_UI_IMAGE:?immutable image digest required}"
: "${CELLN_REVIEW_UI_HOST:?framework node IP required}"
[[ "$CELLN_REVIEW_UI_IMAGE" == *@sha256:* ]] || { echo 'immutable digest required' >&2; exit 1; }
[[ "$CELLN_REVIEW_UI_HOST" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || exit 1
private=${CELLN_REVIEW_UI_PRIVATE:?new private UI state directory required}
templates=${CELLN_REVIEW_UI_TEMPLATES:?operator run-template directory required}
[[ ! -e "$private" ]] || { echo 'refusing to replace existing UI identity; reuse its manifests for updates' >&2; exit 1; }
mkdir -m 700 "$private"
ns=celln-review-ui-495
k() { command kubectl --kubeconfig "$KUBECONFIG" --context "$CELLN_REVIEW_KUBE_CONTEXT" "$@"; }
# Refuse an existing unrelated namespace rather than adopting it.
if k get namespace "$ns" >/dev/null 2>&1; then echo 'review UI namespace already exists; inspect ownership before updating' >&2; exit 1; fi
openssl rand -hex 32 > "$private/token"
openssl req -x509 -newkey rsa:3072 -nodes -keyout "$private/ca.key" -out "$private/ca.crt" -days 30 -subj '/CN=Celln isolated UX review CA' > /dev/null 2>&1
openssl req -newkey rsa:3072 -nodes -keyout "$private/tls.key" -out "$private/tls.csr" -subj "/CN=$CELLN_REVIEW_UI_HOST" > /dev/null 2>&1
printf 'subjectAltName=IP:%s,IP:127.0.0.1,DNS:localhost,DNS:celln-review-ui-495.celln-review-ui-495.svc\nbasicConstraints=critical,CA:FALSE\nextendedKeyUsage=serverAuth\nkeyUsage=critical,digitalSignature,keyEncipherment\n' "$CELLN_REVIEW_UI_HOST" > "$private/extensions"
openssl x509 -req -in "$private/tls.csr" -CA "$private/ca.crt" -CAkey "$private/ca.key" -CAcreateserial -out "$private/tls.crt" -days 14 -extfile "$private/extensions" > /dev/null 2>&1
export CELLN_REVIEW_UI_PRIVATE="$private"
node <<'NODE' > "$private/public.json"
const fs=require('fs');const ns='celln-review-ui-495';const name=ns;
const items=[{apiVersion:'v1',kind:'Namespace',metadata:{name:ns,labels:{'sympozium.ai/review-owner':'celln-framework-495','pod-security.kubernetes.io/enforce':'restricted'}}},{apiVersion:'v1',kind:'ServiceAccount',metadata:{name,namespace:ns}}];
items.push({apiVersion:'rbac.authorization.k8s.io/v1',kind:'Role',metadata:{name,namespace:ns},rules:[{apiGroups:[''],resources:['configmaps'],verbs:['get','create']}]});
items.push({apiVersion:'rbac.authorization.k8s.io/v1',kind:'RoleBinding',metadata:{name,namespace:ns},subjects:[{kind:'ServiceAccount',name,namespace:ns}],roleRef:{apiGroup:'rbac.authorization.k8s.io',kind:'Role',name}});
for(const tenant of ['celln-review-a-495','celln-review-b-495']) {
 items.push({apiVersion:'rbac.authorization.k8s.io/v1',kind:'Role',metadata:{name,namespace:tenant},rules:[{apiGroups:['sympozium.ai'],resources:['agentruns'],verbs:['get','list','create','patch','delete']},{apiGroups:['sympozium.ai'],resources:['agentrunturns'],verbs:['get','list','create','patch','update']}]});
 items.push({apiVersion:'rbac.authorization.k8s.io/v1',kind:'RoleBinding',metadata:{name,namespace:tenant},subjects:[{kind:'ServiceAccount',name,namespace:ns}],roleRef:{apiGroup:'rbac.authorization.k8s.io',kind:'Role',name}});
}
items.push({apiVersion:'apps/v1',kind:'Deployment',metadata:{name,namespace:ns},spec:{replicas:1,selector:{matchLabels:{app:name}},template:{metadata:{labels:{app:name}},spec:{serviceAccountName:name,securityContext:{runAsNonRoot:true,runAsUser:65532,runAsGroup:65532,fsGroup:65532,seccompProfile:{type:'RuntimeDefault'}},containers:[{name:'ui',image:process.env.CELLN_REVIEW_UI_IMAGE,imagePullPolicy:'IfNotPresent',ports:[{containerPort:8443,name:'https'}],securityContext:{allowPrivilegeEscalation:false,readOnlyRootFilesystem:true,capabilities:{drop:['ALL']}},resources:{requests:{cpu:'100m',memory:'128Mi'},limits:{cpu:'1',memory:'384Mi'}},readinessProbe:{httpGet:{path:'/readyz',port:8443,scheme:'HTTPS'}},volumeMounts:[{name:'tls',mountPath:'/tls',readOnly:true},{name:'auth',mountPath:'/auth',readOnly:true},{name:'templates',mountPath:'/templates',readOnly:true}]}],volumes:[{name:'tls',secret:{secretName:name+'-tls',defaultMode:288}},{name:'auth',secret:{secretName:name+'-auth',defaultMode:288}},{name:'templates',configMap:{name:name+'-templates'}}]}}}});
items.push({apiVersion:'v1',kind:'Service',metadata:{name,namespace:ns},spec:{type:'NodePort',selector:{app:name},ports:[{name:'https',port:443,targetPort:8443,nodePort:30495}]}});
console.log(JSON.stringify({apiVersion:'v1',kind:'List',items},null,2));
NODE
# Namespace first, then private projected inputs. Secret values never enter logs.
k create namespace "$ns" >/dev/null
k label namespace "$ns" sympozium.ai/review-owner=celln-framework-495 pod-security.kubernetes.io/enforce=restricted >/dev/null
k -n "$ns" create secret generic "$ns-auth" --from-file=token="$private/token" >/dev/null
k -n "$ns" create secret tls "$ns-tls" --cert="$private/tls.crt" --key="$private/tls.key" >/dev/null
k -n "$ns" create configmap "$ns-templates" --from-file=direct.yaml="$templates/direct.yaml" --from-file=model-one-shot.yaml="$templates/model-one-shot.yaml" --from-file=enduring.yaml="$templates/enduring.yaml" >/dev/null
k apply -f "$private/public.json" >/dev/null
k -n "$ns" rollout status deployment/"$ns" --timeout=120s
printf 'Review UI: https://%s:30495\nPrivate login token: %s/token\nReview CA: %s/ca.crt\n' "$CELLN_REVIEW_UI_HOST" "$private" "$private"
