# Isolated Celln framework manual review

This is an executable, deliberately disposable review deployment. It runs the
real scoped controller, Celln KVM dispatcher/parent, TLS receiver edge, TLS
model gateway, and PostgreSQL in Kubernetes. Its two model origins are a fake,
deterministic TLS fixture. Passing this walkthrough is **not** real-LLM,
multi-tenant, NetworkPolicy/CNI, production hardening, reboot recovery, or
provider acceptance.

The generator does not contact Kubernetes. It refuses mutable image tags,
existing output/state directories, a package with a bad `MANIFEST.blake3`, or
the wrong package/parent contract. It copies only the regenerated package's
signed `authority/` tree into a new private native root. It never merges into an
existing Celln root. Generated public manifests contain no bearer, database
password, TLS private key, CA signing key, or issuer private key.

## Installed framework walkthrough

Review 495 is installed. The deployed controller/native/gateway/PostgreSQL path
has executed `CELLN`, continued an enduring parent with `VIOLET`, refused a third
turn without dispatch, and confirmed parent/descendant cleanup and ledger closure.
This is the **CLI walkthrough**; the new scoped UI code is not deployed here.
See [the installed review record](../design/celln-framework-installed-review.md)
for image identities, actual results, evidence, and remaining acceptance limits.

On framework, select the isolated review context explicitly:

```sh
export KUBECONFIG=/tmp/celln-framework-495.BgIn4ega/kubeconfig
export CELLN_REVIEW_KUBE_CONTEXT=kubernetes-admin@kubernetes
kubectl() { command kubectl --kubeconfig "$KUBECONFIG" --context "$CELLN_REVIEW_KUBE_CONTEXT" "$@"; }
cd /tmp/celln-framework-review-495-manifests
```

The `runs/` manifests below are ready for fresh manual runs. Verification used
separate `verify-*` names. Successful one-shot examples remain available as
`celln-review-a-495/verify-direct-v2` and `verify-model-v3`. The verified enduring
parent was stopped, not left consuming authority. Operator evidence is in
`/tmp/celln-framework-495.BgIn4ega/installed-enduring-verification.log`.

## Generate for review 495

First regenerate the boot-compatible v2 package at the already allocated
private path. Review its public metadata against
`/tmp/celln-framework-package/target/framework-native-package`; do not deploy
the older reference package merely because its shape validates. Then run:

```sh
umask 077
./hack/generate-celln-framework-review.sh \
  --review 495 \
  --image 'REGISTRY/isolated-review@sha256:64_HEX_DIGEST' \
  --postgres-image 'REGISTRY/postgres@sha256:64_HEX_DIGEST' \
  --package /tmp/celln-framework-495.BgIn4ega/native-package-v2 \
  --private-state /tmp/celln-framework-495.BgIn4ega \
  --output /tmp/celln-framework-review-495-manifests
```

The generic review image digest must contain exactly the reviewed
`/usr/local/bin/celln`, `controller` (built from
`cmd/celln-scoped-controller`), `model-gateway`, and `review-provider`
binaries. The PostgreSQL image is separately digest-pinned. Inspect
`GENERATED.txt`, all YAML, the new native authority root, package hashes, image
SBOMs/signatures, certificates, and every generated Secret source file before
proceeding. CA signing keys remain under `deployment-private-495/ca-signing/`;
no generated pod volume references them. Each of the five trust domains has a
different CA and leaf key.

## Preflight and create

Confirm the existing manager still excludes exactly these four namespaces and
that the original `celln-system` workloads are unchanged. The generated script
rechecks exact namespace ownership and requires the tenant label only on A/B.
It refuses *all* pre-existing generated objects—even review-owned ones—and uses
`kubectl create`, never apply/replace/force. A partial create is intentionally
not auto-rolled back; inspect it rather than deleting evidence.

```sh
cd /tmp/celln-framework-review-495-manifests
less GENERATED.txt 10-rbac.yaml 20-platform.yaml 30-components.yaml 40-tenants.yaml
./deploy.sh

kubectl -n celln-review-system-495 rollout status deploy/celln-review-postgres-495 --timeout=180s
kubectl -n celln-review-system-495 rollout status deploy/celln-review-gateway-495 --timeout=180s
kubectl -n celln-review-system-495 rollout status deploy/celln-review-native-495 --timeout=180s
kubectl -n celln-review-system-495 rollout status deploy/celln-review-controller-495 --timeout=180s
kubectl -n celln-review-a-495 rollout status deploy/review-provider-495 --timeout=120s
kubectl -n celln-review-b-495 rollout status deploy/review-provider-495 --timeout=120s
```

The native pod alone is privileged and pinned to node `framework`. It mounts
`/dev/kvm`, the new review root, public trust, its own receiver tokens, and the
exact running host kernel/module directory read-only for native node eligibility.
A bounded init container copies only the named receiver tokens and public inputs
into regular files; native no-symlink checks are not disabled for Kubernetes
projected volumes. PostgreSQL uses a dedicated 2Gi `local-path` PVC, not an
`emptyDir`; never delete that claim while authority or cleanup is unresolved.
Tenant provider pods are non-root, tokenless, read-only, and selected by a
default-deny ingress/egress policy that admits only the review gateway. Whether
the installed CNI enforces this correctly is outside this walkthrough.

