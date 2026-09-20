/**
 * Celln cells in the console: `celln ps` per fleet node on the Harnesses page,
 * parents and running cells on the topology graph, and enduring Celln agents
 * in the feed's Persistent tab. Intercepted browser contract tests — they
 * need no cluster and are not live execution evidence.
 */

const now = Date.now();
const incarnation = "blake3:" + "a".repeat(64);
const child = "blake3:" + "c".repeat(64);
const run = { namespace: "default", name: "hermes-abc12", agent: "hermes", phase: "Running", live: true };

const cells = [
  {
    node: "framework",
    reportedMs: now - 3000,
    stale: false,
    source: "gateway",
    cells: [
      { id: "2e1e3481f3b7", description: child.slice(0, 29) + "…", status: "running", backend: "kvm", started_ms: now - 12000, finished_ms: null, duration_ms: null, error: null, tools: ["/worker"], run, parent: incarnation, turn: "initial" },
      { id: "07359a714094", description: "blake3:80a5803438c9b4dba1f6bc8…", status: "failed", backend: "kvm", started_ms: now - 600000, finished_ms: now - 570000, duration_ms: 30000, error: "guest exited with code 1", tools: ["/worker"] },
    ],
    parents: [
      { incarnation, updatedMs: now - 12000, turns: [{ turnId: "initial", stage: "reserved", child }], run, status: "TurnActive", statusLive: true },
      { incarnation: "blake3:" + "b".repeat(64), updatedMs: now - 900000, turns: [{ turnId: "initial", stage: "child-destroyed", succeeded: false }], run: { ...run, name: "hermes-old01", phase: "Failed", live: false } },
    ],
  },
  // A backend the gateway could not list, filled from its node's own report.
  { node: "gpu-2", reportedMs: now - 600000, stale: true, source: "node-report", cells: [], parents: [] },
];

const hermes = {
  metadata: { name: "hermes", namespace: "default" },
  spec: {
    runtimeRef: "celln-llama-server",
    agents: { default: { model: "Qwen3.8-27B-UD-Q4_K_XL.gguf" } },
    execution: {
      backend: "celln", executionLifecycle: "enduring", modelConnectionRef: "celln-llama-server", model: "Qwen3.8-27B-UD-Q4_K_XL.gguf",
      cellnSelection: { runtimeRef: "celln-llama-server", toolRefs: [], clusterToolRefs: [{ name: "celln-starter-grep", revision: "v1" }] },
      enduring: { leaseSeconds: 14400, maxTurns: 64, maxModelRequests: 384, maxOutputTokens: 196608 },
    },
  },
  status: { phase: "Running" },
};

const wrapper = {
  metadata: { name: "celln-llama-server", namespace: "default", labels: { "sympozium.ai/managed-by": "celln-platform" } },
  spec: { cellnProfileRef: { name: "celln-native-starter-llama-server", revision: "v1" }, supportOwner: "celln-platform" },
};

// The platform's own wrapper Agent names only its fleet runtime.
const wrapperAgent = { metadata: { name: "celln-agent", namespace: "default" }, spec: { runtimeRef: "celln-native" }, status: { phase: "Running" } };
const nativeWrapper = {
  metadata: { name: "celln-native", namespace: "default", labels: { "sympozium.ai/managed-by": "celln-platform" } },
  spec: { cellnProfileRef: { name: "celln-native-starter", revision: "v1" }, supportOwner: "celln-platform" },
};

const conversation = {
  metadata: { name: "hermes-abc12", namespace: "default", uid: "run-uid", creationTimestamp: new Date(now - 12000).toISOString() },
  spec: { agentRef: "hermes", backend: "celln", executionLifecycle: "enduring", task: "Hello", enduring: { maxTurns: 64 } },
  status: { phase: "Running", conditions: [{ type: "CellnParentReady", status: "True" }], cellnParent: { acceptedTurns: 0, createAttempted: true, binding: { incarnation }, initialTurn: { id: "initial", attempted: true } } },
};

function stubConsole() {
  cy.intercept("GET", "**/api/v1/**", { body: [] });
  cy.intercept("GET", "**/api/v1/celln-platform/cells*", { body: cells }).as("cells");
  cy.intercept("GET", "**/api/v1/agents*", { body: [hermes, wrapperAgent] });
  cy.intercept("GET", "**/api/v1/runtimes*", { body: [wrapper, nativeWrapper] });
  cy.intercept("GET", "**/api/v1/runs*", { body: [conversation] });
  cy.intercept("GET", "**/api/v1/gateway*", { body: {} });
  cy.intercept("GET", "**/api/v1/density/**", { body: { nodes: [] } });
}

