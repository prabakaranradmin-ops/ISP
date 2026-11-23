// Config for the screen-capture pass, kept separate from the test suite the
// way playwright.demo.config.ts keeps the video recordings separate.
//
// The suite's own reporter only keeps images on failure, so a run where
// everything passes leaves nothing to look at — which is exactly the run you
// want to show somebody. This config exists to produce that: every console
// section and portal page, at desktop and at phone width, as PNGs plus an
// HTML index.
//
//   PORTAL_URL=https://127.0.0.1:18443 npx playwright test --config=playwright.capture.config.ts
import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './e2e',
  testMatch: '**/screens.capture.ts',
  // One worker: the pages are captured in a fixed order so the index reads
  // the same way every time.
  workers: 1,
  timeout: 300_000,
  reporter: [['list']],
  outputDir: './e2e/screenshots/_trace',
  use: {
    baseURL: process.env.PORTAL_URL ?? 'https://127.0.0.1:18443',
    // Self-signed, as the demo and the e2e suite both are.
    ignoreHTTPSErrors: true,
  },
});
