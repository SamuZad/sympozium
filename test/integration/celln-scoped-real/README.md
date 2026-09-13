# Real scoped Celln framework-package harness

This opt-in executable drives production reconcilers and the concrete scoped
receiver. It loads the public `celln.framework-native-package/v1`
`package.json`, `resources.yaml`, `parent-request.json`, and the exact
`MANIFEST.blake3` file set. It does not accept bespoke runtime metadata.

The bounded suite proves:

- model-free `celln.json-direct/v1` execution of the package's `uppercase`
  tool, with exact output `{"text":"CELLN"}` and zero provider calls;
- composed one-shot `celln.json-tools/v1` execution where the TLS fake provider
  first requests the selected `uppercase` function and only then returns the
  actual native tool result `CELLN`;
- an enduring native parent, initial result, real `AgentRunTurn` returning
  `VIOLET`, a third turn rejected by the original shared PostgreSQL allowance,
  and parent/child cleanup;
- exact reserved/observed request and token counts, Secret UID pinning, TLS CA
  verification, receiver owner/receipt/native provenance, duplicate suppression,
  namespace-policy denial, absence of Kubernetes Jobs, and cleanup of every
  object and protected record created by the runner.

`--artifact-package` must already have been independently admitted into the
fresh caller-owned Celln root. The runner never installs trust. It derives the
receiver's complete `celln.scoped-parent-template/v1` wrapper from the
package's actual `parent-request.json`, including an explicit logical
`reservedMemoryBytes` value; it does not invent an executable, closure, mote,
publisher, tool, or schema.

The PostgreSQL database must initially lack the Celln accounting tables. The
runner applies only migrations 002 and 003 and refuses an existing schema.

## Isolated review namespaces

With `--existing-review-ownership 495`, all four namespaces must already carry
`sympozium.ai/celln-review=495`. The first two must additionally carry
`sympozium.ai/celln-review-tenant=enabled`; the denied and preparation
namespaces must not. The runner inventories them before use, permitting only
the Kubernetes-generated `default` ServiceAccount and `kube-root-ca.crt`
ConfigMap. It never deletes pre-existing namespaces and deletes only resources
whose UIDs it created or recorded. Without this flag, all four namespaces must
be absent and the runner creates and later deletes them.

Every global controller must already exclude the four namespaces in the exact
order passed to `--global-controller-excludes-namespaces`.

## Exact invocation for review 495

```bash
GOCACHE=/tmp/sympozium-go-cache go run ./test/integration/celln-scoped-real \
  --repo /tmp/sympozium-framework-live \
  --kubeconfig /absolute/private/isolated.kubeconfig \
  --context isolated-context \
  --namespace celln-review-a-495 \
  --enduring-namespace celln-review-b-495 \
  --denied-namespace celln-review-denied-495 \
  --preparation-namespace celln-review-system-495 \
  --existing-review-ownership 495 \
  --global-controller-excludes-namespaces celln-review-a-495,celln-review-b-495,celln-review-denied-495,celln-review-system-495 \
  --postgres-url 'postgres://.../initially_empty_disposable_database' \
  --disposable-postgres-confirmation celln-review-a-495 \
  --celln-binary /absolute/path/to/current/celln \
  --celln-root /absolute/path/to/fresh/prepared-celln-root \
  --celln-runtime-dir /absolute/path/to/celln/runtime \
  --celln-token-file /absolute/private/celln-dispatcher-token \
  --scoped-operator-token-file /absolute/private/celln-scoped-operator-token \
  --gateway-operator-token-file /absolute/private/model-gateway-operator-token \
  --artifact-package /tmp/celln-framework-package/target/framework-native-package \
  --timeout 10m
```

Only a secret-free final evidence object is written to stdout. Celln and HTTP
server logs are discarded so permits and model credentials cannot enter output.