describe("Celln cells in the console", () => {
  beforeEach(stubConsole);

  it("shows celln ps per node on the Harnesses page, with -a and a node selector", () => {
    cy.visit("/harnesses#token=test-token");
    cy.wait("@cells");
    cy.get('[data-testid="celln-node-cells"]').within(() => {
      cy.contains("celln ps").should("be.visible");
      // Two sources in one fleet: said per node, and the parent's own status shown.
      cy.get('[data-testid="celln-cells-source"]').should("contain", "source: mixed");
      cy.get('[data-testid="celln-node-source-framework"]').should("contain", "source: gateway");
      cy.get('[data-testid="celln-node-source-gpu-2"]').should("contain", "source: node reports");
      cy.get('[data-testid="celln-parent-status"]').should("contain", "TurnActive");
      cy.get('[data-testid="celln-node-framework"]').within(() => {
        cy.contains("1 running");
        cy.contains("1 live parent");
        cy.contains("tr", "2e1e3481f3b7").should("contain", "default/hermes-abc12").and("contain", "initial");
        // Finished cells and ended parents only with -a.
        cy.contains("07359a714094").should("not.exist");
        cy.contains("hermes-old01").should("not.exist");
      });
      cy.get('[data-testid="celln-node-gpu-2"]').should("contain", "stale");
      cy.contains("label", "Show finished (-a)").click();
      cy.contains("celln ps -a");
      cy.get('[data-testid="celln-node-framework"]').within(() => {
        cy.contains("tr", "07359a714094").should("contain", "failed").and("contain", "guest exited with code 1");
        cy.contains("hermes-old01").should("be.visible");
      });
      cy.get("button[role=combobox]").click();
    });
    cy.get("[role=option]").contains("gpu-2").click();
    cy.get('[data-testid="celln-node-framework"]').should("not.exist");
    cy.get('[data-testid="celln-node-gpu-2"]').should("be.visible");
  });

  it("updates the cells table without a reload", () => {
    let polls = 0;
    cy.intercept("GET", "**/api/v1/celln-platform/cells*", (request) => {
      polls++;
      const report = structuredClone(cells[0]);
      report.reportedMs = Date.now() - 1000;
      // From the third poll on, the node's turn has finished.
      if (polls >= 3) {
        report.cells[0] = { ...report.cells[0], status: "dissolved", finished_ms: Date.now(), duration_ms: 4200 };
      }
      request.reply({ body: [report] });
    }).as("poll");
    cy.visit("/harnesses#token=test-token");
    cy.get('[data-testid="celln-cells-live"]').should("contain", "live");
    cy.get('[data-testid="celln-node-framework"]').should("contain", "1 running").and("contain", "2e1e3481f3b7");
    cy.wait(["@poll", "@poll", "@poll"]);
    cy.get('[data-testid="celln-node-framework"]', { timeout: 10000 }).should("contain", "0 running").and("not.contain", "2e1e3481f3b7");
    cy.contains("label", "Show finished (-a)").click();
    cy.contains("tr", "2e1e3481f3b7").should("contain", "dissolved").and("contain", "4.2 s");
  });

  it("does not list the fleet's wrapper runtimes as Kubernetes harnesses", () => {
    cy.visit("/harnesses#token=test-token");
    cy.wait("@cells");
    cy.contains("No approved harnesses are registered").should("be.visible");
  });

  it("draws the live parent and its running cell on the topology", () => {
    cy.visit("/topology#token=test-token", {
      onBeforeLoad(win) {
        win.localStorage.removeItem("sympozium_topology_positions");
      },
    });
    cy.wait("@cells");
    cy.get(".react-flow__node-k8sNode").should("contain", "framework");
    cy.get(".react-flow__node-cellnCell").should("have.length", 2);
    cy.get(".react-flow__node-cellnCell").contains("Celln parent").parents(".react-flow__node").should("contain", "hermes-abc12");
    cy.get(".react-flow__node-cellnCell").contains("Celln cell").parents(".react-flow__node").should("contain", "2e1e3481f3b7").and("contain", "turn initial");
  });

  it("lists enduring Celln agents as persistent chats in the feed", () => {
    cy.intercept("GET", "**/api/v1/runs/hermes-abc12/turns*", { body: { runUID: "run-uid", items: [], continue: "" } });
    cy.visit("/dashboard#token=test-token");
    cy.get('[title="Open feed"]').click();
    cy.contains("button", "Persistent").click();
    cy.get('[data-testid="feed-celln-agent-hermes"]').should("contain", "1 live").and("contain", "celln-llama-server").within(() => {
      cy.contains("button", "Open").click();
    });
    cy.get('[data-testid="feed-celln-conversation"]').should("contain", "hermes");
    cy.contains("button", "Back").click();
    // The platform wrapper Agent is a persistent Celln chat too.
    cy.get('[data-testid="feed-celln-agent-celln-agent"]').should("contain", "New").and("contain", "celln-native");
    cy.get('[data-testid="feed-celln-agent-hermes"]').should("be.visible");
    // A Celln agent is not offered for Kubernetes quick tasks.
    cy.contains("button", "Runs").click();
    cy.contains("No persistent agents").should("not.exist");
  });
});

