describe("installed isolated Celln UX", () => {
  beforeEach(() => {
    expect(Cypress.config("baseUrl"), "explicit review HTTPS URL").to.match(/^https:\/\//);
    expect(Cypress.env("REVIEW_TOKEN_FILE"), "explicit private review token path").to.be.a("string");
    cy.visit("/login");
    cy.readFile(Cypress.env("REVIEW_TOKEN_FILE"), { log: false }).then((token: string) => {
      cy.get("#token").type(token.trim(), { log: false });
    });
    cy.get('button[type="submit"]').click();
    cy.contains("h1", "Celln workspace").should("be.visible");
    cy.get('[data-testid="workspace-model"]').should("contain", "review-uppercase");
  });

  for (const mode of ["direct", "model"]) {
    it(`${mode}: real result, provenance and retained cleanup evidence`, () => {
      cy.get('[data-testid="review-mode"]').select(mode);
      cy.get('[data-testid="review-task"]').clear().type("celln");
      cy.get('[data-testid="review-create"]').click();
      cy.get('[data-testid="review-selected"]').find("h2").should("contain", `ux-${mode}-`);
      cy.get('[data-testid="review-result"]', { timeout: 150000 }).should("contain", "CELLN");
      cy.get('[data-testid="celln-scoped-cleanup"]', { timeout: 150000 }).should("contain", "Confirmed by the scoped cleanup record");
      cy.get('[data-testid="review-stop"]').click();
      cy.get('[data-testid="review-stop-status"]', { timeout: 150000 }).should("contain", "Stopped — native cleanup confirmed");
      cy.screenshot(`installed-${mode}-cleanup`, { capture: "fullPage" });
    });
  }

  it("enduring: continuation, original API ceiling, refresh and confirmed stop", () => {
    cy.get('[data-testid="review-mode"]').select("enduring");
    cy.get('[data-testid="review-task"]').clear().type("celln");
    cy.get('[data-testid="review-create"]').click();
    cy.get('[data-testid="review-selected"]').find("h2").should("contain", "ux-enduring-");
    cy.get('[data-testid="review-result"]', { timeout: 150000 }).should("have.text", "CELLN");
    cy.get('[data-testid="celln-turn-message"]', { timeout: 150000 }).should("be.enabled").type("violet");
    cy.get('[data-testid="celln-turn-send"]').should("be.enabled").click();
    cy.get('[data-testid="celln-conversation"]').contains("VIOLET", { timeout: 150000 }).should("be.visible");
    cy.get('[data-testid="celln-turn-message"]').should("be.disabled");
    cy.get('[data-testid="review-limit"]').click();
    cy.get('[data-testid="review-limit-result"]').should("contain", "API response").and("contain", "parent turn allowance is exhausted");
    cy.screenshot("installed-enduring-continuation", { capture: "fullPage" });
    cy.reload();
    cy.get('[data-testid="celln-conversation"]').contains("VIOLET").should("be.visible");
    cy.get('[data-testid="celln-turn-message"]').should("be.disabled");
    cy.get('[data-testid="review-stop"]').click();
    cy.get('[data-testid="review-stop-status"]', { timeout: 150000 }).should("contain", "Stopped — native cleanup confirmed");
    cy.screenshot("installed-enduring-cleanup", { capture: "fullPage" });
  });
});
