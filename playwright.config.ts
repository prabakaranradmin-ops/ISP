import { defineConfig, devices } from '@playwright/test';

/**
 * Browser end-to-end tests for the subscriber portal.
 *
 * These run against a stack already brought up with ./scripts/demo_up.sh —
 * deliberately not via Playwright's `webServer`, which would start and stop the
 * stack per run. Bringing up PostgreSQL, three Sentinels, RADIUS and the API
 * takes minutes and reseeds the database, so owning that lifecycle here would
 * make the suite slow and would silently discard whatever state a tester was
 * looking at.
 */
export default defineConfig({
  testDir: './e2e',
  // e2e/demo holds the narrated walkthrough recordings, which belong to
  // playwright.demo.config.ts: they pause deliberately (DEMO_READ_MS) so a
  // viewer can follow along, and under this config's 30s timeout all five
  // fail on page.waitForTimeout. Excluded so `npx playwright test` reports
  // only behaviour — five timeouts in a run otherwise read as regressions
  // and cost a diagnosis before the cause (a config meant for recording)
  // becomes apparent. Record them with ./scripts/record_demo.sh.
  testIgnore: '**/demo/**',
  outputDir: './e2e/test-results',
  fullyParallel: false, // the demo stack has one subscriber; parallel logins race over the same session
  forbidOnly: !!process.env.CI,
  retries: 0,
  workers: 1,
  reporter: [['list'], ['html', { outputFolder: 'e2e/playwright-report', open: 'never' }]],

  use: {
    baseURL: process.env.PORTAL_URL ?? 'https://localhost',

    // The demo issues its own certificate rather than buying a public one, so
    // every request would otherwise fail certificate validation. This is a
    // property of the demo environment, not something the portal asks of real
    // browsers.
    ignoreHTTPSErrors: true,

    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
  },

  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
  ],
});