// Two fleet backends: the Provider step picks the backend, so the wizard must
// not also ask which wrapper runtime to use, and a fleet runtime must not be
// asked for a Kubernetes harness policy. Intercepted; no cluster needed.
describe("Create Agent on a fleet with several backends", () => {
  const profile = (backend: string, model: string) => ({
    name: `celln-native-starter${backend === "native" ? "" : "-" + backend}`,
    revision: "v1", policy: "celln-fleet-starter", model, provider: backend === "native" ? "deepseek" : backend,
    endpoint: backend === "native" ? "https://api.deepseek.com/chat/completions" : "http://framework:8080/v1/chat/completions",
    credentialProfile: `starter${backend === "native" ? "" : "-" + backend}`, systemPrompt: "Keep replies brief.",
    backend, wrapper: backend === "native" ? "celln-native" : `celln-${backend}`, agent: "celln-agent",
    tools: [{ name: "celln-starter-workspace-read", revision: "v1" }, { name: "celln-starter-grep", revision: "v1" }],
    ceilings: { leaseSeconds: 86400, maxTurns: 256, maxModelRequests: 1536, maxOutputTokens: 786432 },
    sessionDefaults: { leaseSeconds: 14400, maxTurns: 64, maxModelRequests: 384, maxOutputTokens: 196608 },
  });
  const profiles = [profile("native", "deepseek-chat"), profile("llama-server", "Qwen3.8-27B-UD-Q4_K_XL.gguf")];
  const runtimeFor = (p: (typeof profiles)[number]) => ({
    metadata: { name: p.wrapper, namespace: "default", labels: { "sympozium.ai/managed-by": "celln-platform" } },
    spec: { cellnProfileRef: { name: p.name, revision: p.revision }, supportOwner: "celln-platform" },
  });

  beforeEach(() => {
    cy.intercept("GET", "**/api/v1/**", { body: [] });
    cy.intercept("GET", "**/api/v1/celln-platform/profiles*", { body: profiles }).as("profiles");
    cy.intercept("GET", "**/api/v1/runtimes*", { body: profiles.map(runtimeFor) });
    cy.intercept("GET", "**/api/v1/cluster-celln-tools*", {
      body: profiles[0].tools.map((tool) => ({
        metadata: { name: tool.name, uid: tool.name }, spec: { revision: tool.revision, invocationABI: "celln.json-stdio/v1", lane: "tool", description: tool.name, limits: { timeoutMillis: 30000, memoryBytes: 1, workspace: "none", effects: "none" }, supportOwner: "op", publisherKey: "k" },
      })),
    });
    cy.intercept("GET", "**/api/v1/capabilities*", { body: { celln: { available: true, state: "ready", reason: "fleet ready" } } });
    cy.intercept("GET", "**/api/v1/model-connections*", { body: [] });
  });

  it("skips the runtime step and never asks a fleet runtime for a harness policy", () => {
    cy.visit("/agents?create=1&kind=agent#token=test-token");
    cy.get('[role="dialog"]').within(() => {
      cy.get('input[placeholder="my-agent"]').type(`fleet-${Date.now().toString(36)}`);
      cy.contains("button", "Next").click();
      cy.get('[data-testid="create-agent-execution-environment"]').contains("button", "Celln").click();
      cy.contains("button", "Next").click();
      // Straight to tools: no "Choose a native Celln runtime" step in between.
      cy.get('[data-testid="create-agent-borrowed-tools"]').should("be.visible");
      cy.contains("Choose a native Celln runtime").should("not.exist");
      cy.contains("needs an approving policy").should("not.exist");
      cy.contains("button", "Next").click();
      // The Provider step is where the backend is chosen.
      cy.get('[data-testid="platform-model-route"]').should("be.visible").and("contain", "DeepSeek");
      cy.contains("button", "Next").should("be.visible").and("be.enabled");
    });
  });

  // Name → Celln plane → tools: the point where the fleet flow starts to differ.
  const openToolsStep = () => {
    cy.visit("/agents?create=1&kind=agent#token=test-token");
    cy.get('[role="dialog"] input[placeholder="my-agent"]').type(`fleet-${Date.now().toString(36)}`);
    cy.get('[role="dialog"]').contains("button", "Next").click();
    cy.get('[data-testid="create-agent-execution-environment"]').contains("button", "Celln").click();
    cy.get('[role="dialog"]').contains("button", "Next").click();
    cy.get('[data-testid="create-agent-borrowed-tools"]').should("be.visible");
  };

  it("asks a fleet agent for no key and no model, and confirms the backend's fixed model", () => {
    openToolsStep();
    cy.get('[data-testid="wizard-steps"] [data-step]').then(($steps) => {
      expect([...$steps].map((el) => el.getAttribute("data-step"))).to.deep.equal(["name", "plane", "tools", "provider", "confirm"]);
    });
    cy.get('[data-testid="wizard-steps"]').should("not.contain", "Auth").and("not.contain", "Model");
    // The header counts what the policy lends, not the platform's cap of 24.
    cy.get('[data-testid="borrowed-tools-count"]').should("have.text", "2 of 2 lent tools selected");
    cy.get('[role="dialog"]').contains("button", "Next").click();
    cy.get('[data-testid="fleet-backend-model"]').should("contain", "Model: deepseek-chat — fixed by fleet backend native");
    cy.get('[data-testid="platform-model-route"]').invoke("text").should("not.match", /wrapper|runtime profile|model route/i);
    cy.get('[role="dialog"]').contains("button", "Next").click();
    // Provider leads straight to Confirm.
    cy.get('[data-testid="execution-confirmation"]').should("be.visible");
    cy.get('[data-testid="fleet-fixed-model"]').should("have.text", "Model: deepseek-chat — fixed by fleet backend native");
    cy.get('[role="dialog"] input#native-model').should("not.exist");
  });

  it("shows a new fleet backend's progress, including the 90-second wait, until it is ready", () => {
    const added = { ...profile("openai", "gpt-4o-mini"), endpoint: "https://api.openai.com/v1/chat/completions" };
    const states = ["pending: waiting for the nodes to configure it", "configuring: waiting 90s for the credential to reach running dispatchers", "ready"];
    let posted = false;
    let polls = 0;
    cy.intercept("POST", "**/api/v1/celln-platform/backends*", (request) => {
      expect(request.body).to.include({ name: "openai", provider: "openai", model: "gpt-4o-mini", credential: "sk-test" });
      posted = true;
      request.reply({ statusCode: 202, body: { name: "openai", provider: "openai", protocol: "openai-chat", endpoint: added.endpoint, model: added.model, allowInsecure: false, source: "added", profile: added.name, state: states[0] } });
    }).as("add");
    const current = () => states[Math.min(Math.max(polls - 1, 0), states.length - 1)];
    cy.intercept("GET", "**/api/v1/celln-platform/backends*", (request) => {
      if (posted) polls++;
      request.reply({ body: posted ? [{ name: "openai", provider: "openai", protocol: "openai-chat", endpoint: added.endpoint, model: added.model, allowInsecure: false, source: "added", profile: added.name, state: current() }] : [] });
    }).as("backends");
    // The profile appears once the backend is ready.
    cy.intercept("GET", "**/api/v1/celln-platform/profiles*", (request) => request.reply({ body: posted && current() === "ready" ? [...profiles, added] : profiles }));

    openToolsStep();
    cy.get('[role="dialog"]').contains("button", "Next").click();
    cy.get('[data-testid="platform-model-route"] button[role=combobox]').first().click();
    cy.get("[role=option]").contains("OpenAI").click();
    // No OpenAI backend yet: the add form, and the step cannot be completed.
    cy.get('[data-testid="celln-provider-add-backend"]').should("contain", "The fleet has no fleet backend for OpenAI yet");
    cy.get('[role="dialog"]').contains("button", "Next").should("be.disabled");
    cy.get('[data-testid="celln-provider-add-backend"] input[type=password]').type("sk-test");
    cy.contains("button", "Add to the fleet").click();
    cy.wait("@add");

    const stage = (name: string) => cy.contains('[data-testid="fleet-backend-progress"] li', name, { timeout: 10000 });
    stage("Recorded").should("have.attr", "data-stage", "done");
    stage("Nodes configuring").should("have.attr", "data-stage", "active");
    stage("Publishing to namespaces").should("have.attr", "data-stage", "todo");
    cy.get('[data-testid="celln-provider-add-backend"]').should("not.exist");
    cy.get('[role="dialog"]').contains("button", "Next").should("be.disabled");

    stage("Publishing to namespaces").should("have.attr", "data-stage", "active");
    stage("Nodes configuring").should("have.attr", "data-stage", "done");
    cy.get('[data-testid="fleet-backend-progress"]').should("contain", "Waiting about 90 seconds for the key to reach the running dispatchers").and("contain", "s of about 90s");

    // Ready: bound on its own, and the step can be completed.
    cy.get('[data-testid="fleet-backend-model"]', { timeout: 15000 }).should("contain", "Model: gpt-4o-mini — fixed by fleet backend openai");
    cy.get('[data-testid="fleet-backend-progress"]').should("not.exist");
    cy.get('[role="dialog"]').contains("button", "Next").should("be.enabled").click();
    cy.get('[data-testid="fleet-fixed-model"]').should("contain", "gpt-4o-mini").and("contain", "fixed by fleet backend openai");
  });

  // A reasoning model can spend Celln's 512 output tokens per request thinking
  // and answer nothing; the fleet backend's model parameters switch that off.
  it("sends model parameters with a new fleet backend, and refuses invalid ones before posting", () => {
    const thinkingOff = { chat_template_kwargs: { enable_thinking: false } };
    const added = { ...profile("custom", "qwen3"), endpoint: "https://models.example/v1/chat/completions" };
    const listed = { name: "custom", provider: "custom", protocol: "openai-chat", endpoint: added.endpoint, model: added.model, allowInsecure: false, parameters: thinkingOff, source: "added", profile: added.name };
    let posts = 0;
    cy.intercept("POST", "**/api/v1/celln-platform/backends*", (request) => {
      posts++;
      expect(request.body).to.deep.include({ name: "custom", provider: "custom", model: "qwen3", protocol: "openai-chat", parameters: thinkingOff });
      request.reply({ statusCode: 202, body: { ...listed, state: "pending: waiting for the nodes to configure it" } });
    }).as("add");
    cy.intercept("GET", "**/api/v1/celln-platform/backends*", (request) => request.reply({ body: posts ? [{ ...listed, state: "ready" }] : [] }));
    cy.intercept("GET", "**/api/v1/celln-platform/profiles*", (request) => request.reply({ body: posts ? [...profiles, added] : profiles }));

    openToolsStep();
    cy.get('[role="dialog"]').contains("button", "Next").click();
    cy.get('[data-testid="platform-model-route"] button[role=combobox]').first().click();
    cy.get("[role=option]").contains("Custom").click();
    cy.get('[data-testid="celln-provider-add-backend"]').within(() => {
      cy.contains("label", "Model").find("input").type("qwen3");
      cy.contains("label", "Chat endpoint").find("input").type("https://models.example/v1");
      cy.get("input[type=password]").type("sk-test-credential-000000001");
      cy.contains("summary", "Advanced: model parameters").click();
      cy.get('[data-testid="celln-model-parameters"]').should("contain", "Celln allows 512 output tokens per request; a reasoning model can spend them all thinking and return nothing.");

      // A reserved key: the precise rule inline, and nothing is posted.
      cy.get('[data-testid="celln-model-parameters-json"]').type('{"max_tokens": 4096}', { parseSpecialCharSequences: false });
      cy.get('[data-testid="celln-model-parameters-error"]').should("have.text", 'model parameters: "max_tokens" is reserved (Celln sets it on every request)');
      cy.contains("button", "Add to the fleet").click();
      cy.contains('[role="alert"]', "Fix the model parameters first.").should("be.visible");

      // Not JSON: the checkbox cannot edit it, and nothing is posted.
      cy.get('[data-testid="celln-model-parameters-json"]').clear().type('{"temperature": ', { parseSpecialCharSequences: false });
      cy.get('[data-testid="celln-model-parameters-error"]').should("contain", "model parameters: not valid JSON");
      cy.get('[data-testid="celln-disable-thinking"]').should("be.disabled");
      cy.contains("button", "Add to the fleet").click();
      cy.then(() => expect(posts).to.equal(0));

      // The checkbox and the JSON edit the same object.
      cy.get('[data-testid="celln-model-parameters-json"]').clear();
      cy.get('[data-testid="celln-model-parameters-error"]').should("not.exist");
      cy.get('[data-testid="celln-disable-thinking"]').should("not.be.checked").check();
      cy.get('[data-testid="celln-model-parameters-json"]').invoke("val").then((text) => expect(JSON.parse(String(text))).to.deep.equal(thinkingOff));
      cy.get('[data-testid="celln-model-parameters-json"]').clear().type('{"chat_template_kwargs": {"enable_thinking": false}, "top_k": 40}', { parseSpecialCharSequences: false });
      cy.get('[data-testid="celln-disable-thinking"]').should("be.checked").uncheck();
      cy.get('[data-testid="celln-model-parameters-json"]').invoke("val").then((text) => expect(JSON.parse(String(text))).to.deep.equal({ top_k: 40 }));
      cy.get('[data-testid="celln-model-parameters-json"]').clear();
      cy.get('[data-testid="celln-disable-thinking"]').check();
      cy.contains("button", "Add to the fleet").click();
    });
    cy.wait("@add").its("request.body.parameters").should("deep.equal", thinkingOff);
    cy.then(() => expect(posts).to.equal(1));

    // Bound once ready; the fixed-model line says what the backend sends.
    cy.get('[data-testid="fleet-backend-model"]', { timeout: 15000 }).should("contain", "Model: qwen3 — fixed by fleet backend custom");
    cy.get('[data-testid="fleet-backend-parameters"]').should("contain", '{"chat_template_kwargs":{"enable_thinking":false}}')
      .and("have.attr", "title", '{"chat_template_kwargs":{"enable_thinking":false}}');
  });

  // A reasoning model left thinking needs more than Celln's 512 output tokens
  // per request; the fleet backend may allow up to 4096, at 6× that per turn.
  it("sends max output tokens per request with a new fleet backend, refuses a value out of range, and shows the API's warning", () => {
    const added = { ...profile("custom", "qwq"), endpoint: "https://models.example/v1/chat/completions" };
    const listed = { name: "custom", provider: "custom", protocol: "openai-chat", endpoint: added.endpoint, model: added.model, allowInsecure: false, maxOutputTokens: 4096, source: "added", profile: added.name };
    const warning = "a turn on backend custom reserves 24576 output tokens (6 requests × 4096), and this fleet's ceilings (1536 model requests, 786432 output tokens per conversation) pay for 32 such turns, fewer than the usual 64.";
    let posts = 0;
    cy.intercept("POST", "**/api/v1/celln-platform/backends*", (request) => {
      posts++;
      expect(request.body).to.deep.include({ name: "custom", provider: "custom", model: "qwq", protocol: "openai-chat", maxOutputTokens: 4096 });
      expect(request.body).not.to.have.property("parameters");
      request.reply({ statusCode: 202, body: { ...listed, warning, state: "pending: waiting for the nodes to configure it" } });
    }).as("add");
    cy.intercept("GET", "**/api/v1/celln-platform/backends*", (request) => request.reply({ body: posts ? [{ ...listed, state: "ready" }] : [] }));
    cy.intercept("GET", "**/api/v1/celln-platform/profiles*", (request) => request.reply({ body: posts ? [...profiles, added] : profiles }));

    openToolsStep();
    cy.get('[role="dialog"]').contains("button", "Next").click();
    cy.get('[data-testid="platform-model-route"] button[role=combobox]').first().click();
    cy.get("[role=option]").contains("Custom").click();
    cy.get('[data-testid="celln-provider-add-backend"]').within(() => {
      cy.contains("label", "Model").find("input").type("qwq");
      cy.contains("label", "Chat endpoint").find("input").type("https://models.example/v1");
      cy.get("input[type=password]").type("sk-test-credential-000000001");
      cy.contains("summary", "Advanced: model parameters and output tokens").click();
      cy.get('[data-testid="celln-max-output-tokens"]').should("have.attr", "placeholder", "512").and("have.attr", "min", "256").and("have.attr", "max", "4096").and("have.value", "");
      cy.get('[data-testid="celln-max-output-tokens-guidance"]').should("contain", "512 suits chat with thinking disabled.")
        .and("contain", "A reasoning model left thinking needs 2048–4096, which costs 4–8× the tokens per turn. The turn limit grows with it: 60 seconds at 512, 4 minutes at 2048, 5 minutes at most.");
      cy.get('[data-testid="celln-turn-reservation"]').should("have.text", "one turn reserves 3072 tokens (6 requests)");

      // Out of range: the rule inline, no computed line, and nothing is posted.
      for (const bad of ["255", "4097", "12.5"]) {
        cy.get('[data-testid="celln-max-output-tokens"]').clear().type(bad);
        cy.get('[data-testid="celln-max-output-tokens-error"]').should("have.text", "max output tokens per request must be a whole number from 256 to 4096 (leave it blank for the default 512)");
        cy.get('[data-testid="celln-turn-reservation"]').should("not.exist");
      }
      cy.contains("button", "Add to the fleet").click();
      cy.contains('[role="alert"]', "Fix the max output tokens per request first.").should("be.visible");
      cy.then(() => expect(posts).to.equal(0));

      cy.get('[data-testid="celln-max-output-tokens"]').clear().type("2048");
      cy.get('[data-testid="celln-max-output-tokens-error"]').should("not.exist");
      cy.get('[data-testid="celln-turn-reservation"]').should("have.text", "one turn reserves 12288 tokens (6 requests)");
      cy.get('[data-testid="celln-max-output-tokens"]').clear().type("4096");
      cy.get('[data-testid="celln-turn-reservation"]').should("have.text", "one turn reserves 24576 tokens (6 requests)");
      cy.contains("button", "Add to the fleet").click();
    });
    cy.wait("@add").its("request.body.maxOutputTokens").should("equal", 4096);
    cy.then(() => expect(posts).to.equal(1));
    cy.get('[data-testid="fleet-backend-warning"]').should("contain", "Added, with a warning").and("contain", "pay for 32 such turns, fewer than the usual 64");

    // Bound once ready; the fixed-model line names the raised cap.
    cy.get('[data-testid="fleet-backend-model"]', { timeout: 15000 }).should("contain", "Model: qwq — fixed by fleet backend custom");
    cy.get('[data-testid="fleet-backend-max-output-tokens"]').should("have.text", "up to 4096 output tokens per request")
      .and("have.attr", "title", "one turn reserves 24576 tokens (6 requests)");
    cy.get('[data-testid="fleet-backend-parameters"]').should("not.exist");
  });

  it("lists a fleet backend's max output tokens on the Harness tab only when not the default, and sends nothing for 512", () => {
    cy.intercept("GET", "**/api/v1/agents/hermes*", { body: hermes });
    cy.intercept("GET", "**/api/v1/celln-platform/backends*", {
      body: [
        { name: "native", provider: "deepseek", protocol: "openai-chat", endpoint: "https://api.deepseek.com/chat/completions", model: "deepseek-chat", allowInsecure: false, source: "install", profile: "celln-native-starter", state: "ready" },
        { name: "thinker", provider: "llama-server", protocol: "openai-chat", endpoint: "http://framework:8080/v1/chat/completions", model: "qwq.gguf", allowInsecure: true, maxOutputTokens: 2048, source: "added", profile: "celln-native-starter-thinker", state: "ready" },
      ],
    });
    cy.intercept("POST", "**/api/v1/celln-platform/backends*", (request) => {
      expect(request.body).to.deep.include({ name: "plain", provider: "llama-server", endpoint: "http://framework:8080", allowInsecure: true });
      expect(request.body).not.to.have.property("maxOutputTokens");
      request.reply({ statusCode: 202, body: { name: "plain", provider: "llama-server", protocol: "openai-chat", endpoint: "http://framework:8080/v1/chat/completions", model: "qwq.gguf", allowInsecure: true, source: "added", profile: "p", state: "pending: waiting for the nodes to configure it" } });
    }).as("add");
    cy.visit("/agents/hermes?tab=harness#token=test-token");
    cy.get('[data-testid="celln-backend-thinker-max-output-tokens"]').should("have.text", "up to 2048 output tokens per request");
    cy.get('[data-testid="celln-backend-native"]').should("not.contain", "output tokens per request");

    cy.get('[data-testid="celln-add-backend"]').within(() => {
      cy.contains("button", "Add a backend").click();
      cy.get("button[role=combobox]").click();
    });
    cy.get("[role=option]").contains("llama-server").click();
    cy.get('[data-testid="celln-add-backend"]').within(() => {
      cy.contains("label", "Name").find("input").clear().type("plain");
      cy.contains("label", "Chat endpoint").find("input").type("http://framework:8080");
      cy.contains("summary", "Advanced: model parameters and output tokens").click();
      cy.get('[data-testid="celln-max-output-tokens"]').type("100");
      cy.contains("button", "Add to the fleet").click();
      cy.contains('[role="alert"]', "Fix the max output tokens per request first.").scrollIntoView().should("be.visible");
      // The default, stated, is the same backend as one that never named it.
      cy.get('[data-testid="celln-max-output-tokens"]').clear().type("512");
      cy.get('[data-testid="celln-turn-reservation"]').should("have.text", "one turn reserves 3072 tokens (6 requests)");
      cy.contains("button", "Add to the fleet").click();
    });
    cy.wait("@add");
    cy.get('[data-testid="celln-add-backend-warning"]').should("not.exist");
  });

  it("lists a fleet backend's model parameters on the Agent's Harness tab and offers the thinking switch for llama-server", () => {
    const thinkingOff = { chat_template_kwargs: { enable_thinking: false } };
    cy.intercept("GET", "**/api/v1/agents/hermes*", { body: hermes });
    cy.intercept("GET", "**/api/v1/celln-platform/backends*", {
      body: [
        { name: "native", provider: "deepseek", protocol: "openai-chat", endpoint: "https://api.deepseek.com/chat/completions", model: "deepseek-chat", allowInsecure: false, source: "install", profile: "celln-native-starter", state: "ready" },
        { name: "qwen-quiet", provider: "llama-server", protocol: "openai-chat", endpoint: "http://framework:8080/v1/chat/completions", model: "qwen.gguf", allowInsecure: true, parameters: thinkingOff, source: "added", profile: "celln-native-starter-qwen-quiet", state: "ready" },
      ],
    });
    cy.intercept("POST", "**/api/v1/celln-platform/backends*", (request) => {
      expect(request.body).to.deep.include({ name: "qwen-2", provider: "llama-server", endpoint: "http://framework:8080", allowInsecure: true, parameters: thinkingOff });
      request.reply({ statusCode: 202, body: { name: "qwen-2", provider: "llama-server", protocol: "openai-chat", endpoint: "http://framework:8080/v1/chat/completions", model: "qwen.gguf", allowInsecure: true, parameters: thinkingOff, source: "added", profile: "p", state: "pending: waiting for the nodes to configure it" } });
    }).as("add");
    cy.visit("/agents/hermes?tab=harness#token=test-token");
    cy.get('[data-testid="celln-backend-qwen-quiet-parameters"]').should("contain", 'sends {"chat_template_kwargs":{"enable_thinking":false}}')
      .and("have.attr", "title", '{"chat_template_kwargs":{"enable_thinking":false}}');
    cy.get('[data-testid="celln-backend-native"]').should("not.contain", "sends");

    cy.get('[data-testid="celln-add-backend"]').within(() => {
      cy.contains("button", "Add a backend").click();
      // DeepSeek takes no chat-template switch; only the JSON is offered.
      cy.contains("summary", "Advanced: model parameters").click();
      cy.get('[data-testid="celln-disable-thinking"]').should("not.exist");
      cy.get("button[role=combobox]").click();
    });
    cy.get("[role=option]").contains("llama-server").click();
    cy.get('[data-testid="celln-add-backend"]').within(() => {
      cy.contains("label", "Name").find("input").clear().type("qwen-2");
      cy.contains("label", "Chat endpoint").find("input").type("http://framework:8080");
      cy.get('[data-testid="celln-disable-thinking"]').check();
      cy.contains("button", "Add to the fleet").click();
    });
    cy.wait("@add");
  });
});

