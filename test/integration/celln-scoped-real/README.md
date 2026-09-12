# Real scoped Celln integration harness

This is an opt-in executable, not a normal `go test` case. It creates a new
caller-named namespace and drives one real `AgentRun` by repeatedly invoking the
production `AgentRunReconciler` with the concrete `cellnscoped.Dispatcher`.
There is no mocked native execution and there is no skip-to-pass path.

The harness starts these local processes/listeners:

- the caller-supplied Celln binary and its `/v1/scoped/*` receiver;
- a TLS reverse proxy in front of Celln's loopback HTTP listener;
- the real in-process `modelgateway.Gateway`, backed by the caller's disposable
  PostgreSQL database and live Kubernetes reads;
- a bounded TLS OpenAI-compatible provider that accepts exactly one scoped
  request and returns the configured deterministic result.

It asserts protected preparation/final-decision persistence, the actual model
credential Secret UID pin, native owner/result provenance, PostgreSQL reserved
and observed usage, duplicate-start suppression, receiver cleanup, gateway
budget fencing, finalizer removal, absence of Kubernetes Jobs, and deletion of
every Kubernetes object it created. The PostgreSQL database must initially lack
the Celln budget tables; the runner refuses an existing schema.
The live self-subject review reports the supplied kubeconfig's real credential
custody permissions; this admin-style harness makes no tenant-RBAC isolation
claim.

## Mandatory isolation

Before invoking the runner, the coordinator must configure **every existing
framework controller to exclude the evaluation namespace** and confirm that
the exclusion is active. The runner refuses to create either namespace unless
`--global-controller-excludes-namespace` exactly repeats `--namespace`. This
flag records the coordinator's prerequisite; it cannot prove an external
controller's selector from inside the process.

Use an absolute, mode-0600 kubeconfig and name its context explicitly. Both
namespaces and the derived cluster-scoped profile/policy must not exist. The
Celln root must be caller-owned and fresh with respect to `root/scoped`, while
already containing the independently admitted signed package members and
publisher policy required by the receiver. The runner never installs trust.

## Package metadata

`--artifact-package` names the genuine signed package directory.
`--package-metadata` is public JSON emitted alongside that package:

```json
{
  "apiVersion": "sympozium.ai/celln-scoped-live-input-v1",
  "packageHash": "blake3:<BLAKE3 of artifact-package/package.json>",
  "runtime": { "the exact CellnRuntimeProfileSpec": "from the package builder" },
  "task": "the exact bounded task",
  "systemPrompt": "the exact bounded system prompt",
  "model": { "provider": "fixture", "protocol": "openai-chat", "name": "fixture-model" },
  "expected": {
    "output": "the exact assistant output",
    "providerCalls": 1,
    "observedOutputTokens": 7,
    "reservedOutputTokens": 512
  }
}
```

The initial contract is deliberately model-only: the runtime is the real
`celln.json-tools/v1` signed JSON runtime but no borrowed tool is selected.
Executable, closure, mote, publisher, and resource values come verbatim from
the package metadata. Celln independently checks those bytes at admission.

## Invocation

```bash
GOCACHE=/tmp/sympozium-go-cache go run ./test/integration/celln-scoped-real \
  --repo /absolute/path/to/sympozium \
  --kubeconfig /private/isolated.kubeconfig \
  --context isolated-context \
  --namespace celln-eval-unique \
  --global-controller-excludes-namespace celln-eval-unique \
  --preparation-namespace celln-eval-unique-authority \
  --postgres-url 'postgres://.../disposable_database' \
  --disposable-postgres-confirmation celln-eval-unique \
  --celln-binary /absolute/path/to/celln \
  --celln-root /absolute/path/to/fresh-prepared-celln-root \
  --celln-runtime-dir /absolute/path/to/celln/runtime \
  --celln-token-file /existing/private/celln-dispatcher-token \
  --scoped-operator-token-file /existing/private/celln-scoped-operator-token \
  --gateway-operator-token-file /existing/private/model-gateway-operator-token \
  --artifact-package /absolute/path/to/signed-package \
  --package-metadata /absolute/path/to/scoped-live-metadata.json \
  --timeout 3m
```

Only the final secret-free evidence JSON is written to stdout. Celln process
output and HTTP server error logs are discarded so scoped/model permits and
model credential bytes cannot enter harness logs or artifacts. Failures report
the controller condition, public native phase, accounting values, and invariant
that failed, but never request bodies or credential material.
