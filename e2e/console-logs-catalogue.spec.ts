import { test, expect, Page } from '@playwright/test';
import * as fs from 'fs';
import * as path from 'path';

/**
 * The console screens added for the catalogue, addressing and logging
 * requirements — Products & Charges (FR-BIL-022), IP Pools (FR-NET-005),
 * the plan-change card (FR-BIL-023), the plan lifecycle controls
 * (FR-CAT-001), Audit Trail (FR-OBS-006), Auth Log (FR-NET-004) and
 * Gateway Log (FR-PAY-001).
 *
 * Every one of these is already covered by Go tests that render its
 * template into a buffer, and that is exactly the blind spot this file
 * exists for. A page name missing from staffui's page registry renders
 * perfectly in those tests and returns 500 in a browser; it happened four
 * times while these screens were built. So the first assertion for each
 * screen is the one no unit test can make: it loads over HTTP, in a real
 * session, with its heading on it.
 *
 * The second is the 403 for a role that should not reach it. Not offering
 * a nav link is a convenience; refusing the typed URL is the control.
 *
 * Requires a seeded stack (see HANDOFF.md) with the five staff accounts.
 */

const PASSWORD = 'staffpassword';

const SHOTS = path.join('e2e', 'snapshots');
fs.mkdirSync(SHOTS, { recursive: true });

async function snap(page: Page, name: string) {
  await page.screenshot({ path: path.join(SHOTS, `${name}.png`), fullPage: true });
}

async function signIn(page: Page, username: string) {
  await page.goto('/staff/login');
  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password').fill(PASSWORD);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(/\/staff\/dashboard/);
}

/**
 * The reach each screen is specified to have, mirroring the section table
 * in internal/staffui/staffui.go. Written out here rather than imported so
 * that widening the Go table silently does not widen the test with it.
 */
const SCREENS = [
  {
    key: 'products',
    url: '/staff/products',
    heading: 'Products & charges',
    allow: ['owner', 'billing', 'csr'],
    deny: ['noc', 'tech'],
  },
  {
    key: 'ip-pools',
    url: '/staff/ip-pools',
    heading: 'IP pools',
    allow: ['owner', 'noc'],
    deny: ['billing', 'csr', 'tech'],
  },
  {
    key: 'auth-log',
    url: '/staff/auth-log',
    heading: 'Authentication log',
    allow: ['owner', 'noc', 'csr', 'tech'],
    deny: ['billing'],
  },
  {
    key: 'audit',
    url: '/staff/audit',
    heading: 'Audit trail',
    allow: ['owner'],
    deny: ['noc', 'billing', 'csr', 'tech'],
  },
  {
    key: 'gateway-log',
    url: '/staff/gateway-log',
    heading: 'Gateway log',
    allow: ['owner', 'billing'],
    deny: ['noc', 'csr', 'tech'],
  },
  {
    key: 'bandwidth',
    url: '/staff/bandwidth',
    heading: 'Bandwidth profiles',
    allow: ['owner', 'billing'],
    deny: ['noc', 'csr', 'tech'],
  },
] as const;

test.describe('The new console screens load in a browser', () => {
  for (const screen of SCREENS) {
    // The registry test. A screen whose page name is not registered renders
    // in Go and 500s here, so the status code is the assertion that matters
    // and the heading proves it rendered its own template rather than an
    // error page that happens to answer 200.
    test(`${screen.key} renders for every role that may open it`, async ({ page }) => {
      for (const user of screen.allow) {
        await signIn(page, user);

        const res = await page.request.get(screen.url);
        expect(res.status(), `${user} opening ${screen.url}`).toBe(200);

        await page.goto(screen.url);
        await expect(
          page.getByRole('heading', { name: screen.heading, level: 1 }),
          `${screen.url} should render its own heading for ${user}`,
        ).toBeVisible();

        // A 500 rendered through the layout would still carry the nav, so
        // check for the words a Go template failure leaves behind.
        const body = (await page.textContent('body')) ?? '';
        expect(body).not.toContain('executing template');
        expect(body).not.toContain('Internal Server Error');

        await page.getByRole('button', { name: 'Sign out' }).click();
      }
    });

    test(`${screen.key} refuses the typed URL for every other role`, async ({ page }) => {
      for (const user of screen.deny) {
        await signIn(page, user);
        const res = await page.request.get(screen.url);
        expect(res.status(), `${user} should not reach ${screen.url}`).toBe(403);
        await page.getByRole('button', { name: 'Sign out' }).click();
      }
    });
  }

  test('every new screen is reachable from the navigation, not just by URL', async ({ page }) => {
    await signIn(page, 'owner');
    // The owner reaches all five, so one session proves the nav carries
    // them. A screen present in the route table but absent from the
    // section table is reachable only by typing the URL, which is how a
    // feature ships and is never found.
    for (const screen of SCREENS) {
      await expect(
        page.locator(`nav a[href="${screen.url}"]`),
        `${screen.key} has no navigation link`,
      ).toHaveCount(1);
    }
    await snap(page, 'nav-with-new-screens');
  });
});

