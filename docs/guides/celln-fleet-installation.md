# Celln fleet installation — enduring parents on every KVM node

The fleet is the multi-node form of the native Celln plane. One reviewed
starter package and one scope are installed once; after that, joining a node
is a single label and no namespace needs its own installation. It replaces
the single pinned dispatcher of the
[native installation](celln-native-installation.md), which remains the
small-cluster path.

What runs where:

| Component | Placement | Role |
| --- | --- | --- |
| `celln-node` DaemonSet | every node labeled `celln.dev/kvm=true` | init step prepares `/var/lib/sympozium-celln/<scope>` from the package; main container is the dispatcher serving `/v1/executions` and `/v1/parents` |
| `celln-router` | any node | gateway; discovers owners through the headless `celln-node` Service and binds each parent to the owner that issued it |
| controller | any node | issues parents through the gateway (`POST /v1/parents/provision`); keeps only its own journal/approvals on a `ReadWriteOnce` claim |

Nothing mounts a host path outside `/var/lib/sympozium-celln/<scope>`, the
controller mounts no host state at all, and no node is ever named in values.

## Trust and credentials

- **Publisher.** The package is signed with an operator seed. Its publisher key
  (from `celln starter-inspect`) is approved once in values; every node writes
  it into `trusted-closures.json` before admission. The package cannot install
  its own trust.
- **Parent principal.** `sympozium install --celln-fleet` generates one bearer
  credential for the gateway (`celln-router-parent`) and publishes only its
  BLAKE3 hash as the client policy every node installs. Owners never see the
  token; the controller never sees the policy. Rerunning the installer verifies
  the pair and refuses to rotate it by replacement.
