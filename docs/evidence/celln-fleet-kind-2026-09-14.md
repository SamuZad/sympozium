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
re-place a refused incarnation, by design. Raising the budget did not help:
`current_node` in the dispatcher zeroes advertised egress whenever any parent
is live ("parent registry does not yet carry exact broker-slot charges"), so
Celln v0.5.11 holds exactly one parent per node. The integration script now
accepts a same-owner refusal as the honest outcome, and per-parent broker
accounting is filed against Celln. Load-aware placement is a possible later
gateway improvement.

## Any authorised namespace (P0, PR #536)

A third trial on a fresh `fleet-ci` cluster ran the same install with the
controller in **platform admission mode**: no grant ConfigMaps, no copied
tools, one `CellnRuntimeProfile` (with native material), three
`ClusterCellnTool`s and one `CellnExecutionPolicy` for the scope, and the
install namespace labeled `celln.sympozium.ai/scope=trial`.

| Step | Result |
| --- | --- |
| Install namespace | `celln-starter-jp65f` resolved through policy, provisioned through the gateway on `fleet-ci-worker2`, answered the initial DeepSeek turn; `celln-starter-2bq6v` hashed to the other owner `fleet-ci-worker` and ran too (an earlier attempt of this trial saw the second run hash to the occupied node and be refused for capacity, not re-placed). |
| Follow-up turn | Distinct child on the same incarnation read back *Content: violet / Revision: 1*. |
| Release | Both live parents were deleted through the gateway; the fleet was free again. |
| Second namespace `celln-agents-b` | Operator labeled the namespace; tenant applied only the three wrapper objects copied from the install namespace. `celln-starter-5djrt` was admitted on `fleet-ci-worker` and answered its initial turn. No grant ConfigMaps, no namespaced tools. |
| Unlabeled namespace `celln-agents-denied` | Same wrapper objects, no label: the run was held with `CellnParentReady=AdmissionPending: Platform policy refused admission (AUTH_POLICY_WITHDRAWN)` and no parent was issued. |
| Node leave | Unlabeling `fleet-ci-worker` drained its owner; `celln-starter-5djrt` reported *context lost or stopped* and deleted cleanly. |

### Defects found and fixed on the way

- The platform resolver treated the controller's own persisted route
  (`spec.model.provider/protocol/baseURL/credentialProfile`) as an inline
  override and refused every run with `AUTH_ROUTE_MISMATCH`; then it compared
  the run's pinned connection revision against a digest computed differently
  from the one the controller pins. Mirrored values are accepted and one
  `modelconnection.Revision` serves both sides.
- The provision plan clamped the per-turn output allowance to the run total
  divided by turns (1024), below the owner's model profile (1536), so the
  owner refused every plan with an opaque `409 parent provisioning refused`.
  The resolver's turn cap now follows a native profile's per-turn allowance
  bounded by the run total; a run whose budget affords less than one turn is
  refused with `AUTH_LIMIT_RANGE` before anything is pinned.
- A failed issuance could never be retried: each retry resolved under a new
  clock, pinned a different choice and failed with "already assigned
  differently". The choice record now carries the frozen resolution; retries
  revalidate it and re-send byte-identical plans, so the gateway's owner
  affinity and the owner's ledger see one plan per incarnation.
- Owner refusals were reduced to "remote parent issuer failed" before reaching
  the log; the status and bounded error text are now kept.
- The guide's tenant wrapper YAML omitted `spec.image` and `spec.agents`,
  which the CRDs require even for profile wrappers.

Deleting a capacity-refused run looped on "parent or turn not found"; fixed
in PR #538 (#534).

## Zero-ceremony namespaces and node-sized capacity (P1a/P1b, PR #538)

A fourth trial on a fresh `fleet-ci` cluster used a Celln build of
sympozium-ai/celln#113 (per-parent broker charging, `d52d55a`) in the
installer image, the default-open policy and `celln.fleet.capacity: auto`.

