import { defineConfig } from "cypress";

// Real provider, real KVM. Explicit operator access; never discover credentials.
export default defineConfig({
  e2e: { supportFile: false, specPattern: "cypress/e2e/celln-product-live.cy.ts", viewportWidth: 1440, viewportHeight: 1100 },
  video: false, screenshotOnRunFailure: false, defaultCommandTimeout: 20000,
});