- **Model credential.** The model profile references an absolute file path.
  In fleet mode that file is a Secret (`celln-fleet-model-credential`) mounted
  read-only into each dispatcher; guests and the controller cannot read it.
  This keeps the key inside the cluster trust boundary until the dedicated
  model gateway (#502) is attached to native parents; treat it as interim.
- **Configuration publication.** Only the DaemonSet's init step holds a
  projected ServiceAccount token, scoped to creating and reading one ConfigMap
  in `celln-system`. The dispatcher container has no API credential.

## Prerequisites

1. Nodes with `/dev/kvm`, a readable kernel under `/boot` with its
   `/lib/modules` directory (the dispatcher's readiness gate checks for one;
   Kind nodes ship without a kernel, so copy the host's in for development),
   and an enforcing CNI if you rely on the rendered NetworkPolicies.
2. A reviewed starter package, built **once** on any Linux host with the
   pinned Celln release (`config/celln/release.json`):

   ```sh
   celln starter-package --runtime-dir /opt/celln/runtime \
     --guest-dir /opt/celln/runtime/pilot --kernel /boot/REVIEWED_KERNEL \
     --signing-key /etc/celln-publisher/private.seed \
     --output /var/lib/celln-packages/starter-2026-09
   celln starter-inspect /var/lib/celln-packages/starter-2026-09   # packageHash, publisher
   ```

   Record `packageHash` and the bundle `publisher`. Guard the seed; it is
   never copied into the package or the cluster.
3. The package published as a digest-pinned OCI image whose only content is
   the package directory at `/package`:

   ```sh
   printf 'FROM scratch\nCOPY package /package\n' > Dockerfile
   cp -r /var/lib/celln-packages/starter-2026-09 package
   docker build -t registry.example/celln/starter:2026-09 . && docker push registry.example/celln/starter:2026-09
   docker inspect --format '{{index .RepoDigests 0}}' registry.example/celln/starter:2026-09
   ```

   Use the `repository@sha256:...` form; tags are refused. A private registry
   needs a `.dockerconfigjson` Secret in `celln-system` referenced by
   `celln.fleet.package.pullSecret`.
4. The model provider credential in a local file (one line), or an existing
   `celln-fleet-model-credential` Secret in `celln-system`.

## Install

```sh
sympozium install -n celln-agents --celln-fleet \
  --celln-fleet-scope starter \
  --celln-fleet-package-image registry.example/celln/starter@sha256:REVIEWED_DIGEST \
  --celln-fleet-package-hash blake3:REVIEWED_PACKAGE_JSON_HASH \
  --celln-fleet-publisher REVIEWED_PUBLISHER_KEY \
  --celln-fleet-model-credential-file /path/to/model-token \
  --celln-fleet-output-dir /ABS/PRIVATE/fleet-starter \
  --celln-native-approve-starter-tools
kubectl label node kvm-a kvm-b celln.dev/kvm=true
```

The command runs two phases and is safe to rerun:

1. Publishes the parent principal and model credential, then installs the
   chart with `celln.fleet.*` set. Labeled nodes pull the package by digest,
   verify `package.json` against the hash, run `starter-admit` (real guest
   member checks on KVM) and `starter-configure`, and the first node publishes
   `catalogue.json`, `configured.json` and `native-template.json` as the
   `celln-fleet-configuration` ConfigMap. Later nodes verify they derived
   identical files; a different package under the same scope is refused.
2. Waits (default 15 minutes, `--celln-fleet-wait`) for that ConfigMap,
   materializes it under the output directory, installs the catalogue and
   grant layers in the target namespace exactly as `celln-tool install-native`
   does, and publishes `celln-parent-config` with a `remoteProvisioner`. The
   final upgrade wires the controller and creates the journal claim.

If the wait expires, label a node, inspect the `prepare` init container's log
in `celln-system`, and rerun the same command: existing trust, credential,
configuration and installation records are verified, never replaced.

## Authorising namespaces

The installer publishes the reviewed starter configuration **once per scope**
as cluster-scoped objects — `CellnRuntimeProfile` `celln-native-<scope>`
(carrying the native parent/worker material), three `ClusterCellnTool`s
`celln-<scope>-<tool>`, and a `CellnExecutionPolicy` `celln-fleet-<scope>`
that admits every namespace labeled `celln.sympozium.ai/scope=<scope>` with a
`host-profile` model route and the reviewed ceilings — and labels the `-n`
namespace with its wrapper objects. Any other namespace needs only:

```sh
kubectl label namespace team-b celln.sympozium.ai/scope=<scope>   # operator: authorises the namespace
kubectl -n team-b apply -f - <<EOF                              # tenant: ordinary workload objects
apiVersion: sympozium.ai/v1alpha1
kind: AgentRuntime
metadata: {name: celln-native}
spec:
  cellnProfileRef: {name: celln-native-<scope>, revision: v1}
---
apiVersion: sympozium.ai/v1alpha1
kind: Agent
metadata: {name: celln-agent}
spec: {runtimeRef: celln-native}
---
apiVersion: sympozium.ai/v1alpha1
kind: ModelConnection
metadata: {name: celln-native}
spec: {provider: deepseek, protocol: openai-chat, endpoint: https://api.deepseek.com/chat/completions, credentialProfile: <scope>, models: [deepseek-chat]}
EOF
```

No per-namespace install, grant ConfigMaps or copied tools. Enduring runs in
that namespace select `runtimeRef: celln-native`, `clusterToolRefs` from the
shared catalogue, `model.connectionRef: celln-native` and the profile's
persona; the controller resolves policy, profile, tools and route into one
immutable decision, issues the parent through the gateway, and re-checks that
authority before every later turn. A run in an unlabeled namespace is held
with `CellnParentReady=AdmissionPending (AUTH_POLICY_WITHDRAWN)`; nothing is
issued. The UI wizard offers wrapper runtimes on the Celln plane, lists the
shared catalogue for them and uses the namespace's host-profile
`ModelConnection` instead of asking for a key.

The `ModelConnection`'s `credentialProfile` names the owner-installed model
credential (the scope); `CellnExecutionPolicy` routes with `auth: host-profile`
are the interim boundary until the model gateway (P1) attaches to native
parents and routes switch to `auth: secret`.

## Verify

```sh
kubectl -n celln-system get daemonset celln-node            # DESIRED = labeled nodes
kubectl -n celln-system get pods -l app.kubernetes.io/name=celln-node -o wide
kubectl -n celln-system logs -l app.kubernetes.io/name=celln-router | grep backends
```

Then create an enduring run in an authorised namespace with the shared starter
tools; the installer leaves a ready-made `run.json` under the output directory
(change its namespace for another authorised namespace).
The run's `status.cellnParent.binding.target` is the gateway; the gateway's
`provisions` and `parents` ledgers on the ownership claim record which owner
holds it, and that owner's `authority/parent-journal` carries the incarnation.
Follow-up turns created by hand must carry the run's controller
`ownerReference` (the API server adds it for you); a turn without one is
refused as unbound. `test/integration/test-celln-fleet.sh` runs the whole
journey on a three-node Kind cluster, including a node-leave drain.

## Operations

- **Join a node:** label it. The package is admitted on that node only.
- **Leave a node:** remove the label or drain it. The DaemonSet pod's preStop
  calls `/v1/drain`, which closes admission, stops every parent tree on that
  owner and confirms teardown; those runs report `ContextLost`. Once the
  address has left the fleet the gateway answers `original parent backend
  removed` for its identities, which the controller also treats as context
  loss (and as established teardown when the run is deleted); nothing is
  re-placed.
- **Rolling updates** replace one node's dispatcher at a time with the same
  drain semantics. A dispatcher restart loses live parents on that node.
- **New package:** use a new scope. One scope carries exactly one package
  hash; nodes refuse to publish a differing configuration.
- **Uninstall** retains `/var/lib/sympozium-celln/<scope>` on each node and the
  journal claim; remove them only after every run has been deleted and
  cleanup confirmed.

## Capacity

The gateway places each parent by incarnation hash, not by load, and an owner
that refuses a create for capacity ends that run (`Parent outcome unavailable`)
rather than re-placing it. **Celln v0.5.11 holds one parent per node**: while
any parent is live the dispatcher advertises no spare egress (its parent
registry does not yet charge exact broker slots), so a second parent — or an
egress-using one-shot — placed on that node is refused. Plan one parent per
labeled node and add nodes for more; per-parent broker accounting is tracked
in the Celln repository. `maxCells`, `memoryBytes` and `egressSlots` still
bound one-shot work on an idle node.

## Limits

Single active turn per parent, no parent migration or checkpoint recovery,
no live lease extension, and the model credential Secret is an interim
boundary. Namespace enablement still creates the catalogue in the target
namespace; the cluster-scoped profile/policy path (#495, #505) is the next
step toward "any authorised namespace".