| Step | Result |
| --- | --- |
| Capacity | Each dispatcher logged `celln capacity: node=66863595520 memory=50147696625 cells=74 egressSlots=74` (the Kind workers see the 64 GiB host). |
| Two parents, one node | `celln-starter-cq7kj` went Ready on `fleet-ci-worker` and answered its DeepSeek turn; the next run, `celln-starter-xtl7p`, hashed to the same owner, went Ready beside it and answered its own turn while the first stayed Ready. With Celln v0.5.11 the second was refused. |
| Follow-up turn | Distinct child read back *Content: violet / Revision: 1*. |
| Delete | Both co-located parents deleted through the gateway. |
| Unlabeled tenant namespace `celln-agents-b` | No label and no YAML: `GET /api/v1/celln-platform/profiles` offered `celln-native-trial`, `POST /api/v1/celln-platform/wrappers` created the three wrappers, and `celln-starter-q8mmp` ran on `fleet-ci-worker`. |
| Excluded namespace `celln-agents-denied` | Labeled `celln.sympozium.ai/excluded=true`: offered no profiles, wrapper creation answered 403, and a run (with hand-applied wrappers) was refused `AUTH_POLICY_WITHDRAWN` with no parent. |
| Node leave | Unlabeling `fleet-ci-worker` reported ContextLost on `celln-starter-q8mmp`, which deleted cleanly. |

A run 14 on Celln v0.5.11 passed the same namespace steps. Terminal
`CreateRefused` outcomes are covered by unit tests; with node-sized capacity
the journey no longer hits a capacity refusal.

## llama-server backend on the framework machine

A fifth trial ran the same journey with the scope's model backend set to a
llama-server on another machine reached over Tailscale
(`--celln-fleet-model-provider llama-server --celln-fleet-model
Qwen3.8-27B-UD-Q4_K_XL.gguf --celln-fleet-model-endpoint
http://100.81.163.75:8080/v1/chat/completions
--celln-fleet-model-allow-insecure`, no credential file). The Celln build
included sympozium-ai/celln#116 (model requests bounded by the turn deadline
instead of 45 seconds); llama-server served about 11 tokens/s.

| Step | Result |
| --- | --- |
| Install | Nodes configured the llama-server model profile; the policy route was stamped `auth: host-profile`, origin `http://100.81.163.75:8080`, `allowInsecure: true`; a placeholder credential Secret was published. |
| First parent | `celln-starter-ntjgc` (connection provider `llama-server`) answered *Done. Wrote "violet" to notes.txt; the file is now at revision 1.* |
| Two parents, one node | `celln-starter-27vh7` joined it on `fleet-ci-worker2` and answered its own turn. |
| Follow-up turn | Distinct child read back *violet (revision 1)*. |
| Tenant and excluded namespaces | `celln-starter-l484n` ran in unlabeled `celln-agents-b` with wrappers created on first use (a llama-server connection); `celln-agents-denied` was refused `AUTH_POLICY_WITHDRAWN`. |
| Node leave | ContextLost on `celln-starter-l484n`, clean delete. |

The first attempt failed at install: the `CellnExecutionPolicy` CRD rule
allowed plain HTTP only for no-auth loopback routes. Policy routes now carry
an explicit `allowInsecure` for host-profile credentials. OpenAI and Anthropic
presets are covered by unit tests; no live keys were available for this trial.

## Anthropic protocol and two backends side by side

A sixth trial ran the full journey with the llama-server backend over the
Anthropic Messages protocol (`--celln-fleet-model-protocol
anthropic-messages`, endpoint `http://100.81.163.75:8080/v1/messages`), with
sympozium-ai/celln#118 in the dispatcher. llama-server returns `thinking`
content blocks for reasoning models, which the broker used to refuse; they
are now dropped. Every step passed: first turn *Done. "violet" written to
notes.txt (now revision 1)*, two parents on one node, follow-up *violet
revision: 1*, deletes, label-free tenant namespace, excluded namespace,
node loss.

A second cluster, `fleet-ds`, ran the DeepSeek backend at the same time. Each
fleet then got a brand-new namespace, prepared only through
`/api/v1/celln-platform/wrappers`, and one enduring run asked *"Where is
Botswana? Answer in two sentences without using any tools."*:

| Namespace | Backend | Answer |
| --- | --- | --- |
| `botswana-llama` (`fleet-ci`) | llama-server, Qwen3.8-27B, `anthropic-messages` | *Botswana is a landlocked country located in southern Africa. It is bordered by South Africa, Namibia, Zimbabwe, and Zambia.* |
| `botswana-deepseek` (`fleet-ds`) | DeepSeek, `deepseek-chat`, `openai-chat` | *Botswana is a landlocked country in Southern Africa, bordered by South Africa to the south, Namibia to the west and north, Zimbabwe to the northeast, and Zambia to the north. Its capital is Gaborone, located in the country's southeastern corner near the South African border.* |

Two clusters were used because a fleet scope has one model backend; several
backends in one cluster is #535.

