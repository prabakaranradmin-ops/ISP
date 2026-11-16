import type { Page } from '@playwright/test';

/**
 * Shared helpers for the demo recordings.
 *
 * The captions are the point. A screen recording of a web app without narration
 * shows that something happened but not what it meant — and these flows turn on
 * things that are invisible unless named, like a panel being absent because of
 * the viewer's role rather than because the record is empty.
 */

/** How long a caption stays on screen before the next action, in ms. */
const READ_MS = Number(process.env.DEMO_READ_MS ?? 2200);

/** Longer hold for a point worth dwelling on. */
const BEAT_MS = Number(process.env.DEMO_BEAT_MS ?? 3400);

const CAPTION_ID = 'demo-caption';
const CHROME_ID = 'demo-chrome';

/**
 * Draw (or update) the caption bar.
 *
 * Re-injected on every call rather than once per page, because a full
 * navigation destroys the DOM and would silently take the narration with it.
 *
 * The sub-caption is a positional string rather than an options field on
 * purpose. It was previously `opts.sub`, and calling `say(page, text, 'detail')`
 * — the obvious shape — silently read `String.prototype.sub`, a legacy method
 * that returns a native function, which Playwright then failed to serialize.
 * Making the common call the correct one removes the trap.
 */
export async function say(page: Page, text: string, sub = '', opts: { hold?: number } = {}) {
  await page.evaluate(
    ({ text, sub, capId }) => {
      let bar = document.getElementById(capId);
      if (!bar) {
        bar = document.createElement('div');
        bar.id = capId;
        bar.style.cssText = [
          'position:fixed', 'left:0', 'right:0', 'bottom:0', 'z-index:2147483647',
          'background:linear-gradient(to top, rgba(9,15,20,.97), rgba(9,15,20,.92))',
          'color:#F1F4F7', 'padding:16px 28px 18px', 'text-align:center',
          'font-family:system-ui,-apple-system,"Segoe UI",sans-serif',
          'border-top:3px solid #F0A62A', 'pointer-events:none',
        ].join(';');
        document.body.appendChild(bar);
      }
      bar.innerHTML =
        `<div style="font-size:19px;font-weight:600;line-height:1.35">${text}</div>` +
        (sub
          ? `<div style="font-size:14px;color:#A9B9C6;margin-top:5px;line-height:1.4">${sub}</div>`
          : '');
    },
    { text, sub, capId: CAPTION_ID },
  );
  await page.waitForTimeout(opts.hold ?? READ_MS);
}

/** Hold on the current frame — for letting a point land. */
export async function beat(page: Page, ms = BEAT_MS) {
  await page.waitForTimeout(ms);
}

/**
 * Pin a persona badge to the top-right for the rest of the recording, so a
 * viewer who joins mid-video knows whose screen they are looking at.
 */
export async function persona(page: Page, name: string, role: string) {
  await page.evaluate(
    ({ name, role, chromeId }) => {
      let tag = document.getElementById(chromeId);
      if (!tag) {
        tag = document.createElement('div');
        tag.id = chromeId;
        tag.style.cssText = [
          'position:fixed', 'top:0', 'right:0', 'z-index:2147483647',
          'background:#0F1922', 'color:#F1F4F7', 'padding:9px 16px',
          'font-family:ui-monospace,Consolas,monospace', 'font-size:13px',
          'border-bottom-left-radius:6px', 'pointer-events:none',
        ].join(';');
        document.body.appendChild(tag);
      }
      tag.innerHTML =
        `<span style="color:#F0A62A;font-weight:600">${name}</span>` +
        `<span style="color:#7C8E9C"> · ${role}</span>`;
    },
    { name, role, chromeId: CHROME_ID },
  );
}

/** Re-apply persona chrome + caption after a navigation wipes the DOM. */
export async function reframe(page: Page, name: string, role: string, text: string, sub?: string) {
  await persona(page, name, role);
  await say(page, text, sub);
}

export async function staffSignIn(page: Page, username: string) {
  await page.goto('/staff/login');
  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password').fill('staffpassword');
  await page.getByRole('button', { name: 'Sign in' }).click();
  // Sign-in lands on the dashboard; the recorded walkthroughs start from the
  // subscriber list, so go there to keep each demo's opening frame the same.
  await page.waitForURL(/\/staff\/dashboard/);
  await page.goto('/staff/subscribers');
}

export async function portalSignIn(page: Page, username: string) {
  await page.goto('/ui/login');
  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password').fill('testpassword');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.waitForURL(/\/ui\/dashboard/);
}
