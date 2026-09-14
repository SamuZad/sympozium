# Celln fleet on multi-node Kind — 2026-09-14

Local evidence for #530 (fleet) on a fresh three-node Kind cluster
(`fleet-control-plane`, `fleet-worker`, `fleet-worker2`; kernel
`7.1.13-200.fc44.x86_64`, real `/dev/kvm`). This is a development trial, not a
release claim: images were built from the working tree and the model
credential was the operator's own DeepSeek key.

## What ran

- Celln trial build: `main` (0.5.10) plus celln#109 (`POST /v1/parents/provision`,
  affinity at provisioning) and celln#110 (live `--backends-srv` refresh),
  static musl binaries, image `celln:fleet-trial`.
- Sympozium: this branch's controller/apiserver/webhook/celln-installer images
  tagged `fleet-trial`; CLI built from the branch.
- Starter package: `celln starter-package` with the host kernel and a
  throwaway operator seed; `celln starter-inspect` reported
  `blake3:c75a6a4b976f5d7b26748b32b19fe2033577dd7ac88e948aa8dc87c12d4fc1bb`,
  publisher `a39e74b9…10363`; pushed as a `FROM scratch` image to a registry on
  the Kind network and referenced by digest
  `sha256:69c7858565a13a19a64c379a5b2472d0e0984e1eddcd6be3e2d2c667082ce6d6`.
- One command: `sympozium install -n celln-agents --celln-fleet …` (see the
  guide). Workers were labeled `celln.dev/kvm=true`; no other per-node action.

## Observed

| Step | Result |
| --- | --- |
| Node preparation | Both `celln-node` init steps pulled the package by digest, verified the hash, admitted all five bundles with real guest member checks (`"admitted":true`), configured the model profile, and `fleet-worker` published `celln-fleet-configuration`; `fleet-worker2` verified identical files. |
| Plane | Two dispatchers Running; router started with zero owners, then discovered both through `celln-node.celln-system.svc.cluster.local` (capability report listed two nodes). Controller: no `kubernetes.io/hostname` selector, no hostPath, `CELLN_PARENT_CONFIG=/var/lib/sympozium/celln-parent/approvals` on the `celln-parent-journal` claim, `remoteProvisioner` targeting the router. |
| Run 1 `celln-starter-bb8tg` | Issued through the gateway (binding target = router), incarnation `blake3:e67a91f6…8644`, launch profile `blake3:d781c5b6…18e9` present only on `fleet-worker`; parent Ready; initial turn (real DeepSeek + `workspace-write` in child `blake3:ba8e0655…f1a5`) answered *Done — wrote "violet" to notes.txt (now at revision 1)*. |
| Run 2 `celln-starter-krhfh` | Incarnation `blake3:0842e2c9…9718`, owner `fleet-worker2`; initial turn succeeded. Owners are spread across the fleet. |
| Follow-up turn | `AgentRunTurn` with the run's controller ownerReference: distinct child `blake3:c70666f5…f634` on the same incarnation answered *Content: violet / Revision: 1*; `acceptedTurns=1`, slot released. |
| Node leave | `kubectl label node fleet-worker2 celln.dev/kvm-` drained the owner (preStop `/v1/drain`); run 2 failed with *Celln parent context lost or stopped … owner=Stopped reachedReady=true admittedAge=3m29s … no automatic reconstruction* and `status.cellnParent.ownerOutcome`; run 1 on `fleet-worker` stayed Running/Ready. |

## Defects found and fixed on the way

- `main` held every `enduring` catalogue selection for the tenancy scoped
  receiver even when only the prepared native parent path was configured, so
  no native parent could start without `CELLN_SCOPED_CONFIG`. Fixed: a scoped
  receiver still owns all enduring selections and shared intent never reaches
  legacy issuance; a legacy selection reaches the prepared parent path.
- The unified controller did not register the `AgentRunTurn` reconciler on the
  native parent path (only the retired parent-only binary did), so follow-up
  turns were never reconciled. Fixed.
- `sympozium install --celln-fleet` published trust before the chart created
  `celln-system`, and bound the catalogue before the controller rollout
  finished. Both reordered.
- Once a drained owner's address left the fleet, the gateway answered
  `503 original parent backend removed` and the controller parked the run as
  "uncertain" indefinitely (and could never confirm its cleanup). That refusal
  is now a distinct client error mapped to `ContextLost` on startup/status and
  to an established teardown on stop, so the run fails honestly and deletes.

## Placement is by hash, and a refused create is terminal

A second trial on a fresh `fleet-ci` cluster created two runs back to back
with the default 2 GiB node budget. Both incarnations hashed to the same owner
(`fleet-ci-worker2` held both launch profiles); the first parent came up Ready
and the owner refused the second create for capacity — each parent reserves
one egress slot and the fleet defaulted to `egressSlots: 1`, and two 1.25 GiB
parents also exceed a 2 GiB budget — so that run reported *Parent outcome
unavailable; preserving original incarnation without replay* and the gateway's
status read reached the owner (`parent owner not found`). The gateway does not
re-place a refused incarnation, by design. A live parent keeps a warm child, so it holds two
cells and two egress contexts. The fleet defaults now hold two parents per
node (`maxCells: 4`, `egressSlots: 4`, `memoryBytes: 4 GiB`); size them for the
parents a node should hold. The integration script starts runs
one at a time. Load-aware placement is a possible later gateway improvement.

## Environment caveats

- Kind nodes have no kernel in `/boot`; the dispatcher's readiness gate
  (`guest_kernel`) needs one, so the host kernel was copied into each worker.
  Real nodes are unaffected. The integration script does this automatically.
- Hand-written `AgentRunTurn` objects must carry the controller ownerReference
  to their AgentRun (`BindTurn` refuses otherwise); the API server sets it.
- The model credential reached dispatchers as a Secret mount (interim; the
  model gateway remains the target boundary).

Reproduce with `test/integration/test-celln-fleet.sh`.
