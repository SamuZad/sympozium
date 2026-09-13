# Framework workspace: real-provider product walkthrough

This supersedes the earlier fixture-only CLI/UI review epoch. See [the workspace walkthrough](../guides/celln-workspace-framework.md). It is a bounded MVP product workspace, not completion of epic #495 or production acceptance.

## Installed images

All are immutable images in the framework review registry:

| Component | Repository suffix | Digest |
| --- | --- | --- |
| Workspace UI/API | `celln-review-ui` | `sha256:303c403243b30d731b5690362648afabb0520995943f6e29b956276c7e434b7f` |
| Scoped controller | `celln-review` | `sha256:72670d8b5f945b9ed3c123405cce9b1a56dfffacb0ec3160e1da17406a90f4b0` |
| Native receiver | `celln-review` | `sha256:72a3678ebd77f6a7619f9d61edcc0af80dd78bc3c9b46f3a41184772fb9db339` |
| Model gateway | `celln-review` | `sha256:0017975d80a0304a66ab146476627470fc5313e92b21604912ed4e1d5d5423b6` |

Registry prefix: `localhost:30501/`. PostgreSQL retains its existing dedicated PVC and image. Existing main UI, celln-system deployments, provider source Secret and native authority root were not replaced.

Native source: Celln `d10d70c`. Scoped parents explicitly reserve one serialized-child broker slot under the same admission lock as one-shot and legacy parent admission. Unknown legacy charges still fence spare egress; uncertain joins never release charges. The pool increased from 1 GiB to 2 GiB after reconciling the original expired owner, with 6 cell slots and 4 broker slots.

Public providers use system/public CAs. Private fixture CA additions apply only to explicitly allowlisted private origins, not DeepSeek. No ambient proxy, redirect, retry or insecure-TLS fallback was introduced.

## Final browser evidence

The final `cypress.product-live.config.ts` run passed both real-provider journeys in 66 seconds, after the public-CA gateway update:

- Cairo answered directly by `deepseek-v4-flash` through native Celln, with one-shot cleanup.
- Two enduring parents ready concurrently: Cairo and Paris.
- Contextual Cairo → Nile → Egypt follow-ups, uppercase tool workflow, original four-turn ceiling, refresh without budget reset, and stop/cleanup for both original owners.

Final roots:

| Conversation | Run UID | Initial native cell |
| --- | --- | --- |
| Cairo, with three follow-ups | `b3c45db7-8d93-473a-ae45-0cb139fcaf56` | `bdeac34f1276` |
| Concurrent Paris | `a16fe151-f2c2-4506-8f8e-57a981151a21` | `5da788ec9c7c` |

Independent operator checks—not Kubernetes object absence—confirmed each exact native receiver/owner record reported **“retained parent and descendants stopped”** and each original PostgreSQL budget was closed.

- Cairo: ceilings 8 requests / 4096 output tokens / 4 turns; 4 turns registered; 5 requests reserved, 2560 output tokens reserved, 187 output tokens observed; closed.
- Paris: same original ceilings; 1 turn registered; 1 request / 512 output tokens reserved, 14 output tokens observed; closed.

The five-request/four-turn Cairo workflow includes the tool-call round trip. The extra API ceiling probe created no additional turn or allowance. The browser probe proves API refusal; the earlier native integration suite separately proved gateway/native exhaustion fencing.

Evidence under the private operator work directory:

| Artifact | SHA-256 |
| --- | --- |
| `product-final-evidence.json` | `2765d7dcefc8006b9f597da6f1e434e6024a593dc4d468d91eeaa2436559616d` |
| `product-browser-final.log` | `a5b679b93565afab92c0abd2da552329528ffc9ceb73ca000b85a3df5c01770b` |
| `product-rbac-results.txt` | `661ab3f059f98ba2a58fa5e81683074842efd23fe5367aa2cc5af3af77638a03` |

The RBAC probe used an actual short-lived **workspace service-account token**: provider Secret reads in A/B and control-plane Secret listing returned 403; permitted run listing returned 200. This is not general tenant-user RBAC acceptance.

## Product behavior and boundaries

- Profiles/model/limits come from operator configuration, not a fixture label hardcoded into the browser.
- The signed bundle fixes the available uppercase tool. The model can answer without invoking it; arbitrary removal/recomposition of signed bundle sources is not advertised as supported.
- Raw native diagnostics are not exposed. Known capacity/deadline/budget refusals have mapped public explanations.
- Controller cleanup never falls through to legacy RBAC or successor dispatch in scoped-only mode, including when an observer finalizer retains the record.
- Only the workspace's own evidence hold can be removed by its archive route, after original-UID cleanup confirmation or explicit pre-admission cancellation/refusal. No-start proof is not described as VM teardown.
- Immutable UI-local request claims exclude replacement execution even after an archived run disappears.
- The original expired user parent was explicitly reconciled before native replacement. No active verification parent was left behind.

Batched Go authority/controller/API/gateway/budget checks passed, including enabled PostgreSQL checks on a disposable test database. Native scoped and parent-registry tests passed, including broker accounting, unknown legacy owners and uncertain teardown. The guarded provider installation script was reapplied successfully.

Remaining gates: tenant-user/CNI qualification, production recovery/drain/migration, broader catalogue UI integration, A01–A12 acceptance, contract review and final release packaging/stack merges. Do not turn this walkthrough evidence into an epic-completion claim.