The gateway's `readinessNamespaces` are A/B. Its startup SelfSubjectAccessReview
therefore checks named Namespace identity and namespaced `get` on
ModelConnections/Secrets without asking for cluster-wide Secret authority.
Omitting this field preserves the old cluster-wide readiness behavior. The
gateway has no Secret list/watch permission. The controller has no Secret API
permission. Provider credentials are different in A and B and are never
mounted in native or controller.

## Run the bounded cases

Submit one at a time. Status output deliberately omits Secret values:

```sh
kubectl create -f runs/denied.yaml
kubectl -n celln-review-denied-495 get agentrun denied-uppercase -w

kubectl create -f runs/direct.yaml
kubectl -n celln-review-a-495 get agentrun direct-uppercase -w
kubectl -n celln-review-a-495 get agentrun direct-uppercase -o jsonpath='{.status.phase}{"\n"}{.status.result}{"\n"}'

kubectl create -f runs/model-one-shot.yaml
kubectl -n celln-review-a-495 get agentrun model-uppercase -w
kubectl -n celln-review-a-495 get agentrun model-uppercase -o jsonpath='{.status.phase}{"\n"}{.status.result}{"\n"}'

kubectl create -f runs/enduring.yaml
kubectl -n celln-review-b-495 get agentrun enduring-uppercase -w
kubectl -n celln-review-b-495 get agentrun enduring-uppercase -o yaml
```

The parent lease and new operator parent-template ceiling are ten minutes;
the separate child deadline remains 120 seconds. Existing incarnations are never
extended by changing the template. Policy and run ceilings are two turns, four
total model requests, 2048 output tokens, and a 120-second turn deadline. The
gateway accepts at most 512 reserved output tokens per provider request; the
two-turn split gives each turn two requests/1024 tokens. The deterministic
fixture uses two requests per successful turn (tool request then answer), so
the initial turn plus one follow-up consume the original four-request budget.

Create the follow-up from the concrete template after substituting the actual
immutable parent UID:

```sh
run_uid=$(kubectl -n celln-review-b-495 get agentrun enduring-uppercase -o jsonpath='{.metadata.uid}')
sed "s/@@RUN_UID@@/$run_uid/g" runs/turn.yaml | kubectl create -f -
kubectl -n celln-review-b-495 get agentrunturn violet-turn -w
kubectl -n celln-review-b-495 get agentrunturn violet-turn -o yaml
```

To prove the original allowance refuses a third turn, use the supplied second
concrete template with the same parent UID. Its `CellnTurnComplete` condition must
report `BudgetExhausted`, with `cellnScoped.cleanupConfirmed: true` and no native
start attempt. The native `Cancelled` phase represents the confirmed never-started
fence, not a cancelled running VM. There must be no fifth provider request; do not
alter the parent or policy to make it pass:

```sh
sed "s/@@RUN_UID@@/$run_uid/g" runs/third-turn-denied.yaml | kubectl create -f -
kubectl -n celln-review-b-495 get agentrunturn third-turn-must-deny -w
```

## Stop, delete, and preserve cleanup evidence

Deleting the run is the supported parent stop operation. Keep native,
controller, gateway, and PostgreSQL running until authenticated native cleanup
has completed and the controller itself removes the finalizer:

Start a status watch **before** deletion if you want to observe the cleanup
status transition; the controller can confirm cleanup and remove its finalizer
within one reconciliation. Object absence alone is not teardown evidence.

```sh
# In another terminal, with the same explicit kubeconfig/context:
kubectl -n celln-review-b-495 get agentrun enduring-uppercase -w -o custom-columns='NAME:.metadata.name,NATIVE:.status.cellnScoped.nativePhase,CLEANUP:.status.cellnScoped.cleanupConfirmed'

kubectl -n celln-review-b-495 delete agentrun enduring-uppercase --wait=false
kubectl -n celln-review-b-495 get agentrunturns
kubectl -n celln-review-system-495 get configmaps -l sympozium.ai/celln-prepared=true
kubectl -n celln-review-system-495 get configmaps -l sympozium.ai/celln-final=true
```

For an independent operator check, retain the original `receiverId` and `owner`
before deletion and inspect only those fields plus `cleanupConfirmed` in the exact
`native-root-495/scoped/status/<receiverId without sha256:>` record using `sudo jq`.
The PostgreSQL `celln_model_budgets` row for the original run UID must also be
`closed`, with its original request/token ceilings and counters unchanged. The
installed proof observed four requests, 2048 reserved output tokens and 32 actual
fixture output tokens; the third-turn fence charged nothing.

Never patch/remove an AgentRun or AgentRunTurn finalizer. If deletion remains,
retain the new host root, protected ConfigMaps, controller issuer material,
gateway/PostgreSQL state, and pod logs for authenticated recovery. Do not delete
namespaces or unrelated resources. Only after every review run is absent and
native cleanup is confirmed may the coordinator explicitly delete the exact
review-labelled generated objects and decide whether to archive the private
state; this generator intentionally provides no teardown script.