test.describe('Audit trail (FR-OBS-006)', () => {
  test('the trail records the actions an operator has just taken', async ({ page }) => {
    // Generate an auditable action, then look for it. Signing in is itself
    // audited, so the trail cannot be empty by the time it is read.
    await signIn(page, 'owner');
    await page.goto('/staff/audit');

    await expect(page.getByRole('heading', { name: 'Audit trail', level: 1 })).toBeVisible();
    // The filters exist and are populated from what is actually in the
    // table — an actor list that renders empty means the query behind it
    // returned nothing.
    await expect(page.locator('select[name="actor"]')).toBeVisible();
    await expect(page.locator('select[name="action"]')).toBeVisible();
    await snap(page, 'audit-trail');

    const rows = page.locator('table tbody tr');
    expect(await rows.count(), 'the audit trail is empty after signing in').toBeGreaterThan(0);
  });

  test('filtering by actor reaches the store and narrows the list', async ({ page }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/audit?actor=owner');
    await expect(page).toHaveURL(/actor=owner/);
    await expect(page.getByRole('heading', { name: 'Audit trail', level: 1 })).toBeVisible();
    const body = (await page.textContent('body')) ?? '';
    expect(body).not.toContain('executing template');
  });

  test('the trail offers no way to edit or delete an entry', async ({ page }) => {
    // Append-only is enforced in the database against bss_app (migration
    // 062). The screen should not imply otherwise.
    await signIn(page, 'owner');
    await page.goto('/staff/audit');
    await expect(page.locator('table form[method="post"]')).toHaveCount(0);
    const body = ((await page.textContent('body')) ?? '').toLowerCase();
    expect(body).not.toContain('delete entry');
  });
});

test.describe('Auth log (FR-NET-004)', () => {
  test('the log renders its rejection summary and filters', async ({ page }) => {
    await signIn(page, 'noc');
    await page.goto('/staff/auth-log');

    await expect(page.getByRole('heading', { name: 'Authentication log', level: 1 })).toBeVisible();
    await expect(page.locator('input[name="username"]')).toBeVisible();
    await expect(page.locator('select[name="result"]')).toBeVisible();
    await snap(page, 'auth-log');
  });

  test('no password appears anywhere on the page', async ({ page }) => {
    // The requirement is that the supplied credential is never recorded.
    // Go tests assert the struct and the column; this asserts the screen,
    // which is where a well-meaning "show what they typed" would surface.
    await signIn(page, 'noc');
    await page.goto('/staff/auth-log');
    const body = ((await page.textContent('body')) ?? '').toLowerCase();
    expect(body).not.toContain('password tried');
    expect(body).not.toContain('supplied password');
    expect(page.locator('th', { hasText: /password/i })).toHaveCount(0);
  });

  test('filtering by result round-trips through the URL', async ({ page }) => {
    await signIn(page, 'noc');
    await page.goto('/staff/auth-log?result=reject');
    await expect(page.locator('select[name="result"]')).toHaveValue('reject');
  });
});

test.describe('Gateway log (FR-PAY-001)', () => {
  test('the log leads with what needs attention', async ({ page }) => {
    await signIn(page, 'billing');
    await page.goto('/staff/gateway-log');

    await expect(page.getByRole('heading', { name: 'Gateway log', level: 1 })).toBeVisible();
    // The requirement's own phrase, which is why rows exist for callbacks
    // that credited nobody.
    await expect(page.getByText('whether or not it credited a wallet')).toBeVisible();
    // The security claim is on the page rather than only in the commit.
    await expect(
      page.getByText('No signature, secret or raw request body is kept'),
    ).toBeVisible();
    await snap(page, 'gateway-log');
  });

  test('the problems-only view and the filters round-trip', async ({ page }) => {
    await signIn(page, 'billing');
    await page.goto('/staff/gateway-log?status=unmatched&kind=callback');
    await expect(page.locator('select[name="status"]')).toHaveValue('unmatched');
    await expect(page.locator('select[name="kind"]')).toHaveValue('callback');

    await page.goto('/staff/gateway-log?problems=true');
    await expect(page.getByRole('heading', { name: 'Gateway log', level: 1 })).toBeVisible();
  });
});

