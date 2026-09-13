describe("Celln product workspace with real provider", () => {
  beforeEach(() => {
    expect(Cypress.config("baseUrl")).to.match(/^https:\/\//);
    expect(Cypress.env("REVIEW_TOKEN_FILE")).to.be.a("string");
    cy.visit("/login");
    cy.readFile(Cypress.env("REVIEW_TOKEN_FILE"), { log: false }).then((token: string) => cy.get("#token").type(token.trim(), { log: false }));
    cy.get('button[type="submit"]').click();
    cy.contains("h1", "Celln workspace").should("be.visible");
    cy.get('[data-testid="workspace-model"]').should("contain", "deepseek-v4-flash");
  });

  it("answers Cairo directly through a real LLM, then confirms one-shot cleanup", () => {
    cy.get('[data-testid="review-mode"]').select("model");
    cy.get('[data-testid="review-task"]').clear().type("Tell me where Cairo is. Answer in one sentence.");
    cy.get('[data-testid="review-create"]').click();
    cy.get('[data-testid="review-selected"] h2').should("contain", "ux-model-");
    cy.get('[data-testid="review-result"]', { timeout: 180000 }).should("contain", "Egypt");
    cy.get('[data-testid="celln-scoped-cleanup"]', { timeout: 180000 }).should("contain", "Confirmed by the scoped cleanup record");
    cy.screenshot("real-llm-cairo-one-shot", { capture: "fullPage" });
    cy.get('[data-testid="review-stop"]').click();
    cy.get('[data-testid="review-stop-status"]').should("contain", "cleanup confirmed");
  });

  it("runs concurrent parents, retains context, invokes a tool, enforces limits and stops both", () => {
    let first = "", second = "";
    cy.intercept("POST", "/api/v1/review/runs").as("start");
    cy.get('[data-testid="review-mode"]').select("enduring");
    cy.get('[data-testid="review-task"]').clear().type("Where is Cairo? Answer briefly.");
    cy.get('[data-testid="review-create"]').click();
    cy.wait("@start").then(({ response }) => { expect(response?.statusCode).eq(200); first = response!.body.metadata.name; });
    cy.get('[data-testid="review-result"]', { timeout: 180000 }).should("contain", "Egypt");
    cy.get('[data-testid="review-task"]').clear().type("What is the capital of France? Answer briefly.");
    cy.get('[data-testid="review-create"]').click();
    cy.wait("@start").then(({ response }) => { expect(response?.statusCode).eq(200); second = response!.body.metadata.name; });
    cy.get('[data-testid="review-result"]', { timeout: 180000 }).should("contain", "Paris");
    cy.then(() => {
      cy.contains('[data-testid="review-history"]', second).should("contain", "Running");
      cy.contains('[data-testid="review-history"]', first).should("contain", "Running").click();
      cy.writeFile("/tmp/celln-product-browser-ids.json", { first, second }, { log: false });
    });
    cy.get('[data-testid="celln-turn-message"]', { timeout: 180000 }).should("be.enabled").type("What river runs through that city? Give just the river name.");
    cy.get('[data-testid="celln-turn-send"]').click();
    cy.get('[data-testid="celln-conversation"]').contains("Agent: Nile", { timeout: 180000 }).should("be.visible");
    cy.get('[data-testid="celln-turn-message"]', { timeout: 180000 }).should("be.enabled").type("Call the uppercase tool with text sYmPoZiUm. Return only the tool result.");
    cy.get('[data-testid="celln-turn-send"]').click();
    cy.get('[data-testid="celln-conversation"]').contains("Agent: SYMPOZIUM", { timeout: 180000 }).should("be.visible");
    cy.get('[data-testid="celln-turn-message"]', { timeout: 180000 }).should("be.enabled").type("Name the country we discussed initially. Reply with only its name.");
    cy.get('[data-testid="celln-turn-send"]').click();
    cy.get('[data-testid="celln-conversation"]').contains("Agent: Egypt", { timeout: 180000 }).should("be.visible");
    cy.get('[data-testid="celln-turn-message"]').should("be.disabled");
    cy.get('[data-testid="review-limit"]').click();
    cy.get('[data-testid="review-limit-result"]').should("contain", "parent turn allowance is exhausted");
    cy.screenshot("real-llm-concurrent-context-tool-budget", { capture: "fullPage" });
    cy.reload();
    cy.then(() => cy.contains('[data-testid="review-history"]', first).click());
    cy.get('[data-testid="celln-conversation"]').contains("Agent: SYMPOZIUM").should("be.visible");
    cy.get('[data-testid="celln-turn-message"]').should("be.disabled");
    cy.get('[data-testid="review-stop"]').click();
    cy.get('[data-testid="review-stop-status"]', { timeout: 180000 }).should("contain", "cleanup confirmed");
    cy.screenshot("real-llm-cleanup", { capture: "fullPage" });
    cy.then(() => cy.contains('[data-testid="review-history"]', second).click());
    cy.get('[data-testid="review-stop"]').click();
    cy.get('[data-testid="review-stop-status"]', { timeout: 180000 }).should("contain", "cleanup confirmed");
  });
});
