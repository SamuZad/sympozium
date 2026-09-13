# Framework manual acceptance target — epic #495

The requested review milestone is a working deployment on the Kubernetes node
`framework`, not another component-test checkpoint. Existing workloads must be
preserved. Use explicitly named, isolated evaluation resources until replacement
of the installed runtime is qualified.

## Required manual journeys

- Create a model-free one-shot run using immutable shared catalogue artifacts;
  inspect the actual native result and confirmed cell cleanup.
- Create a model-backed one-shot run; correlate controller operation, native
  execution, scoped gateway reservations, result, and cleanup.
- Create an enduring run; observe the real retained native parent, submit multiple
  turns, verify that turns share the original allowance and deadline, then stop
  the run and observe confirmed descendant/parent teardown.
- Attempt a budget-exhausting turn/request and observe refusal without allowance
  reset, fallback provider access, or an automatic second execution.
- Exercise namespace A/B binding and a denied namespace. Distinguish actual
  tenant-RBAC tests from administrator-issued identity-binding tests.
- Inspect uncertainty after interrupted execution; no replay may manufacture a
  fresh native context. Unconfirmed cleanup must remain visibly unconfirmed.

Provide exact namespace names, UI/CLI access instructions, example resources,
expected results, image/source pins, and a bounded cleanup procedure with the
manual-review handoff. Do not label the deployment ready until these journeys
have actually been exercised.

## Implementation order

1. Receiver-owned authenticated admission and immutable artifact/resource checks.
2. Controller-owned durable preparation before dispatch side effects; native
   one-shot launch with the original scoped broker, correlated result and cleanup.
3. Native enduring ownership, fresh turn authority under original ceilings,
   cancellation, recovery and teardown.
4. Deploy compatible controller/runtime/gateway/trust/artifacts on framework;
   complete user-visible lifecycle and budget state, then run installed journeys.

## Current evidence boundary

The scoped gateway/relay and PostgreSQL accounting have component evidence.
Celln additionally has a durable owner journal and a real-KVM model-free JSON
adapter test. These do not yet establish the controller-created or enduring
manual journeys above. Contract review remains a production-enablement gate.

Read-only discovery confirmed framework is Ready and currently hosts existing
Celln and Sympozium deployments. No deployment replacement was performed as
part of that discovery. Flannel is present; do not infer NetworkPolicy from installed policy YAML.
