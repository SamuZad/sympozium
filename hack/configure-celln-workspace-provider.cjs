#!/usr/bin/env node
// Operator installation, not a tenant API. Credential bytes are transferred
// only in memory to namespaced Secrets; never printed or written to artifacts.
'use strict';
const fs = require('fs'), os = require('os'), path = require('path'), cp = require('child_process');
process.umask(0o077);
function required(name) { if (!process.env[name]) throw Error(`${name} is required`); return process.env[name]; }
const context = required('CELLN_REVIEW_KUBE_CONTEXT'), kubeconfig = required('KUBECONFIG');
const sourceNS = required('CELLN_PROVIDER_SECRET_NAMESPACE'), sourceName = required('CELLN_PROVIDER_SECRET_NAME'), sourceKey = required('CELLN_PROVIDER_SECRET_KEY');
const provider = required('CELLN_PROVIDER'), model = required('CELLN_MODEL'), endpoint = required('CELLN_PROVIDER_ENDPOINT');
const destination = process.env.CELLN_WORKSPACE_CONNECTION || 'live-deepseek';
const review = process.env.CELLN_REVIEW_ID || '495';
if (!/^[a-z0-9-]+$/.test(review) || !/^[a-z0-9-]+$/.test(destination)) throw Error('invalid review or connection identity');
const url = new URL(endpoint);
if (url.protocol !== 'https:' || url.username || url.password || url.search || url.hash || url.port) throw Error('fixed HTTPS provider endpoint required');
const uiNS = `celln-review-ui-${review}`;
const tenants = [`celln-review-a-${review}`, `celln-review-b-${review}`];
function k(args, input) {
  const result = cp.spawnSync('kubectl', ['--kubeconfig', kubeconfig, '--context', context, ...args], { input: input === undefined ? undefined : JSON.stringify(input), encoding: 'utf8', maxBuffer: 8 << 20 });
  if (result.status !== 0) throw Error('operator Kubernetes operation refused');
  return result.stdout;
}
function get(args) { return JSON.parse(k([...args, '-o', 'json'])); }
const deployment = get(['-n', uiNS, 'get', 'deployment', uiNS]);
if (deployment.spec.replicas !== 0 || (deployment.status?.replicas || 0) !== 0) throw Error('scale the isolated workspace UI to zero before changing new-run profiles');
for (const namespace of tenants) {
  const runs = get(['-n', namespace, 'get', 'agentruns']);
  if (runs.items.some(run => run.status?.cellnScoped ? !run.status.cellnScoped.cleanupConfirmed : !['Succeeded', 'Failed', 'Skipped'].includes(run.status?.phase))) throw Error('active or uncertain runs remain; reconcile their original owners first');
}
const source = get(['-n', sourceNS, 'get', 'secret', sourceName]);
if (!source.data?.[sourceKey]) throw Error('provider source key missing');
for (const namespace of tenants) {
  let existing;
  try { existing = get(['-n', namespace, 'get', 'secret', destination]); } catch { /* create below fails closed on API errors */ }
  if (existing) {
    if (existing.data?.OPENAI_API_KEY !== source.data[sourceKey]) throw Error('existing credential differs; explicit rotation is required');
  } else {
    k(['create', '-f', '-'], { apiVersion: 'v1', kind: 'Secret', metadata: { name: destination, namespace, labels: { 'sympozium.ai/celln-review': review } }, type: 'Opaque', data: { OPENAI_API_KEY: source.data[sourceKey] } });
  }
  k(['apply', '-f', '-'], { apiVersion: 'sympozium.ai/v1alpha1', kind: 'ModelConnection', metadata: { name: destination, namespace }, spec: { provider, protocol: 'openai-chat', endpoint, secretRef: destination, models: [model] } });
}
const policy = get(['get', 'cellnexecutionpolicy', `celln-review-policy-${review}`]);
const route = { provider, protocol: 'openai-chat', models: [model], endpointOrigins: [url.origin], auth: 'secret' };
if (!policy.spec.routes.some(value => JSON.stringify(value) === JSON.stringify(route))) {
  // Kubernetes JSON field order is not stable.
  const same = policy.spec.routes.some(value => value.provider === provider && value.protocol === 'openai-chat' && value.auth === 'secret' && value.models.length === 1 && value.models[0] === model && value.endpointOrigins.length === 1 && value.endpointOrigins[0] === url.origin);
  if (!same) policy.spec.routes.push(route);
}
policy.spec.ceilings.maxTurns = 4; policy.spec.ceilings.maxModelRequests = 8; policy.spec.ceilings.maxOutputTokens = 4096;
k(['replace', '-f', '-'], policy);
const config = get(['-n', uiNS, 'get', 'configmap', `${uiNS}-templates`]);
const temporary = fs.mkdtempSync(path.join(os.tmpdir(), 'celln-workspace-public-'));
try {
  for (const file of ['direct.yaml', 'model-one-shot.yaml', 'enduring.yaml']) {
    const input = path.join(temporary, file); fs.writeFileSync(input, config.data[file]);
    const decoded = cp.spawnSync('go', ['run', './hack/celln-framework-package-verify', '--yaml-to-json', input], { cwd: path.resolve(__dirname, '..'), encoding: 'utf8' });
    if (decoded.status !== 0) throw Error('public template decoding failed');
    const run = JSON.parse(decoded.stdout);
    if (file !== 'direct.yaml') {
      run.spec.model = { connectionRef: destination, provider, model };
      run.spec.task = 'Tell me where Cairo is.';
      run.spec.systemPrompt = 'You are a helpful assistant. Answer the user directly and accurately. Use the provided uppercase tool when the user asks to uppercase text; otherwise tools are optional. Keep answers concise.';
    }
    if (file === 'enduring.yaml') {
      Object.assign(run.spec.enduring, { maxTurns: 4, maxModelRequests: 8, maxOutputTokens: 4096, requireToolCall: false });
    }
    config.data[file] = JSON.stringify(run);
  }
  k(['replace', '-f', '-'], config);
} finally { fs.rmSync(temporary, { recursive: true }); }
console.log('Workspace provider installed. Original source Secret unchanged. Start the UI after completing native/operator upgrades.');