// A namespace-native Celln runtime (no fleet backend) still asks for the key
// and the model: only a fleet backend fixes them.
describe("Create Agent on a namespace-native Celln runtime", () => {
  it("keeps the Auth and Model steps", () => {
    cy.intercept("GET", "**/api/v1/**", { body: [] });
    cy.intercept("GET", "**/api/v1/runtimes*", {
      body: [{ metadata: { name: "native-local", namespace: "default" }, spec: { image: "", celln: { contractVersion: "celln.json-tools/v1" }, supportOwner: "op" } }],
    });
    cy.intercept("GET", "**/api/v1/capabilities*", { body: { celln: { available: true, state: "ready", reason: "ready" } } });
    cy.visit("/agents?create=1&kind=agent#token=test-token");
    cy.get('[role="dialog"] input[placeholder="my-agent"]').type(`native-${Date.now().toString(36)}`);
    cy.get('[role="dialog"]').contains("button", "Next").click();
    cy.get('[data-testid="create-agent-execution-environment"]').contains("button", "Celln").click();
    cy.get('[data-testid="wizard-steps"] [data-step]').then(($steps) => {
      expect([...$steps].map((el) => el.getAttribute("data-step"))).to.deep.equal(["name", "plane", "tools", "provider", "apikey", "model", "confirm"]);
    });
    cy.get('[data-testid="wizard-steps"]').should("contain", "Auth").and("contain", "Model");
    cy.get('[role="dialog"]').contains("button", "Next").click();
    // Namespaced tools are "installed", never "lent".
    cy.get('[data-testid="create-agent-borrowed-tools"]').should("be.visible").and("not.contain", "lent tools");
    cy.get('[role="dialog"]').contains("button", "Next").click();
    cy.get('[data-testid="platform-model-route"]').should("not.exist");
    cy.contains("label", "AI Provider").should("be.visible");
    cy.get('[role="dialog"]').contains("button", "Next").click();
    cy.get('[role="dialog"]').contains("button", "Next").click();
    cy.get("#native-model").should("be.visible");
  });
});
