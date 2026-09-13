# Celln workspace on framework

The workspace now uses **real DeepSeek (`deepseek-v4-flash`) through the Celln model gateway**, not the uppercase fixture. Model inference is remote; the agent/harness and its tool execution run in native KVM cells.

## Try it

Open **https://192.168.1.237:30495**. Existing workspace login tokens still work.

Operator-local access files on framework:

- Token: `/tmp/celln-framework-495.BgIn4ega/ui-private/token`
- Private review CA: `/tmp/celln-framework-495.BgIn4ega/ui-private/ca.crt`

The certificate uses that private CA. Trust the supplied CA rather than disabling certificate verification globally. Do not publish the token or surrounding private directory.

1. Refresh the page; the title is **Celln workspace**, and the model is **deepseek-v4-flash**.
2. Choose **Single answer** or **Conversation**. Ask **“Tell me where Cairo is.”**
3. In a conversation, ask **“What river runs through that city?”** to test retained context.
4. Ask **“Call the uppercase tool with text sYmPoZiUm. Return only the tool result.”**
5. Start another conversation without stopping the first. Both can run concurrently; history switches between their original identities.
6. After four total turns, sending is disabled. **Check API refusal** verifies the original turn allowance without granting more authority. This is API enforcement, not a gateway-usage dashboard.
7. Select **Stop and retain evidence**. Wait for **cleanup confirmed**. Expand native evidence to inspect cell IDs and receipts, or download the root record.
8. **Remove stopped record** removes only the UI's evidence hold after confirmed cleanup. It never strips native/controller finalizers. Immutable request claims remain, preventing replay after record removal.

The approved model and signed tool bundle are operator-managed. The available uppercase tool is optional for the model to invoke; geography questions do not force an uppercase call. Arbitrary providers, executable images and tool bundles are not accepted by this workspace API.

## Current bounds

- Conversation: **4 total turns, 8 model requests, 4096 output tokens**, 600-second parent lifetime.
- Child execution deadline: **120 seconds**. Refreshing does not renew either deadline.
- Native pool: **2 GiB logical admission memory, 6 cell slots, 4 broker slots**.
- Each current parent reserves **576 MiB**, two cell slots (parent plus possible active child), and one broker slot. Two concurrent real-provider conversations were browser-tested. Other work may reduce available capacity.
- Capacity refusal is not a host-wide one-parent rule. Accounted scoped owners share the pool; legacy/unaccounted owners retain the conservative egress fence.
- Uncertain teardown keeps capacity charged. Stopping, expiration, missing objects and missing processes are not interchangeable cleanup proofs.

The UI/API has no provider, issuer, receiver or gateway credentials. Its own token and TLS key are separate. The original operator provider Secret was left unchanged; tenant-local credential copies are read by the gateway only.

## Build and installation

This is an additive isolated workspace, not a replacement of the existing main UI.

```bash
(cd web && npm ci && VITE_CELLN_REVIEW=true npm run build)
mkdir -p /tmp/celln-workspace-image
CGO_ENABLED=0 go build -trimpath -o /tmp/celln-workspace-image/celln-review-ui ./cmd/celln-review-ui
# Build images/celln-review-ui/Dockerfile with that directory as build context.
# Push/import the image, then supply its immutable digest to:
hack/deploy-celln-review-ui.sh
```

The deployment script requires explicit `KUBECONFIG`, `CELLN_REVIEW_KUBE_CONTEXT`, `CELLN_REVIEW_UI_IMAGE`, `CELLN_REVIEW_UI_HOST`, a **new** `CELLN_REVIEW_UI_PRIVATE` directory and operator `CELLN_REVIEW_UI_TEMPLATES`. It never adopts an unrelated existing namespace or replaces an existing UI identity.

To configure a real OpenAI-compatible provider, first stop/reconcile existing runs and scale the isolated UI fully to zero. Then run `hack/configure-celln-workspace-provider.cjs` with explicit:

- `KUBECONFIG`, `CELLN_REVIEW_KUBE_CONTEXT`
- `CELLN_PROVIDER_SECRET_NAMESPACE`, `CELLN_PROVIDER_SECRET_NAME`, `CELLN_PROVIDER_SECRET_KEY`
- `CELLN_PROVIDER`, `CELLN_MODEL`, `CELLN_PROVIDER_ENDPOINT` (fixed HTTPS endpoint)
- Optional `CELLN_REVIEW_ID` (default `495`) and `CELLN_WORKSPACE_CONNECTION`

Credential transfer stays in subprocess memory and namespaced Kubernetes Secrets. Different existing credentials require explicit rotation; the script does not silently replace them. Public route/template updates affect fresh runs, not frozen authorities. Restart/scale the UI back up after configuration. The script was reapplied successfully against the installed real-provider configuration.

## Verification and remaining acceptance

`web/cypress.product-live.config.ts` tests the actual HTTPS deployment with an explicitly supplied `CYPRESS_BASE_URL` and private `CYPRESS_REVIEW_TOKEN_FILE` path. It verifies real Cairo answers, concurrent parents, contextual Nile/Egypt follow-ups, uppercase workflow, original API turn ceiling, refresh and confirmed cleanup.

This is the product walkthrough milestone, **not epic closure or production enablement**. Tenant-user RBAC/CNI qualification, failure/restart/drain/migration coverage, broader model/tool catalogue integration, A01–A12 evidence and contract review remain gates. The workspace exposes only its bounded operator-approved execution profiles.
