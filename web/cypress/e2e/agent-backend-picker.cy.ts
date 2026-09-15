// Requires a fleet with at least two model backends offered to the namespace
// (test/integration/test-celln-fleet.sh with FLEET_SECOND_BACKEND leaves
// celln-agents-b that way). No fabricated API responses: the page rebinds a
// real Agent to another backend, asks it a question once, and reads the run.
const namespace = Cypress.env("FLEET_NAMESPACE") || "celln-agents-b";
const name = `cypress-backend-${Date.now()}`;
function request(method: string, path: string, body?: object) {
  return cy.request({ method, url: `/api/v1/${path}?namespace=${namespace}`, body,
    headers: { Authorization: `Bearer ${Cypress.env("API_TOKEN")}` }, failOnStatusCode: false });
}
function visit(path: string) {
  cy.visit(path, { onBeforeLoad(win) { win.localStorage.setItem("sympozium_namespace", namespace); } });
}

describe("Agent picks its fleet backend and lifecycle", () => {
  let profiles: { name: string; backend: string; wrapper: string; model: string; tools: { name: string; revision: string }[]; sessionDefaults: object }[] = [];
  before(() => {
    request("GET", "celln-platform/profiles").then((res) => {
      expect(res.status).to.equal(200);
      profiles = res.body;
      expect(profiles.length, "the namespace is offered at least two backends").to.be.gte(2);
      const first = profiles.find((profile) => profile.backend === "native") || profiles[0];
      request("POST", "celln-platform/wrappers", { profile: first.name }).its("status").should("equal", 200);
      request("POST", "agents", {
        name, model: first.model, runtimeRef: first.wrapper,
        execution: { backend: "celln", executionLifecycle: "enduring", modelConnectionRef: first.wrapper, model: first.model, enduring: first.sessionDefaults,
          cellnSelection: { runtimeRef: first.wrapper, toolRefs: [], clusterToolRefs: first.tools } },
      }).its("status").should("be.oneOf", [200, 201]);
    });
  });
  after(() => { request("DELETE", `agents/${name}`); });

  it("shows the backend the Agent runs on and switches it without YAML", () => {
    visit(`/agents/${name}?tab=harness`);
    cy.get('[data-testid="agent-backend-picker"]').should("be.visible").contains("Runs on");
    const other = () => profiles.find((profile) => profile.backend !== "native") || profiles[1];
    cy.get('[data-testid="agent-backend-picker"] button[role="combobox"]').click();
    cy.get('[role="option"]').contains(`${other().backend} — `).click();
    cy.get('[data-testid="agent-backend-picker"]').contains(`wrapper ${other().wrapper}`, { timeout: 20000 });
    request("GET", `agents/${name}`).then((res) => {
      expect(res.body.spec.runtimeRef).to.equal(other().wrapper);
      expect(res.body.spec.execution.modelConnectionRef).to.equal(other().wrapper);
      expect(res.body.spec.execution.model).to.equal(other().model);
      expect(res.body.spec.execution.cellnSelection.clusterToolRefs).to.have.length(other().tools.length);
    });
    cy.get('[data-testid="agent-shared-tools"]').should("contain", other().tools[0].name);
  });

  it("explains both lifecycles and answers once on the chosen backend", () => {
    visit(`/agents/${name}?tab=harness`);
    cy.get('[data-testid="agent-lifecycle"]').should("contain", "single-turn parent").and("contain", "follow-up turns");
    cy.get('[data-testid="celln-agent-first-message"]').type("Where is Botswana? Reply with one short sentence; do not use tools.");
    cy.get('[data-testid="celln-agent-once"]').check();
    cy.get('[data-testid="celln-agent-start"]').should("contain", "Ask once").click();
    cy.wait(2000);
    request("GET", "runs").then((res) => {
      const run = res.body.find((candidate: { spec: { agentRef: string; executionLifecycle?: string } }) => candidate.spec.agentRef === name);
      expect(run, "a one-shot run was created for the Agent").to.exist;
      expect(run.spec.executionLifecycle).to.equal("one-shot");
      expect(run.spec.enduring).to.be.undefined;
      const other = profiles.find((profile) => profile.backend !== "native") || profiles[1];
      expect(run.spec.cellnSelection.runtimeRef).to.equal(other.wrapper);
      const runName = run.metadata.name;
      const poll = (left: number) => {
        request("GET", `runs/${runName}`).then((current) => {
          const phase = current.body.status?.phase;
          if (phase === "Succeeded") {
            expect(current.body.status.result, "the answer").to.match(/Botswana/i);
            return;
          }
          expect(phase, `run ${runName} failed: ${current.body.status?.error}`).not.to.equal("Failed");
          expect(left, "one-shot did not finish in time").to.be.greaterThan(0);
          cy.wait(5000).then(() => poll(left - 1));
        });
      };
      poll(48);
    });
  });
});