Running two installs on one host exposed installer defects, all fixed: Helm
ignored `$KUBECONFIG` and installed into whichever cluster `~/.kube/config`
selected; CRDs were not awaited as established; cert-manager readiness was
skipped when it was already present; GitHub 504s on release manifests aborted
the install (now retried, and the journey can use a cached cert-manager
manifest); and the journey leaked its API port-forward.

## Day-long ceilings and capacity-aware placement (Celln v0.5.16)

Two more journeys ran on the **released** Celln v0.5.16 bundle (host limits,
celln#120; capacity-aware placement, celln#122) with the installer defaults:
DeepSeek on `fleet-ds` and llama-server on `fleet-ci`.

| Check | Result |
| --- | --- |
| Ceilings | Policy `maxParentLeaseSeconds 86400, maxTurns 256, maxModelRequests 768, maxOutputTokens 393216`; the node's reviewed parent request carries `timeoutMs 86400000`; the sample conversation asks for `14400 s / 64 turns / 192 / 98304`. |
| Placement | Both fleets: the first parent landed on one owner, the second on the emptier owner, the third tied and co-located with the first (`fleet-ds`: `gc5sk` → worker, `8pdfs` → worker2, `9kt44` → worker; `fleet-ci`: `69696` → worker, `dkt5s` → worker2, `v9f7w` → worker). |
| Everything else | Real turns, follow-up context, deletes, two API conversations of one Agent in a label-free namespace, excluded namespace refused, node leave → ContextLost, clean delete — all passed on both fleets. |

The first DeepSeek attempt failed before any run: its generated NATS
password began with a digit, `nats.conf` references it as a bare variable,
NATS crash-looped and the controller never became ready (about one install
in six). Fixed in the chart in PR #542.

## Several model backends per scope (#535)

One scope, two backends, every node configures both, one namespace runs
parents on either. On `fleet-ci` (two KVM workers) the scope `ci` was
installed with `--celln-fleet-backend` twice against the framework machine's
llama-server: `native` over `openai-chat` (`/v1/chat/completions`) and
`messages` over `anthropic-messages` (`/v1/messages`), same model, same
origin, different protocol.

| Check | Result |
| --- | --- |
| Node preparation | Each node ran `starter-configure` twice from the one admitted package and published `native.*` and `messages.*` keys in `celln-fleet-configuration`; the second node verified. |
| Catalogue | Profiles `celln-native-ci` (backend `native`, protocol `openai-chat`, credential `ci`) and `celln-native-ci-messages` (backend `messages`, protocol `anthropic-messages`, credential `ci-messages`); one policy `celln-fleet-ci` with both profiles and both routes. |
| Credentials | Secrets `celln-fleet-model-credential` and `celln-fleet-model-credential-messages`, each mounted read-only into every dispatcher at its own directory (`/etc/celln-native`, `/etc/celln-native/messages`); the prepare step mounts neither. |
| Tenant | Label-free `celln-agents-b` was offered both profiles by the API and got wrappers `celln-native`/`celln-agent` and `celln-messages`/`celln-agent-messages`, each connection bound to the route of its own protocol. |
| Runs | Three conversations at once in that namespace: two of `celln-agent` on `native`, one of `celln-agent-messages` on `messages`. The `messages` parent answered "Botswana is a landlocked country in southern Africa, bordered by South Africa, Namibia, Zimbabwe, and Zambia." |
| Everything else | Real turns, follow-up context, gateway deletes, excluded namespace refused, node leave → ContextLost, clean delete — all passed. |

A first attempt paired DeepSeek (`native`) with llama-server (`local`). The
install was correct (two profiles, two Secrets, prefixed ConfigMap keys,
both wrappers) but DeepSeek was returning `503 Service is too busy` and then
hanging for 60 s at the time; its parents lost context after the 60 s turn
deadline while the llama-server parent answered in seconds. The pair with
two llama-server protocols was run instead. Worth noting: a provider outage
surfaces as `ContextLost` rather than a failed turn, which is a Celln
owner behaviour to look at separately.

## Environment caveats

- Kind nodes have no kernel in `/boot`; the dispatcher's readiness gate
  (`guest_kernel`) needs one, so the host kernel was copied into each worker.
  Real nodes are unaffected. The integration script does this automatically.
- Hand-written `AgentRunTurn` objects must carry the controller ownerReference
  to their AgentRun (`BindTurn` refuses otherwise); the API server sets it.
- The model credential reached dispatchers as a Secret mount (interim; the
  model gateway remains the target boundary).

Reproduce with `test/integration/test-celln-fleet.sh`.
