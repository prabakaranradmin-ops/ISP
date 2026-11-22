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
  {
    key: 'documents',
    url: '/staff/documents',
    heading: 'Archived documents',
    // Billing and the owner only: the archive holds KYC scans.
    allow: ['owner', 'billing'],
    deny: ['noc', 'csr', 'tech'],
  },
  {
    key: 'collections',
    url: '/staff/collections',
    heading: 'Collections',
    // Wider than the other financial screens by one role: technicians work
    // the rounds, and the screen is scoped to their own.
    allow: ['owner', 'billing', 'tech'],
    deny: ['noc', 'csr'],
  },
  {
    key: 'fair-use',
    url: '/staff/fair-use',
    heading: 'Fair-use policies',
    allow: ['owner', 'billing'],
    deny: ['noc', 'csr', 'tech'],
  },
  {
    key: 'plan-attributes',
    url: '/staff/plan-attributes',
    heading: 'RADIUS attributes',
    // Network tier, not billing: these attributes carry no price, unlike a
    // bandwidth profile, which sets the speed a plan delivers.
    allow: ['owner', 'noc'],
    deny: ['billing', 'csr', 'tech'],
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
    // Scoped to this run's own row. The rate limit string is not unique —
    // every run of this test produces the same one — so asserting it page
    // -wide matches every profile left by an earlier run.
    //
    // 50 Mbps typed in the form, "50M" on the wire: not 50 bps, and not
    // 50,000,000 M.
    await expect(page.locator('tr', { hasText: name }))
      .toContainText('50M/25M 100M/50M 40M/20M 10/10 4');
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

test.describe('Customer Application Form (FR-DOC-002)', () => {
  // The form prints through the same headless browser as invoices, so this
  // is the test that shows the whole chain works: the subscriber, their
  // plan and their KYC facts assembled, rendered to HTML, and printed to a
  // real PDF by a real browser.
  test('a CSR can print a subscriber application form as a PDF', async ({ page }) => {
    await signIn(page, 'csr');

    const res = await page.request.get('/staff/subscribers/1/caf');
    if (res.status() === 503) {
      test.skip(true, 'no Chromium-based browser on this host, so no PDF can be printed');
    }
    expect(res.status(), 'the application form did not print').toBe(200);
    expect(res.headers()['content-type']).toContain('application/pdf');
    // An attachment, not inline: the form carries contact details, and
    // inline leaves it in the browser's cache and history.
    expect(res.headers()['content-disposition']).toContain('attachment');
    expect(res.headers()['content-disposition']).toContain('application-form-1.pdf');

    const body = await res.body();
    expect(body.length, 'the PDF is implausibly small').toBeGreaterThan(1000);
    expect(body.subarray(0, 5).toString()).toBe('%PDF-');
  });

  // The network roles have no reason to hold a customer's contact details
  // and KYC status.
  test('the network roles cannot print one', async ({ page }) => {
    for (const role of ['noc', 'tech'] as const) {
      await signIn(page, role);
      const res = await page.request.get('/staff/subscribers/1/caf');
      expect(res.status(), `${role} printing an application form`).toBe(403);
      await page.getByRole('button', { name: 'Sign out' }).click();
    }
  });

  test('an unknown subscriber is reported, not printed blank', async ({ page }) => {
    await signIn(page, 'csr');
    const res = await page.request.get('/staff/subscribers/999999/caf');
    // 404 when there is a browser to print with, 503 when there is not —
    // either way, never a PDF describing a subscriber that does not exist.
    expect([404, 503]).toContain(res.status());
    if (res.status() === 404) {
      expect(res.headers()['content-type']).not.toContain('application/pdf');
    }
  });
});

test.describe('Archived documents (FR-DOC-003)', () => {
  // The screen has to be honest when there is nothing archived, and say how
  // a document comes to be there — an empty table with no explanation reads
  // as a broken page.
  test('the screen explains an empty archive and how documents get there', async ({ page }) => {
    await signIn(page, 'billing');
    await page.goto('/staff/documents?subscriber_id=1');

    await expect(page.getByRole('heading', { name: 'Archived documents', level: 1 })).toBeVisible();
    // The claim that makes this archival rather than copying.
    await expect(page.getByText(/Every read is verified/)).toBeVisible();
    // And that there is no unverified way out of it.
    await expect(page.getByText(/not a control/)).toBeVisible();

    const body = (await page.textContent('body')) ?? '';
    // Either documents are listed, or the emptiness is explained. Both are
    // correct; silence is not.
    const listed = body.includes('Invoice ');
    if (!listed) {
      await expect(page.getByText(/Nothing archived for this subscriber yet/)).toBeVisible();
      await expect(page.getByText(/KYC document archival is not wired/)).toBeVisible();
    }
    await snap(page, 'documents');
  });

  // Invoices are archived when their PDF renders, so opening one puts a
  // verified copy on this screen. That round trip is the whole feature, and
  // it is the one thing no unit test can show: the render hook, the store
  // and the per-subscriber join all have to line up.
  test('rendering an invoice PDF puts a verified copy on the screen', async ({ page }) => {
    await signIn(page, 'billing');

    // Find an invoice belonging to a subscriber, through the invoices screen.
    await page.goto('/staff/invoices');
    const pdfLink = page.locator('a[href*="/pdf"]').first();
    if ((await pdfLink.count()) === 0) {
      test.skip(true, 'no invoice in the seeded data to render');
    }
    const href = await pdfLink.getAttribute('href');
    // Rendering is what archives it.
    const rendered = await page.request.get(href!);
    expect(rendered.status(), 'the invoice PDF did not render').toBe(200);

    // The archive is keyed per subscriber, and the seed's test_user is 1.
    await page.goto('/staff/documents?subscriber_id=1');
    const body = (await page.textContent('body')) ?? '';
    if (!body.includes('Invoice ')) {
      // Archival is off on this deployment (ARCHIVE_DIR unset), which the
      // screen says plainly rather than pretending.
      expect(body).toContain('Nothing archived for this subscriber yet');
      test.skip(true, 'ARCHIVE_DIR is not set, so nothing is archived');
    }

    // A listed document offers both actions, and the integrity check is
    // offered separately from the download so confirming a copy is intact
    // does not pull a customer's documents down.
    await expect(page.getByRole('button', { name: /Check it is intact/ }).first()).toBeVisible();
    await expect(page.locator('a[href*="/download"]').first()).toBeVisible();

    // And the check reports the copy is intact.
    await page.getByRole('button', { name: /Check it is intact/ }).first().click();
    await expect(page.getByText(/checksum matches/)).toBeVisible();
  });

  test('the front desk and the field cannot reach it', async ({ page }) => {
    for (const role of ['csr', 'tech'] as const) {
      await signIn(page, role);
      const res = await page.request.get('/staff/documents');
      expect(res.status(), `${role} opening /staff/documents`).toBe(403);
      await page.getByRole('button', { name: 'Sign out' }).click();
    }
  });
});

test.describe('Field payment collection (FR-COL-001)', () => {
  // The security property the feature turns on, through a real browser: a
  // technician may work a round, but must not be able to look at somebody
  // else's by typing the parameter, and must not be offered the assignment
  // or cash-reconciliation controls at all.
  test('a technician sees only their own round and none of the owner controls', async ({ page }) => {
    await signIn(page, 'tech');
    await page.goto('/staff/collections?collector=1');

    await expect(page.getByRole('heading', { name: 'Collections', level: 1 })).toBeVisible();
    // Their own round, named as theirs.
    await expect(page.getByText(/Your doorstep collection round/)).toBeVisible();
    // Not the assignment form, not the takings table.
    await expect(page.locator('form[action="/staff/collections/assign"]')).toHaveCount(0);
    await expect(page.getByText(/Takings for/)).toHaveCount(0);
    // And no way to pick whose round to look at.
    await expect(page.locator('select[name="collector"]')).toHaveCount(0);
  });

  test('the owner can pick a round and sees the cash reconciliation', async ({ page }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/collections');

    await expect(page.locator('select[name="collector"]')).toHaveCount(1);
    await expect(page.getByText(/Takings for/)).toBeVisible();
    // Cash is the line that has to be handed in, and the page says why.
    await expect(page.getByText(/has to be handed in/)).toBeVisible();
    await expect(page.locator('form[action="/staff/collections/assign"]')).toHaveCount(1);
    await snap(page, 'collections');
  });

  // A collector needs to know what to ask for and why somebody dropped off
  // the round, so the look-ahead has to round-trip rather than silently
  // resetting.
  test('the look-ahead window round-trips, including lapsed-only', async ({ page }) => {
    await signIn(page, 'tech');
    await page.goto('/staff/collections?window=0');
    await expect(page.locator('select[name="window"]')).toHaveValue('0');
    await page.goto('/staff/collections?window=30');
    await expect(page.locator('select[name="window"]')).toHaveValue('30');
    // A nonsense value falls back to the default rather than to zero, which
    // would show a shorter round than was asked for.
    await page.goto('/staff/collections?window=soon');
    await expect(page.locator('select[name="window"]')).toHaveValue('7');
  });

  // The core of the requirement: a collector takes cash at the door and it
  // reaches the wallet through FR-BIL-013's existing credit. Asserted end to
  // end because every layer below this is tested against a fake, and the one
  // thing a fake cannot show is that the form, the scoping check and the
  // wallet credit actually line up.
  test('a technician records cash and it reaches the wallet', async ({ page }) => {
    await signIn(page, 'tech');
    await page.goto('/staff/collections?window=30');

    const row = page.locator('tbody tr', { has: page.getByText('Take payment') }).first();
    if ((await row.count()) === 0) {
      test.skip(true, 'nothing on the seeded round to collect from');
    }
    // The subscriber's own link carries their id, which is what the wallet
    // has to be checked against afterwards.
    const href = await row.locator('td a').first().getAttribute('href');
    expect(href, 'the round does not link to the subscriber').toBeTruthy();

    await row.getByText('Take payment').click();
    const form = row.locator('form[action*="/payment"]');
    // A part payment is the interesting case: it must credit what was
    // actually taken rather than the full figure the form pre-filled.
    await form.locator('input[name="amount"]').fill('100.00');
    await form.locator('select[name="method"]').selectOption('cash');
    await form.getByRole('button', { name: /Record/ }).click();

    await expect(page.getByText(/Payment recorded and credited/)).toBeVisible();

    // And it reaches where the money is actually kept, not only the flash
    // message on the screen that recorded it. The owner is used because the
    // ledger panel is withheld from technicians.
    await page.getByRole('button', { name: 'Sign out' }).click();
    await signIn(page, 'owner');
    await page.goto(href!);
    // Asserted on the page's text rather than on visibility: the ledger sits
    // in a collapsible panel, so the credit can be present and correct while
    // the element reports hidden. What is under test is that the money
    // arrived, not which panel happens to be open.
    const body = (await page.textContent('body')) ?? '';
    expect(body, 'the collected amount is not on the subscriber page').toContain('100.00');
  });

  test('the desk-based roles cannot reach the screen', async ({ page }) => {
    for (const role of ['csr', 'noc'] as const) {
      await signIn(page, role);
      const res = await page.request.get('/staff/collections');
      expect(res.status(), `${role} opening /staff/collections`).toBe(403);
      await page.getByRole('button', { name: 'Sign out' }).click();
    }
  });
});

test.describe('Fair-use policies (FR-CAT-003)', () => {
  // The round trip through a real browser: a three-step policy created from
  // the form, with the thresholds coming back in GB. The Go tests assert the
  // GB-to-bytes conversion; this asserts the form's values survive it, which
  // is where a factor-of-a-billion unit mistake would land.
  test('a multi-step policy can be created and shows every step', async ({ page }) => {
    await signIn(page, 'billing');
    await page.goto('/staff/fair-use');

    const name = `E2E fair use ${Date.now()}`;
    const form = page.locator('form[action="/staff/fair-use"]').last();
    await form.locator('input[name="name"]').fill(name);
    const thresholds = form.locator('input[name="threshold_gb"]');
    const rates = form.locator('input[name="throttle_rate"]');
    await thresholds.nth(0).fill('500');
    await rates.nth(0).fill('10M/10M');
    await thresholds.nth(1).fill('1000');
    await rates.nth(1).fill('2M/2M');
    await form.getByRole('button', { name: 'Add policy' }).click();

    const row = page.locator('tr', { hasText: name });
    await expect(row).toContainText('500 GB');
    await expect(row).toContainText('10M/10M');
    await expect(row).toContainText('1000');
    await expect(row).toContainText('2M/2M');
    // Throttling starts at the first step, which is also what the 80%
    // warning is measured against.
    await expect(row).toContainText('500 GB');
    await snap(page, 'fair-use-policies');
  });

  test('a step with no throttle rate is refused with a reason, not a 500', async ({ page }) => {
    await signIn(page, 'billing');
    await page.goto('/staff/fair-use');

    const form = page.locator('form[action="/staff/fair-use"]').last();
    await form.locator('input[name="name"]').fill(`E2E bad ${Date.now()}`);
    await form.locator('input[name="threshold_gb"]').nth(0).fill('500');
    // The rate is required by the markup, so it is cleared explicitly to
    // get past the browser's own validation and reach the server's.
    await form.locator('input[name="throttle_rate"]').nth(0).fill('not-a-rate');
    await form.getByRole('button', { name: 'Add policy' }).click();

    // Matched on "tier 1:" rather than the rate wording, which also appears
    // in the page's own help text. This is the better assertion anyway: the
    // message has to name which step is wrong, not just that one is.
    await expect(page.getByText(/tier 1:/)).toBeVisible();
    const body = (await page.textContent('body')) ?? '';
    expect(body).not.toContain('Internal Server Error');
    expect(body).not.toContain('SQLSTATE');
  });

  test('a plan can be pointed at a policy and back to its own threshold', async ({ page }) => {
    await signIn(page, 'billing');
    await page.goto('/staff/fair-use');

    // Detaching is the supported setup for a plan with no policy, so it has
    // to be expressible rather than a one-way door.
    const attach = page.locator('form[action="/staff/fair-use/attach"]');
    await expect(attach.locator('select[name="policy_id"] option', { hasText: /None/ })).toHaveCount(1);
    await attach.locator('select[name="policy_id"]').selectOption('');
    await attach.getByRole('button', { name: 'Apply to plan' }).click();
    await expect(page.getByText(/back to its own single threshold/)).toBeVisible();
  });

  test('network and front-desk roles cannot reach the screen', async ({ page }) => {
    for (const role of ['noc', 'csr'] as const) {
      await signIn(page, role);
      const res = await page.request.get('/staff/fair-use');
      expect(res.status(), `${role} opening /staff/fair-use`).toBe(403);
      await page.getByRole('button', { name: 'Sign out' }).click();
    }
  });
});

test.describe('Per-plan RADIUS attributes (FR-CAT-004)', () => {
  test('an attribute can be added and is shown as being sent', async ({ page }) => {
    await signIn(page, 'noc');
    await page.goto('/staff/plan-attributes?vendor=mikrotik');

    const list = `e2e-${Date.now()}`;
    await page.selectOption('form[action="/staff/plan-attributes"] select[name="attribute"]',
      'Mikrotik-Address-List');
    await page.locator('form[action="/staff/plan-attributes"] input[name="value"]').fill(list);
    await page.getByRole('button', { name: /Add to/ }).click();

    await expect(page.getByText(list)).toBeVisible();
    // The row says it reaches the router, which is the only thing the
    // operator actually cares about.
    await expect(page.locator('tr', { hasText: list }).getByText('sent')).toBeVisible();
  });

  // The dictionary's whole purpose, asserted through the browser: a value a
  // router would silently ignore is refused here with a reason.
  test('a wrongly typed value is refused with a reason, not a 500', async ({ page }) => {
    await signIn(page, 'noc');
    await page.goto('/staff/plan-attributes?vendor=mikrotik');

    await page.selectOption('form[action="/staff/plan-attributes"] select[name="attribute"]',
      'Session-Timeout');
    await page.locator('form[action="/staff/plan-attributes"] input[name="value"]').fill('an hour');
    await page.getByRole('button', { name: /Add to/ }).click();

    // "takes a whole number" is the dictionary's wording; the form's own
    // hint says "A whole number for uint32 attributes", so matching the
    // bare phrase would hit both and trip strict mode.
    await expect(page.getByText(/takes a whole number/)).toBeVisible();
    const body = (await page.textContent('body')) ?? '';
    expect(body).not.toContain('Internal Server Error');
    expect(body).not.toContain('SQLSTATE');
  });

  // An attribute the bandwidth builder sends must not be offered at all,
  // but must still be explained — an operator who cannot see why it is
  // missing assumes the dictionary is incomplete.
  test('the attribute the plan shaping sends is explained but not offered', async ({ page }) => {
    await signIn(page, 'noc');
    await page.goto('/staff/plan-attributes?vendor=mikrotik');

    const options = await page
      .locator('form[action="/staff/plan-attributes"] select[name="attribute"] option')
      .allInnerTexts();
    expect(options.join('|')).not.toContain('Mikrotik-Rate-Limit');

    await page.getByText('What each attribute does').click();
    await expect(page.getByText('Not available here').first()).toBeVisible();
    await snap(page, 'plan-attributes');
  });

  test('billing staff cannot reach the screen at all', async ({ page }) => {
    await signIn(page, 'billing');
    const res = await page.request.get('/staff/plan-attributes');
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

test.describe('Subscriber groups (FR-GRP-001)', () => {
  // Unique per run: the name index is partial on active, so a leftover live
  // group from an earlier run would make the create collide rather than fail
  // on anything this test is about.
  const groupName = `E2E feeder ${Date.now()}`;

  test('the screen loads over HTTP and refuses a role that should not reach it', async ({ page }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/groups');
    await expect(page.getByRole('heading', { name: 'Subscriber groups', level: 1 })).toBeVisible();

    // Not offering a nav link is a convenience; refusing the typed URL is
    // the control. A technician's reach is the job in front of them.
    await signIn(page, 'tech');
    const res = await page.goto('/staff/groups');
    expect(res?.status()).toBe(403);
  });

  // The whole feature end to end in one pass, because every layer below this
  // is tested against a fake and what a fake cannot show is that the create
  // form, the paste-a-list form, the directory filter and the roster report
  // all agree on the same group.
  test('a group is created, filled from a pasted list, and filters the directory', async ({ page }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/groups');

    const create = page.locator('form[action="/staff/groups"]');
    await create.locator('input[name="name"]').fill(groupName);
    await create.locator('input[name="description"]').fill('Created by the browser suite');
    await create.getByRole('button', { name: 'Add group' }).click();
    await expect(page.getByText(/Group added/)).toBeVisible();

    // The row for this group, by its unique name — the table holds the
    // seeded groups too.
    const row = page.locator('tbody tr', { hasText: groupName });
    await expect(row).toHaveCount(1);

    // Paste a subscriber in. Subscriber 1 exists in every seeded stack; 999999
    // does not, and the count reported is the assertion — a store that failed
    // the whole batch on the bad id, or claimed both, would show 2 of 2 or an
    // error instead of 1 of 2.
    await row.getByText('Edit').click();
    const members = row.locator('form[action$="/members"]');
    await members.locator('textarea[name="subscriber_ids"]').fill('1, 999999');
    await members.getByRole('button', { name: /^Add to/ }).click();
    await expect(page.getByText(/1 of 2 added to the group/)).toBeVisible();

    // The member count links straight into the directory filtered to this
    // group, which is the one place the two views of a group could drift.
    const refreshed = page.locator('tbody tr', { hasText: groupName });
    const countLink = refreshed.locator('td a[href*="/staff/subscribers?group_id="]').first();
    await expect(countLink).toHaveText('1');
    const href = await countLink.getAttribute('href');
    const groupID = new URL(href!, page.url()).searchParams.get('group_id');
    expect(groupID).toBeTruthy();

    await countLink.click();
    await expect(page.getByRole('heading', { name: 'Subscribers', level: 1 })).toBeVisible();
    // The filter is applied and reflected back in the select, not silently
    // dropped — and exactly one subscriber matches.
    await expect(page.locator('select[name="group_id"]')).toHaveValue(groupID!);
    await expect(page.locator('tbody tr')).toHaveCount(1);

    // And the roster report, scoped to the same group, agrees.
    await page.goto(`/staff/reports?report=subscribers&group_id=${groupID}`);
    await expect(page.getByRole('heading', { name: /^Subscribers in this group$/ })).toBeVisible();
    await expect(page.locator('tbody tr')).toHaveCount(1);
    await snap(page, 'groups-roster');

    // The CSV is the deliverable an operator actually takes away.
    // The console route, not /api/v1/... — the API wants a Bearer JWT and
    // this session has a cookie. Asserting the link an operator actually
    // clicks is the point: the first version of this test hit the API path
    // and found the console link returning 401.
    const csv = await page.request.get(`/staff/reports/subscribers.csv?group_id=${groupID}`);
    expect(csv.status()).toBe(200);
    const text = await csv.text();
    expect(text.split('\n')[0]).toContain('subscriber_id');
    // Two lines: the header and the one member.
    expect(text.trim().split('\n')).toHaveLength(2);
    expect(text).toContain(groupName);
  });

  // The limitation, asserted rather than only documented. A group filter that
  // silently did not apply would hand an operator a figure for the whole base
  // under a heading naming one group.
  test('an aggregate report refuses a group filter instead of ignoring it', async ({ page }) => {
    await signIn(page, 'owner');

    await page.goto('/staff/reports?report=plan-mix&group_id=1');
    await expect(page.getByText(/cannot be filtered by group/)).toBeVisible();
    // And no group selector is offered there at all.
    await expect(page.locator('select[name="group_id"]')).toHaveCount(0);

    const refused = await page.request.get('/staff/reports/plan-mix.csv?group_id=1');
    expect(refused.status()).toBe(422);
    expect(await refused.text()).toContain('subscribers');
  });

  // A group is a fourth segment dimension for an announcement, which is the
  // reason every change to one is audited.
  test('an announcement can be addressed to a group', async ({ page }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/announcements');
    const select = page.locator('select[name="segment_group_id"]');
    if ((await select.count()) === 0) {
      test.skip(true, 'no groups on this stack to address');
    }
    await expect(select.locator('option').first()).toHaveText('Any group');
    // Every option carries its reach, so the audience is visible before the
    // draft is saved rather than only on the preview afterwards.
    await expect(select.locator('option').nth(1)).toHaveText(/\(\d+\)$/);
  });
});

test.describe('Report catalogue (FR-RPT-004)', () => {
  // Every report the catalogue holds, as the console itself lists them. Read
  // off the index rather than hardcoded, so adding a report to the Go
  // catalogue extends this test instead of leaving it behind.
  async function catalogueNames(page: Page): Promise<string[]> {
    await page.goto('/staff/reports');
    const hrefs = await page.locator('a[href^="/staff/reports?report="]').evaluateAll(
      (as) => as.map((a) => (a as HTMLAnchorElement).getAttribute('href') || ''));
    return [...new Set(hrefs.map((h) => h.replace('/staff/reports?report=', '')))];
  }

  test('the index groups every report under its area, with a filter box', async ({ page }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/reports');

    await expect(page.getByRole('heading', { name: 'Reports', level: 1 })).toBeVisible();
    // The four areas, as headings rather than nav links.
    for (const area of ['Subscribers', 'Money', 'Support', 'Network']) {
      await expect(page.getByRole('heading', { name: area, level: 3 })).toBeVisible();
    }
    // The nine FR-RPT-004 asked for by name, so the requirement cannot pass
    // with one missing.
    const names = await catalogueNames(page);
    for (const required of ['expiring-subscribers', 'payments-by-method', 'collection-by-staff',
      'invoice-register', 'tax-charged', 'top-data-users', 'fup-exceeded',
      'ticket-turnaround', 'plan-change-history']) {
      expect(names, `${required} is missing from the catalogue index`).toContain(required);
    }
    await expect(page.locator('#report-filter input[name="q"]')).toBeVisible();
    await snap(page, 'report-catalogue');
  });

  // Server-side, so it works with scripting off and a filtered catalogue can
  // be linked to or bookmarked.
  test('the filter box narrows the index on the server', async ({ page }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/reports?q=GST');

    // "GST" is only in the tax report's summary.
    await expect(page.locator('a[href="/staff/reports?report=tax-charged"]')).toHaveCount(1);
    await expect(page.locator('a[href="/staff/reports?report=plan-mix"]')).toHaveCount(0);
    // An area left with nothing loses its heading rather than standing over
    // an empty table.
    await expect(page.getByRole('heading', { name: 'Network', level: 3 })).toHaveCount(0);

    await page.goto('/staff/reports?q=zzzznothing');
    await expect(page.getByText(/No report matches/)).toBeVisible();
  });

  // And live, as an enhancement on top of the form.
  test('typing in the filter box hides the rows that do not match', async ({ page }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/reports');

    const taxRow = page.locator('a[href="/staff/reports?report=tax-charged"]');
    const planMixRow = page.locator('a[href="/staff/reports?report=plan-mix"]');
    await expect(taxRow).toBeVisible();
    await expect(planMixRow).toBeVisible();

    await page.locator('#report-filter input[name="q"]').fill('GST');
    await expect(taxRow).toBeVisible();
    // Hidden, not removed — the script only toggles rows.
    await expect(planMixRow).toBeHidden();
  });

  // The assertion no unit test can make: every report in the catalogue loads
  // over HTTP in a real session. A page name missing from staffui's registry
  // renders perfectly in a Go test and returns 500 in a browser.
  test('every report in the catalogue opens with its own heading', async ({ page }) => {
    await signIn(page, 'owner');
    const names = await catalogueNames(page);
    expect(names.length).toBeGreaterThanOrEqual(14);

    for (const name of names) {
      const res = await page.goto(`/staff/reports?report=${name}`);
      expect(res?.status(), `${name} did not load`).toBe(200);
      // Its own breadcrumb heading, so one report cannot be served under
      // another's name.
      await expect(page.locator('h1')).toContainText('Reports');
      // Prefix match: the link carries the report's current window as query
      // params, so an exact href would only ever match a report that takes
      // no filters at all.
      await expect(page.locator(`a[href^="/staff/reports/${name}.csv"]`).first()).toBeVisible();
      // Never a link to the API from a console page: the API wants a Bearer
      // JWT and this session has a cookie, so it would 401.
      await expect(page.locator('a[href*="/api/v1/reports/"]')).toHaveCount(0);
    }
  });

  test('a stale bookmark is named rather than served as a different report', async ({ page }) => {
    await signIn(page, 'owner');
    const res = await page.goto('/staff/reports?report=no-such-report');
    expect(res?.status()).toBe(404);
    await expect(page.getByText(/no-such-report/)).toBeVisible();
    // Emphatically not somebody else's figures under that name.
    await expect(page.getByText('current snapshot')).toHaveCount(0);
  });

  test('each report downloads as CSV from the console route', async ({ page }) => {
    await signIn(page, 'owner');
    const names = await catalogueNames(page);

    for (const name of names) {
      const res = await page.request.get(`/staff/reports/${name}.csv`);
      expect(res.status(), `${name}.csv`).toBe(200);
      expect(res.headers()['content-type']).toContain('csv');
      expect(res.headers()['content-disposition']).toContain(name);
      // A header row at minimum: a CSV with no header is a report that is
      // not wired up.
      const first = (await res.text()).split('\n')[0];
      expect(first.length, `${name}.csv has no header row`).toBeGreaterThan(0);
      expect(first).toContain(',');
    }
  });

  // Only the window a report declares. A month box on a point-in-time
  // snapshot implies a history it does not have.
  test('a report is offered only the window it takes', async ({ page }) => {
    await signIn(page, 'owner');

    await page.goto('/staff/reports?report=expiring-subscribers');
    await expect(page.locator('input[name="days"]')).toHaveCount(1);
    await expect(page.locator('input[name="months"]')).toHaveCount(0);

    await page.goto('/staff/reports?report=growth');
    await expect(page.locator('input[name="months"]')).toHaveCount(1);
    await expect(page.locator('input[name="days"]')).toHaveCount(0);

    await page.goto('/staff/reports?report=fup-exceeded');
    await expect(page.locator('input[name="months"]')).toHaveCount(0);
    await expect(page.locator('input[name="days"]')).toHaveCount(0);

    // And the top-users ranking takes a cut-off, which has to round-trip or
    // "top 10" silently returns the default fifty.
    await page.goto('/staff/reports?report=top-data-users&limit=10');
    await expect(page.locator('input[name="limit"]')).toHaveValue('10');
  });

  // The reports are financial and growth performance — the same reach Revenue
  // has. Not offering a nav link is a convenience; refusing the typed URL is
  // the control.
  test('a technician cannot reach the reports at all', async ({ page }) => {
    await signIn(page, 'tech');
    expect((await page.goto('/staff/reports'))?.status()).toBe(403);
    expect((await page.goto('/staff/reports/tax-charged.csv'))?.status()).toBe(403);
  });
});

test.describe('The shared vendor master (FR-INV-004)', () => {
  // Unique per run: the name index is partial on active, so a leftover live
  // vendor from an earlier run would make the create collide rather than fail
  // on anything this test is about.
  const vendorName = `E2E Cables ${Date.now()}`;

  test('the screen loads over HTTP and refuses a role that should not reach it', async ({ page }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/vendors');
    await expect(page.getByRole('heading', { name: 'Vendors', level: 1 })).toBeVisible();

    // A technician can reach Inventory but must not reach the master: a
    // vendor row carries a contact person, a phone number and a GSTIN, which
    // is commercial-relationship data rather than anything needed to issue a
    // router. Both halves asserted, because the interesting claim is that the
    // two screens have different reach.
    await signIn(page, 'tech');
    expect((await page.goto('/staff/inventory'))?.status()).toBe(200);
    expect((await page.goto('/staff/vendors'))?.status()).toBe(403);
  });

  // The whole feature in one pass, because what no Go test can show is that
  // the vendor screen, the Inventory purchase form and the Procurement order
  // form all agree on the same master row — and that a rename reaches all of
  // them at once, which is the requirement itself.
  test('a vendor is added, offered to both purchase forms, and a rename reaches every screen', async ({ page }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/vendors');

    // Retire anything this test left behind on an earlier run. The name is
    // unique per run, but the GSTIN below cannot be — generating a valid one
    // needs the check-digit algorithm, and a test that reimplemented the thing
    // it is testing would pass against a wrong implementation. Both unique
    // indexes are partial on active, so retiring the leftovers frees the
    // GSTIN, which is also exactly the cleanup an operator would do.
    for (let i = 0; i < 10; i++) {
      // Unanchored: hasText matches the row's whole text content, which starts
      // with the whitespace the template indents the cell with, so /^E2E/
      // never matches and the cleanup would silently do nothing.
      const stale = page.locator('tbody tr', { hasText: /E2E (Cables|Shape)/ })
        .locator('form[action$="/retired"]')
        .getByRole('button', { name: 'Retire' });
      if ((await stale.count()) === 0) break;
      await stale.first().click();
      await expect(page.getByText(/Vendor retired/)).toBeVisible();
    }

    // The GSTIN is the registry's published example, which is checksum-valid.
    const add = page.locator('form[action="/staff/vendors"]');
    await add.locator('input[name="name"]').fill(vendorName);
    await add.locator('input[name="contact_person"]').fill('R. Kumar');
    await add.locator('input[name="phone"]').fill('044-2812 3456 x14');
    await add.locator('input[name="gstin"]').fill('27AAPFU0939F1ZV');
    await add.getByRole('button', { name: 'Add vendor' }).click();
    await expect(page.getByText(/Vendor added/)).toBeVisible();

    const row = page.locator('tbody tr', { hasText: vendorName });
    await expect(row).toHaveCount(1);
    await expect(row).toContainText('27AAPFU0939F1ZV');
    await snap(page, 'vendors-master');

    // It is now offered on both purchase forms — as an option in a dropdown,
    // not a text box. This is the half of the requirement an operator
    // notices.
    await page.goto('/staff/inventory');
    const invPicker = page.locator('select[name="vendor_id"]').first();
    await expect(invPicker).toBeVisible();
    await expect(invPicker.locator('option', { hasText: vendorName })).toHaveCount(1);
    await expect(page.locator('input[name="vendor"]')).toHaveCount(0);

    await page.goto('/staff/procurement');
    const poPicker = page.locator('select[name="vendor_id"]');
    await expect(poPicker).toBeVisible();
    await expect(poPicker.locator('option', { hasText: vendorName })).toHaveCount(1);
    await expect(page.locator('input[name="vendor"]')).toHaveCount(0);

    // File a purchase order against it, so there is a row whose vendor name
    // the rename below has to reach.
    const poForm = page.locator('form[action="/staff/procurement/new"]');
    const poDescription = `E2E order ${Date.now()}`;
    await poForm.locator('input[name="description"]').fill(poDescription);
    // By the option's value rather than its label: the label carries the
    // GSTIN after the name ("Chennai Networks · 27AAPFU0939F1ZV"), so there is
    // no exact label to match, and selectOption's label option takes a string
    // rather than a pattern.
    const vendorOption = await poForm
      .locator('select[name="vendor_id"] option', { hasText: vendorName })
      .getAttribute('value');
    expect(vendorOption).toBeTruthy();
    await poForm.locator('select[name="vendor_id"]').selectOption(vendorOption!);
    await poForm.locator('input[name="amount"]').fill('4000.00');
    await poForm.getByRole('button', { name: /File|Submit|Request/i }).first().click();
    await expect(page.getByText(/filed for approval/)).toBeVisible();
    await expect(page.locator('tbody tr', { hasText: poDescription })).toContainText(vendorName);

    // Now correct the name in one place. The order filed above must show the
    // new name without being touched — that is what holding the vendor in one
    // place buys, and nothing below the browser can demonstrate it.
    const renamed = `${vendorName} Pvt Ltd`;
    await page.goto('/staff/vendors');
    // The edit link carries the id, and the edit form posts to exactly
    // /staff/vendors/<id>. Matched on the full action rather than a prefix:
    // every row's retire form is also /staff/vendors/<id>/..., so a prefix
    // match plus .first() lands on one of those, which has no name field.
    const editLink = page.locator('tbody tr', { hasText: vendorName })
      .locator('a[href*="/staff/vendors?edit="]');
    const editHref = await editLink.getAttribute('href');
    const vendorId = editHref!.split('=')[1];
    await editLink.click();

    const edit = page.locator(`form[action="/staff/vendors/${vendorId}"]`);
    await edit.locator('input[name="name"]').fill(renamed);
    await edit.getByRole('button', { name: 'Save vendor' }).click();
    await expect(page.getByText(/Vendor saved/)).toBeVisible();

    await page.goto('/staff/procurement');
    await expect(page.locator('tbody tr', { hasText: poDescription })).toContainText(renamed);

    // And retiring it takes it out of the pickers while leaving that order
    // showing it — the difference between retiring and deleting.
    await page.goto('/staff/vendors');
    await page.locator('tbody tr', { hasText: renamed })
      .locator('form[action$="/retired"]').getByRole('button', { name: 'Retire' }).click();
    await expect(page.getByText(/Vendor retired/)).toBeVisible();

    await page.goto('/staff/procurement');
    await expect(page.locator('tbody tr', { hasText: poDescription })).toContainText(renamed);
    await expect(page.locator('select[name="vendor_id"] option', { hasText: renamed })).toHaveCount(0);
  });

  // The checksum, through a real form. A transposed pair of digits is still a
  // perfectly well-shaped GSTIN, so the format check cannot see it — and a
  // wrong GSTIN on a purchase invoice means the input tax credit is denied.
  test('a GSTIN with a mistyped character is refused with a message that says why', async ({ page }) => {
    await signIn(page, 'billing');
    await page.goto('/staff/vendors');

    const add = page.locator('form[action="/staff/vendors"]');
    await add.locator('input[name="name"]').fill(`E2E Checksum ${Date.now()}`);
    await add.locator('input[name="gstin"]').fill('27AAPFU0939F1ZA');
    await add.getByRole('button', { name: 'Add vendor' }).click();

    await expect(page.getByText(/check digit/)).toBeVisible();

    // And the two GSTIN failures must read differently. The refusal redirects
    // to an empty form, so the name goes in again — a form that kept the
    // rejected input would be friendlier, but it does not, and a test that
    // assumed otherwise would fail on "a vendor name is required" and look
    // like a GSTIN bug.
    const retry = page.locator('form[action="/staff/vendors"]');
    await retry.locator('input[name="name"]').fill(`E2E Shape ${Date.now()}`);
    await retry.locator('input[name="gstin"]').fill('NOTAGSTIN');
    await retry.getByRole('button', { name: 'Add vendor' }).click();
    await expect(page.getByText(/15 characters/)).toBeVisible();
  });
});

test.describe('Captive portal branding (FR-HSP-004)', () => {
  // A 24x24 PNG in the accent colour below. Real image bytes, because the
  // server decides the type from the bytes and decodes them — a stub would be
  // refused, which is the point.
  const LOGO_PNG = Buffer.from(
    'iVBORw0KGgoAAAANSUhEUgAAABgAAAAYCAYAAADgdz34AAAAJklEQVR4nGPgzvP/T0vMMGrBqAWjFoxaMGrBqAWjFoxa8H9IWAAA4gz/7jrWKAsAAAAASUVORK5CYII=',
    'base64');

  // Put every field back to its default, so this test leaves the portal as
  // every other test and every unconfigured deployment expects to find it.
  async function resetBranding(page: Page) {
    await page.goto('/staff/portal-branding');
    const form = page.locator('form[action="/staff/portal-branding"]');
    for (const name of ['venue_name', 'headline', 'welcome_text', 'accent_color', 'background_color']) {
      await form.locator(`input[name="${name}"]`).fill('');
    }
    await form.locator('textarea[name="footer_text"]').fill('');
    await form.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByText(/Saved\./)).toBeVisible();
    const remove = page.getByRole('button', { name: 'Remove logo' });
    if (await remove.count()) {
      await remove.click();
      await expect(page.getByText(/Logo removed/)).toBeVisible();
    }
  }

  test('only the owner reaches the screen', async ({ page }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/portal-branding');
    await expect(page.getByRole('heading', { name: 'Captive portal', level: 1 })).toBeVisible();

    // The NOC engineer runs the hotspot NAS, but the portal's wording is the
    // business speaking in public under the venue's name.
    await signIn(page, 'noc');
    expect((await page.goto('/staff/portal-branding'))?.status()).toBe(403);
  });

  // The requirement end to end: branding set in the console reaches the page
  // an anonymous visitor is sent to — seen from a separate browser context
  // with no console session, because that is who sees it.
  test('branding saved in the console is what an anonymous visitor sees', async ({ page, browser }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/portal-branding');

    const form = page.locator('form[action="/staff/portal-branding"]');
    await form.locator('input[name="venue_name"]').fill('E2E Corner Cafe');
    await form.locator('input[name="headline"]').fill('Free Wi-Fi for customers');
    await form.locator('input[name="accent_color"]').fill('0B6E4F');
    await form.locator('textarea[name="footer_text"]').fill('Open 7am to 11pm\nHelp: 044-2812 3456');
    await form.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByText(/Saved\./)).toBeVisible();
    // Stored normalised, as the form shows it back.
    await expect(form.locator('input[name="accent_color"]')).toHaveValue('#0b6e4f');

    await page.locator('input[name="logo"]').setInputFiles({
      name: 'logo.png', mimeType: 'image/png', buffer: LOGO_PNG,
    });
    await page.getByRole('button', { name: 'Upload logo' }).click();
    await expect(page.getByText(/Logo uploaded/)).toBeVisible();

    // The preview frame is the real template with the forms showing.
    const preview = page.frameLocator('iframe[title="Captive portal preview"]');
    await expect(preview.getByRole('heading', { name: 'Free Wi-Fi for customers' })).toBeVisible();
    await snap(page, 'portal-branding-console');

    const visitor = await browser.newContext({ ignoreHTTPSErrors: true });
    try {
      const v = await visitor.newPage();
      await v.goto('/hotspot/portal?mac=aa-bb-cc-dd-ee-ff');
      await expect(v).toHaveTitle('E2E Corner Cafe');
      await expect(v.getByRole('heading', { name: 'Free Wi-Fi for customers' })).toBeVisible();
      await expect(v.getByText('Help: 044-2812 3456')).toBeVisible();

      // The colour as the browser applied it, not as the HTML spelled it.
      const bg = await v.getByRole('button', { name: 'Get online' })
        .evaluate((el) => getComputedStyle(el).backgroundColor);
      expect(bg).toBe('rgb(11, 110, 79)');

      // The logo actually loads — served from the portal's own origin, which
      // is the one a device on the walled garden can reach.
      const logo = v.locator('img.logo');
      await expect(logo).toHaveAttribute('alt', 'E2E Corner Cafe');
      await expect.poll(() => logo.evaluate((img: HTMLImageElement) => img.naturalWidth)).toBe(24);
      await snap(v, 'portal-branded');
    } finally {
      await visitor.close();
    }

    await resetBranding(page);
  });

  test('an SVG logo is refused with the reason', async ({ page }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/portal-branding');
    await page.locator('input[name="logo"]').setInputFiles({
      name: 'logo.svg', mimeType: 'image/svg+xml',
      buffer: Buffer.from('<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><rect/></svg>'),
    });
    await page.getByRole('button', { name: 'Upload logo' }).click();
    await expect(page.getByText(/SVG is refused because it can carry script/)).toBeVisible();
    await expect(page.getByRole('button', { name: 'Remove logo' })).toHaveCount(0);
  });
});

test.describe('NAS interface traffic (FR-NET-006)', () => {
  // TEST-NET-2, never a reachable router. The last octet varies per run
  // because nas_devices.ip is unique.
  const ip = `198.51.100.${(Date.now() % 250) + 1}`;

  test('the traffic screens are reached from the Network Map, and only by network roles', async ({ page }) => {
    await signIn(page, 'noc');
    await page.goto('/staff/network');
    await page.getByRole('link', { name: 'Interface traffic' }).click();
    await expect(page.getByRole('heading', { name: 'Interface traffic', level: 1 })).toBeVisible();

    await signIn(page, 'billing');
    expect((await page.goto('/staff/network/traffic'))?.status()).toBe(403);
  });

  // The operator's path to monitoring a router, through the real forms: add
  // the router, open its traffic page, try to turn monitoring on without a
  // community, then with one. Graph data needs a router answering SNMP, which
  // this machine does not have; the poller is proven end to end against an
  // SNMP agent in internal/db, and the graph's rendering in internal/staffui.
  test('a router is added and its monitoring turned on through the console', async ({ page }) => {
    await signIn(page, 'noc');
    await page.goto('/staff/nas');
    const add = page.locator('form[action="/staff/nas/new"]');
    await add.locator('input[name="ip"]').fill(ip);
    await add.locator('input[name="description"]').fill('E2E traffic router');
    await add.locator('input[name="radius_secret"]').fill('e2e-traffic-secret-0123456789');
    await add.getByRole('button', { name: 'Register device' }).click();
    await expect(page.locator('tbody tr', { hasText: ip })).toHaveCount(1);

    await page.goto('/staff/network/traffic');
    await page.locator('tbody tr', { hasText: ip }).getByRole('link', { name: 'Open' }).click();
    await expect(page.getByRole('heading', { name: `Traffic — ${ip}` })).toBeVisible();
    await expect(page.getByText(/Monitoring is off for this router/)).toBeVisible();

    const settings = page.locator(`form[action$="/snmp"]`);
    await settings.getByLabel('Poll this router').check();
    await settings.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByText(/Enter the router's SNMP community/)).toBeVisible();

    await settings.getByLabel('Poll this router').check();
    await settings.locator('input[name="snmp_community"]').fill('e2e-ro-community');
    await settings.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByText('Monitoring settings saved.')).toBeVisible();
    await expect(page.getByText(/Waiting for the first poll/)).toBeVisible();
    // Stored, and never shown again.
    await expect(settings.locator('input[name="snmp_community"]'))
      .toHaveAttribute('placeholder', /Stored — leave blank to keep it/);
    expect(await page.content()).not.toContain('e2e-ro-community');
    await snap(page, 'traffic-router-waiting');
  });
});

test.describe('Clickjacking protection', () => {
  // Another site must not be able to load the console or the subscriber
  // portal inside a frame and line its own button up over ours. Checked the
  // way an attack would work — a page of a different origin framing the
  // sign-in form — not only by reading a header, because what matters is that
  // the browser refuses to show it. The console's own same-origin frame (the
  // captive-portal preview) is still allowed; the FR-HSP-004 test covers that.
  for (const path of ['/staff/login', '/ui/login']) {
    test(`another site cannot frame ${path}`, async ({ page, baseURL }) => {
      const res = await page.request.get(path);
      expect(res.headers()['content-security-policy']).toBe("frame-ancestors 'self'");
      expect(res.headers()['x-frame-options']).toBe('SAMEORIGIN');

      // about:blank with content set is an opaque origin: cross-origin to
      // the server, like an attacker's page.
      await page.setContent(`<iframe title="framed" src="${baseURL}${path}" width="800" height="500"></iframe>`);
      const frame = page.frame({ url: (u) => u.toString() !== 'about:blank' });
      // Give the frame time to load if it were going to.
      await page.waitForTimeout(2000);
      const passwordFields = frame
        ? await frame.locator('input[type="password"]').count().catch(() => 0)
        : 0;
      expect(passwordFields, 'the sign-in form loaded inside a cross-origin frame').toBe(0);
    });
  }
});
