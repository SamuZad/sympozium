# Framework: installed scoped Celln review 495

> Historical fixture epoch. The current real-provider workspace and browser
> verification are documented in [the workspace installed review](celln-workspace-installed-review.md).

The isolated **CLI manual walkthrough is deployed and verified**. This is a
bounded deterministic-provider review, not completion of epic #495, the A01–A12
acceptance bundle, or production enablement. The scoped UI code is merged but
is not served by this review deployment.

Use [the manual walkthrough](../guides/celln-framework-manual-review.md).

## Actual installed path

- Kubernetes context: `kubernetes-admin@kubernetes`, node `framework`.
- Review namespaces: `celln-review-{a,b,denied,system}-495`.
- Dedicated scoped-only controller, TLS native receiver, TLS gateway, separate
  TLS fixtures in A/B, and PostgreSQL with a dedicated 2Gi `local-path` PVC.
- Real KVM execution; no AgentRun Jobs. Native has read-only exact host
  kernel/modules plus its isolated state and receiver inputs. Projected inputs
  are copied into regular files without weakening native no-symlink checks.
- The existing controller excludes the four review namespaces. Existing
  `celln-system` dispatcher/router remained ready; their provider mounts were
  not changed.
- Native/controller service accounts were denied provider Secret API reads
  during explicit administrator impersonation checks. This is not a tenant
  token/RBAC journey or CNI enforcement qualification.

Reviewed source: Sympozium `5e61e22`, Celln `481a2b9`.

Image repository is `localhost:30501/celln-review`, with immutable digests:

- Controller: `sha256:1c6accc764eadfb4291f7bc4c2f60e2ed31318db3773a18b41f883d26c889c20`
- Native, gateway, fixtures: `sha256:a9b53aaf679b701b9f07b439226658dc6f7ce1a7ca3156f293b2fe98e5d0c656`
- PostgreSQL repository `localhost:30501/celln-review-postgres`:
  `sha256:d13db94ae661d517c5ed57c509a578d5ea64aae639871ba25294f4f42d83de28`

Native package: `blake3:f587218c9d90599fbfc508a2ac716652b575d3135737084f4d80d2fc4f639c30`.

## Observed results

Installed direct execution returned `{"text":"CELLN"}`. Model-backed one-shot
execution returned `CELLN` with a real child cell and confirmed native cleanup.
Those examples remain as `verify-direct-v2` and `verify-model-v3` in A.

The latest complete installed enduring verification used:

| Identity | Value |
|---|---|
| Root UID | `63eca31b-8ef1-4ab3-8dbe-dc818bd1e836` |
| Initial child cell | `7f0894534510` |
| Continuation UID | `457a81f9-8091-4110-9579-b09b986b063d` |
| Continuation cell | `960622d12752` |
| Initial / continuation result | `CELLN` / `VIOLET` |
| Root request / output-token ceilings | 4 / 2048 |
| Turns registered | 2 |
| Requests / tokens reserved | 4 / 2048 |
| Fixture output tokens observed | 32 |
| Third turn | `BudgetExhausted`, no native start, cleanup confirmed |
| Root stop | Native parent/descendants stopped; original ledger closed |

The third-turn cleanup atomically fenced missing/partial gateway registration
without charging a turn or changing the original counters. Parent deletion
was followed by independent inspection of its exact native status record and
original PostgreSQL budget row, not an assumption based on object absence.

The denied namespace returned `AUTH_POLICY_WITHDRAWN` before receiver enrollment.
The two-minute child deadline does not replace the ten-minute operator parent
ceiling; updating that template never extends an existing incarnation.

## Evidence and checks

Operator evidence is under `/tmp/celln-framework-495.BgIn4ega/`. It includes
`installed-enduring-verification.log`, root/turn snapshots, the installed public
component inventory, and `INSTALLED-REVIEW.sha256`. Do not publish the surrounding
private directory or its issuer, TLS, provider, and database material.

The latest installed verification log SHA-256 is
`ee73a38c577f861522ff0e22209cc1ca47f4f88e8fc5f4d03a17e820930f3c1c`.

The real integration harness separately passed all one-shot/enduring/accounting
and cleanup checks (`live-17.log`). Batched Go authority, scoped controller/API,
PostgreSQL gateway/budget, and integration-helper suites passed. Native scoped
contract and parent-journal suites passed. Offline deployment regeneration was
checked with a rejecting kubectl shim, including newline-free fixture keys.

Real-provider execution, browser journeys, tenant token isolation, enforced CNI
boundaries, production recovery/drain/migration qualification, and contract
review remain outstanding. Keep all protected dependencies if cleanup becomes
uncertain; never remove finalizers merely because an owner process is absent.