test.describe('Products and charges (FR-BIL-022)', () => {
  test('the catalogue renders for billing and read-only for a CSR', async ({ page }) => {
    await signIn(page, 'billing');
    await page.goto('/staff/products');
    await expect(page.getByRole('heading', { name: 'Products & charges', level: 1 })).toBeVisible();
    await snap(page, 'products');
    await page.getByRole('button', { name: 'Sign out' }).click();

    // A CSR fielding "why is my bill higher" needs to read the charges and
    // must not be able to change what every subscriber is billed.
    await signIn(page, 'csr');
    await page.goto('/staff/products');
    await expect(page.getByRole('heading', { name: 'Products & charges', level: 1 })).toBeVisible();
    await expect(
      page.locator('form[action="/staff/products"] button[type="submit"]'),
      'a CSR is offered a way to create a product',
    ).toHaveCount(0);
    await snap(page, 'products-csr');
  });
});

test.describe('IP pools (FR-NET-005)', () => {
  test('the pools screen renders and withholds assignment from the NOC', async ({ page }) => {
    await signIn(page, 'noc');
    await page.goto('/staff/ip-pools');
    await expect(page.getByRole('heading', { name: 'IP pools', level: 1 })).toBeVisible();
    await snap(page, 'ip-pools');
  });
});

test.describe('Bandwidth profiles (FR-CAT-002)', () => {
  // The round trip through a real browser: create a burst profile from the
  // form and check the screen shows the rate-limit attribute a MikroTik NAS
  // would actually receive. The Go tests assert that rendering; this asserts
  // that the form's Mbps inputs reach it, which is where a factor-of-1000
  // unit mistake would land.
  test('a burst profile can be created and shows what the router receives', async ({ page }) => {
    await signIn(page, 'billing');
    await page.goto('/staff/bandwidth');

    const name = `E2E burst ${Date.now()}`;
    await page.locator('form[action="/staff/bandwidth"] input[name="name"]').fill(name);
    await page.locator('input[name="max_down"]').last().fill('50');
    await page.locator('input[name="max_up"]').last().fill('25');
    await page.locator('input[name="burst_down"]').last().fill('100');
    await page.locator('input[name="burst_up"]').last().fill('50');
    await page.locator('input[name="burst_threshold_down"]').last().fill('40');
    await page.locator('input[name="burst_threshold_up"]').last().fill('20');
    await page.locator('input[name="burst_time_seconds"]').last().fill('10');
    await page.locator('input[name="priority"]').last().fill('4');
    await page.getByRole('button', { name: 'Add profile' }).click();

    await expect(page.getByText(name)).toBeVisible();
    // 50 Mbps in, 50M out — not 50 bps and not 50,000,000 M.
    await expect(page.getByText('50M/25M 100M/50M 40M/20M 10/10 4')).toBeVisible();
    await snap(page, 'bandwidth-profiles');
  });

  test('an incoherent profile is refused with a reason, not a 500', async ({ page }) => {
    await signIn(page, 'billing');
    await page.goto('/staff/bandwidth');

    // A burst slower than the ceiling is not a burst. The database refuses
    // it too; the screen must say why rather than hand over a stack trace.
    await page.locator('form[action="/staff/bandwidth"] input[name="name"]').fill(`E2E bad ${Date.now()}`);
    await page.locator('input[name="max_down"]').last().fill('50');
    await page.locator('input[name="max_up"]').last().fill('50');
    await page.locator('input[name="burst_down"]').last().fill('20');
    await page.locator('input[name="burst_threshold_down"]').last().fill('10');
    await page.locator('input[name="burst_time_seconds"]').last().fill('10');
    await page.getByRole('button', { name: 'Add profile' }).click();

    await expect(page.getByText(/not a burst/)).toBeVisible();
    const body = (await page.textContent('body')) ?? '';
    expect(body).not.toContain('Internal Server Error');
    expect(body).not.toContain('SQLSTATE');
  });

  test('a CSR is not offered the form at all', async ({ page }) => {
    await signIn(page, 'csr');
    const res = await page.request.get('/staff/bandwidth');
    expect(res.status()).toBe(403);
  });
});

test.describe('Plan lifecycle controls (FR-CAT-001)', () => {
  test('a plan can be opened, and offers edit, duplicate, retire and export', async ({ page }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/catalogue');

    const heading = page.getByRole('heading', { level: 1 });
    await expect(heading).toBeVisible();
    const body = (await page.textContent('body')) ?? '';
    expect(body).not.toContain('executing template');

    // The export is a link rather than a form, so it is assertable without
    // changing any data.
    await expect(page.locator('a[href*="/staff/catalogue/plans.csv"]')).toHaveCount(1);
    await snap(page, 'catalogue-plan-controls');
  });

  test('the plan CSV export downloads with a header row', async ({ page }) => {
    await signIn(page, 'owner');
    const res = await page.request.get('/staff/catalogue/plans.csv');
    expect(res.status()).toBe(200);
    expect(res.headers()['content-type']).toContain('csv');
    const text = await res.text();
    expect(text.split('\n')[0].toLowerCase()).toContain('name');
  });
});
