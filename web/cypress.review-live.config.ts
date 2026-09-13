import { defineConfig } from "cypress";

// Explicit isolated HTTPS review deployment only, with an operator-supplied
// private token-file path. No kubeconfig or credential discovery.
export default defineConfig({
  e2e: { supportFile: false, specPattern: "cypress/e2e/celln-review-live.cy.ts", viewportWidth: 1440, viewportHeight: 1100 },
  video: false,
  screenshotOnRunFailure: false,
  defaultCommandTimeout: 15000,
});
