// Intercepted manual-review contracts for scoped status. No native execution is
// claimed by these tests; live runtime proof belongs to the separate live suite.
describe("Scoped Celln run review", () => {
  beforeEach(() => {
    cy.intercept("GET", "/api/v1/**", { body: [] });
  });

  it("shows one-shot owner, native IDs, receipt, and unconfirmed cleanup honestly", () => {
    cy.intercept("GET", "/api/v1/runs/scoped-once*", { body: {
      metadata: { name: "scoped-once", namespace: "default", uid: "run-uid", generation: 2 },
      spec: { agentRef: "agent", backend: "celln", task: "sealed work", executionLifecycle: "one-shot", cellnSelection: { toolRefs: [] } },
      status: {
        phase: "Succeeded",
        result: "done",
        cellnScoped: {
          preparationName: "prepared", preparationUid: "prepared-uid", decisionName: "final", decisionUid: "final-uid",
          receiverId: "receiver-operation", owner: "native-owner", startAttempted: true, nativePhase: "Succeeded",
          receiptDigest: `sha256:${"a".repeat(64)}`, parentId: "native-parent", childId: "native-child", cellId: "native-cell",
        },
        conditions: [{ type: "CellnScopedExecution", status: "True", reason: "TerminalOwnerRecordObserved", message: "Terminal owner record observed; cleanup confirmation is pending", observedGeneration: 2 }],
      },
    } });
    cy.visit("/runs/scoped-once");
    cy.get('[data-testid="celln-scoped-mode"]').should("have.text", "One-shot scoped cell");
    cy.get('[data-testid="celln-scoped-execution"]').should("contain", "receiver-operation").and("contain", "native-owner")
      .and("contain", "native-parent").and("contain", "native-child").and("contain", "native-cell")
      .and("contain", `sha256:${"a".repeat(64)}`);
    cy.get('[data-testid="celln-scoped-cleanup"]').should("contain", "Not confirmed").and("contain", "terminal phase alone");
  });

  it("enables a turn for a genuinely ready scoped parent without legacy parent status", () => {
    cy.intercept("GET", "/api/v1/runs/scoped-parent*", { body: {
      metadata: { name: "scoped-parent", namespace: "default", uid: "parent-uid", generation: 3 },
      spec: { agentRef: "agent", backend: "celln", task: "Remember violet", executionLifecycle: "enduring", enduring: { maxTurns: 3 }, cellnSelection: { toolRefs: [] } },
      status: {
        phase: "Running", result: "Remembered violet",
        cellnScoped: {
          preparationName: "prepared", preparationUid: "prepared-uid", decisionName: "final", decisionUid: "final-uid",
          receiverId: "parent-operation", owner: "native-owner", parentIncarnation: `blake3:${"b".repeat(64)}`,
          startAttempted: true, nativePhase: "Running", receiptDigest: `sha256:${"c".repeat(64)}`, output: "Remembered violet", parentId: "native-parent",
        },
        conditions: [{ type: "CellnScopedExecution", status: "True", reason: "EnduringParentReady", message: "Original native parent remains running", observedGeneration: 3 }],
      },
    } });
    cy.intercept("GET", "/api/v1/runs/scoped-parent/turns*", { body: { runUID: "parent-uid", items: [], continue: "" } }).as("history");
    cy.intercept("POST", "/api/v1/runs/scoped-parent/turns*", (request) => {
      expect(request.body.runUID).to.eq("parent-uid");
      expect(request.body.message).to.eq("What colour?");
      request.reply({ statusCode: 202, body: {} });
    }).as("submit");
    cy.visit("/runs/scoped-parent");
    cy.wait("@history");
    cy.contains("Enduring scoped Celln conversation").should("be.visible");
    cy.get('[data-testid="celln-scoped-mode"]').should("have.text", "Enduring scoped parent");
    cy.get('[data-testid="celln-turn-message"]').should("be.enabled").type("What colour?");
    cy.get('[data-testid="celln-turn-send"]').should("be.enabled").click();
    cy.wait("@submit");
  });

  it("allows cancellation only for the scoped child bound to the live parent", () => {
    const incarnation = `blake3:${"d".repeat(64)}`;
    cy.intercept("GET", "/api/v1/runs/scoped-cancel*", { body: {
      metadata: { name: "scoped-cancel", namespace: "default", uid: "parent-uid", generation: 1 },
      spec: { agentRef: "agent", backend: "celln", task: "Initial", executionLifecycle: "enduring", enduring: { maxTurns: 4 }, cellnSelection: { toolRefs: [] } },
      status: { phase: "Running", cellnScoped: {
        preparationName: "prepared", preparationUid: "p", decisionName: "final", decisionUid: "d", receiverId: "parent-op", owner: "owner",
        parentIncarnation: incarnation, startAttempted: true, nativePhase: "Running", receiptDigest: `sha256:${"e".repeat(64)}`, output: "ready",
      }, conditions: [{ type: "CellnScopedExecution", status: "True", reason: "EnduringParentReady", message: "ready", observedGeneration: 1 }] },
    } });
    cy.intercept("GET", "/api/v1/runs/scoped-cancel/turns*", { body: { runUID: "parent-uid", continue: "", items: [{
      metadata: { name: "turn-one", uid: "turn-uid", generation: 1 }, spec: { runName: "scoped-cancel", runUID: "parent-uid", message: "Working" },
      status: { parentIncarnation: incarnation, cellnScoped: {
        preparationName: "turn-prepared", preparationUid: "tp", decisionName: "turn-final", decisionUid: "td",
        receiverId: "turn-op", owner: "owner", parentIncarnation: incarnation, turnId: "turn-uid", startAttempted: true,
        nativePhase: "Running", childId: "native-child", cellId: "native-cell",
      }, conditions: [{ type: "CellnTurnComplete", status: "False", reason: "Pending", message: "waiting", observedGeneration: 1 }] },
    }] } }).as("history");
    cy.intercept("POST", "/api/v1/runs/scoped-cancel/turns/turn-one/cancel*", (request) => {
      expect(request.body).to.deep.eq({ runUID: "parent-uid", turnUID: "turn-uid" });
      request.reply({ statusCode: 202, body: {} });
    }).as("cancel");
    cy.visit("/runs/scoped-cancel");
    cy.window().then((win) => cy.stub(win, "confirm").returns(true));
    cy.wait("@history");
    cy.get('[data-testid="celln-turn-cancel"]').should("be.enabled").click();
    cy.wait("@cancel");
    cy.get('[data-testid="celln-turn-cancel-pending"]').should("contain", "teardown is not confirmed");
  });
});
