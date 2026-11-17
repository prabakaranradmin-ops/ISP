import { test, expect, Page } from '@playwright/test';
import * as fs from 'fs';
import * as path from 'path';

/**
 * Operations console, end to end in a real browser.
 *
 * The console's job is to show five different roles five different subsets of
 * a customer's data, so most of what is worth testing is negative: not "can the
 * owner see the ledger" but "does the technician still not see it after typing
 * the URL directly". Hiding a nav link is a convenience; the 403 is the control.
 *
 * Requires a stack from ./scripts/demo_up.sh, seeded with the five staff
 * accounts.
 */

const PASSWORD = 'staffpassword';

// Snapshots are written for the report rather than compared against a baseline.
// Pixel-diffing a page whose figures move with the seed date would fail for
// reasons that have nothing to do with the console working.
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
  // Every ISP-wide role lands on the dashboard: Home redirects to the first
  // section a role may open, and the dashboard leads the section table.
  await expect(page).toHaveURL(/\/staff\/dashboard/);
}

/** Sections every role should and should not reach, mirroring the API. */
const MATRIX = {
  owner:   { role: 'isp_owner',     allow: ['subscribers', 'billing', 'tickets', 'revenue', 'lea'] },
  noc:     { role: 'noc_engineer',  allow: ['subscribers', 'lea'] },
  billing: { role: 'billing_admin', allow: ['subscribers', 'billing'] },
  csr:     { role: 'csr',           allow: ['subscribers', 'billing', 'tickets'] },
  tech:    { role: 'technician',    allow: ['subscribers', 'tickets'] },
} as const;

const ALL_SECTIONS = ['subscribers', 'billing', 'tickets', 'revenue', 'lea'] as const;

test.describe('Console authentication', () => {
  test('the sign-in page renders', async ({ page }) => {
    await page.goto('/staff/login');
    await expect(page.getByRole('heading', { name: 'Operations Console' })).toBeVisible();
    await expect(page.getByLabel('Username')).toBeVisible();
    await snap(page, 'login');
  });

  test('all five staff accounts can sign in', async ({ page }) => {
    for (const user of Object.keys(MATRIX)) {
      await signIn(page, user);
      await expect(page.locator('.who .role')).toHaveText(MATRIX[user as keyof typeof MATRIX].role);
      await page.getByRole('button', { name: 'Sign out' }).click();
      await expect(page).toHaveURL(/\/staff\/login/);
    }
  });

  test('a wrong password is refused without naming which field was wrong', async ({ page }) => {
    await page.goto('/staff/login');
    await page.getByLabel('Username').fill('owner');
    await page.getByLabel('Password').fill('not-the-password');
    await page.getByRole('button', { name: 'Sign in' }).click();

    await expect(page.getByText('Incorrect username or password')).toBeVisible();
    await expect(page).not.toHaveURL(/\/staff\/dashboard/);

    // Naming the wrong field would confirm the username exists, turning the
    // form into a way to enumerate staff accounts.
    const body = (await page.textContent('body')) ?? '';
    expect(body.toLowerCase()).not.toContain('no such user');
    expect(body.toLowerCase()).not.toContain('unknown user');
    await snap(page, 'login-rejected');
  });

  test('an unknown username is refused the same way', async ({ page }) => {
    await page.goto('/staff/login');
    await page.getByLabel('Username').fill('nobody-here');
    await page.getByLabel('Password').fill(PASSWORD);
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(page.getByText('Incorrect username or password')).toBeVisible();
  });

  test('every console page bounces an unauthenticated visitor to sign-in', async ({ page }) => {
    for (const section of ALL_SECTIONS) {
      await page.goto(`/staff/${section}`);
      await expect(page, `/staff/${section} should redirect when signed out`).toHaveURL(/\/staff\/login/);
    }
  });

  test('signing out ends the session', async ({ page }) => {
    await signIn(page, 'owner');
    await page.getByRole('button', { name: 'Sign out' }).click();
    await expect(page).toHaveURL(/\/staff\/login/);
    await page.goto('/staff/revenue');
    await expect(page).toHaveURL(/\/staff\/login/);
  });
});

test.describe('Role-based access', () => {
  for (const [user, { allow }] of Object.entries(MATRIX)) {
    test(`${user} reaches exactly its own sections`, async ({ page }) => {
      await signIn(page, user);

      for (const section of ALL_SECTIONS) {
        const permitted = (allow as readonly string[]).includes(section);
        const res = await page.request.get(`/staff/${section}`);

        if (permitted) {
          expect(res.status(), `${user} should reach ${section}`).toBe(200);
        } else {
          // Typing the URL is the real test. The navigation not offering a
          // link proves nothing about what the server will serve.
          expect(res.status(), `${user} must be refused ${section}`).toBe(403);
        }
      }
    });

    test(`${user} sees only its own navigation`, async ({ page }) => {
      await signIn(page, user);
      const labels: Record<string, string> = {
        subscribers: 'Subscribers', billing: 'Billing', tickets: 'Support',
        revenue: 'Revenue', lea: 'LEA Lookup',
      };
      for (const section of ALL_SECTIONS) {
        // Counted rather than checked for visibility: most links sit inside
        // a closed top-bar menu until it is opened, and a link in a closed
        // <details> is still offered — it is one click away, not withheld.
        // Not offering it means it is absent from the page altogether.
        const link = page.locator('.topbar nav').getByRole('link', { name: labels[section], exact: true, includeHidden: true });
        if ((allow as readonly string[]).includes(section)) {
          await expect(link, `${user} nav should offer ${section}`).toHaveCount(1);
        } else {
          await expect(link, `${user} nav must not offer ${section}`).toHaveCount(0);
        }
      }
      await snap(page, `nav-${user}`);
    });
  }
});

test.describe('Dashboard and menus', () => {
  test('the dashboard counts subscribers and each tile opens its list', async ({ page }) => {
    await signIn(page, 'owner');
    // Linked tiles only: the status names also appear in the legend inside
    // the (unlinked) "All subscribers" tile.
    const tile = (label: string) => page.locator('a.tile', { hasText: label });
    // Counts are checked for consistency rather than against fixed numbers:
    // other tests (lead conversion) add subscribers to the same database,
    // as real use does. The total must equal its own per-status breakdown,
    // and the seed guarantees at least one active, one hard-suspended and
    // one online subscriber.
    const num = async (loc: ReturnType<Page['locator']>) => Number((await loc.innerText()).trim());
    const total = await num(page.locator('.tile-total .tile-num'));
    const legend = await page.locator('.tile-total .legend b').allInnerTexts();
    expect(legend.reduce((sum, n) => sum + Number(n), 0)).toBe(total);
    expect(await num(tile('Online now').locator('.tile-num'))).toBeGreaterThanOrEqual(1);
    expect(await num(tile('Active').locator('.tile-num'))).toBeGreaterThanOrEqual(1);
    expect(await num(tile('Hard suspended').locator('.tile-num'))).toBeGreaterThanOrEqual(1);
    await snap(page, 'dashboard-owner');

    await tile('Hard suspended').click();
    await expect(page).toHaveURL(/\/staff\/subscribers\?status=hard_suspended/);
    await expect(page.locator('tbody').getByText('suspended_user', { exact: true })).toBeVisible();
    await expect(page.locator('tbody').getByText('test_user', { exact: true })).toHaveCount(0);
  });

  test('ticket figures are shown only to roles that work tickets', async ({ page }) => {
    await signIn(page, 'csr');
    await expect(page.getByText('Open tickets')).toBeVisible();
    await page.getByRole('button', { name: 'Sign out' }).click();
    await signIn(page, 'noc');
    await expect(page.getByText('Open tickets')).toHaveCount(0);
  });

  test('a top-bar menu opens, navigates, and marks where you are', async ({ page }) => {
    await signIn(page, 'owner');
    const finance = page.locator('.topbar .navgroup', { hasText: 'Finance' });
    await finance.locator('summary').click();
    await expect(finance.getByRole('link', { name: 'General Ledger' })).toBeVisible();
    await snap(page, 'nav-menu-open');
    await finance.getByRole('link', { name: 'General Ledger' }).click();
    await expect(page).toHaveURL(/\/staff\/ledger/);
    await expect(page.locator('.topbar .navgroup.on summary')).toHaveText('Finance');
  });

  test('opening one menu closes the other', async ({ page }) => {
    await signIn(page, 'owner');
    const finance = page.locator('.topbar .navgroup', { hasText: 'Finance' });
    const network = page.locator('.topbar .navgroup', { hasText: 'Network' });
    await finance.locator('summary').click();
    await network.locator('summary').click();
    await expect(network).toHaveAttribute('open', '');
    await expect(finance).not.toHaveAttribute('open', '');
  });
});

test.describe('Payments with method', () => {
  test('billing records a cheque, it is listed with its details, and a bounce reverses it', async ({ page }) => {
    const chequeNo = Date.now().toString().slice(-6);
    await signIn(page, 'billing');
    await page.goto('/staff/subscribers/1');
    await page.getByRole('button', { name: 'Billing', exact: true }).click();
    await page.fill('input[name="amount"]', '1000.00');
    await page.selectOption('select[name="method"]', 'cheque');
    await page.fill('input[name="cheque_number"]', chequeNo);
    await page.fill('input[name="cheque_bank"]', 'State Bank of India');
    await page.fill('input[name="cheque_date"]', new Date().toISOString().slice(0, 10));
    page.once('dialog', (d) => d.accept());
    await page.getByRole('button', { name: 'Record payment' }).click();
    await expect(page.getByText(/Payment of .1000\.00 recorded/)).toBeVisible();

    await page.goto('/staff/invoices?tab=payments&from=2000-01-01');
    const row = page.locator('tbody tr', { hasText: `Cheque ${chequeNo}` });
    await expect(row).toContainText('State Bank of India');
    await expect(row).toContainText('1000.00');
    await snap(page, 'payments-cheque');

    await row.locator('input[name="reason"]').fill('Insufficient funds');
    page.once('dialog', (d) => d.accept());
    await row.getByRole('button', { name: 'Bounced' }).click();
    await expect(page.getByText('Cheque marked bounced')).toBeVisible();
    await expect(page.locator('tbody tr', { hasText: `Cheque ${chequeNo}` })).toContainText('bounced');
    await expect(page.locator('tbody tr', { hasText: `Cheque ${chequeNo}` }).getByRole('button', { name: 'Bounced' })).toHaveCount(0);
  });

  test('a cheque without its bank is refused with a reason', async ({ page }) => {
    await signIn(page, 'billing');
    await page.goto('/staff/subscribers/1');
    await page.getByRole('button', { name: 'Billing', exact: true }).click();
    await page.fill('input[name="amount"]', '10');
    await page.selectOption('select[name="method"]', 'cheque');
    await page.fill('input[name="cheque_number"]', '1');
    page.once('dialog', (d) => d.accept());
    await page.getByRole('button', { name: 'Record payment' }).click();
    await expect(page.getByText(/a cheque needs its number, bank and date/)).toBeVisible();
  });

  test('a CSR is not offered the payment form', async ({ page }) => {
    await signIn(page, 'csr');
    await page.goto('/staff/subscribers/1');
    await expect(page.getByRole('button', { name: 'Record payment' })).toHaveCount(0);
  });
});

/** A date n days from today, in the browser's own calendar, as YYYY-MM-DD. */
function daysFromToday(n: number): string {
  const d = new Date(Date.now() + n * 864e5);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

async function openLapsedBilling(page: Page) {
  await page.goto('/staff/subscribers');
  await page.getByPlaceholder('Username, ID, phone, email, CAF or IP').fill('lapsed_user');
  await page.getByRole('button', { name: 'Search' }).click();
  await page.locator('tbody tr', { hasText: 'lapsed_user' }).getByRole('link', { name: 'Open' }).click();
  await page.getByRole('button', { name: 'Billing', exact: true }).click();
}

test.describe('Grace and dunning schedule', () => {
  test('billing extends a lapsed subscriber\'s grace with a reason, then ends it', async ({ page }) => {
    await signIn(page, 'billing');
    await openLapsedBilling(page);
    const card = page.locator('#grace');
    await expect(card).toContainText('Grace extension');

    await card.locator('input[name="until"]').fill(daysFromToday(4));
    await card.locator('input[name="reason"]').fill('Promised to pay on salary day');
    page.once('dialog', (d) => d.accept());
    await card.getByRole('button', { name: 'Extend grace' }).click();
    await expect(page.getByText(/^Grace extended\./)).toBeVisible();

    await page.getByRole('button', { name: 'Billing', exact: true }).click();
    await expect(card).toContainText('Held in grace');
    await expect(card).toContainText('Promised to pay on salary day');
    await expect(card).toContainText('billing');
    await snap(page, 'grace-extended');

    page.once('dialog', (d) => d.accept());
    await card.getByRole('button', { name: 'End extension' }).click();
    await expect(page.getByText(/^Grace extension ended\./)).toBeVisible();
    await page.getByRole('button', { name: 'Billing', exact: true }).click();
    await expect(card.getByRole('button', { name: 'Extend grace' })).toBeVisible();
  });

  test('a subscriber who has paid is not offered grace', async ({ page }) => {
    await signIn(page, 'billing');
    await page.goto('/staff/subscribers/1');
    await page.getByRole('button', { name: 'Billing', exact: true }).click();
    await expect(page.locator('#grace')).toContainText('Not needed');
    await expect(page.getByRole('button', { name: 'Extend grace' })).toHaveCount(0);
  });

  test('billing changes the schedule, sees it spelled out, and a schedule out of order is refused', async ({ page }) => {
    await signIn(page, 'billing');
    await page.goto('/staff/dunning');
    const save = async (first: string, second: string) => {
      await page.fill('input[name="remind_first_days"]', first);
      await page.fill('input[name="remind_second_days"]', second);
      page.once('dialog', (d) => d.accept());
      await page.getByRole('button', { name: 'Save schedule' }).click();
    };
    try {
      await save('10', '3');
      await expect(page.getByText(/^Saved\./)).toBeVisible();
      await expect(page.locator('tbody')).toContainText('10 days before expiry');
      await expect(page.getByText(/Last changed by billing/)).toBeVisible();
      await snap(page, 'dunning-settings');

      await save('2', '5');
      await expect(page.getByText(/reminders must count down/)).toBeVisible();
      // The figures typed stay in the form to be corrected.
      await expect(page.locator('input[name="remind_second_days"]')).toHaveValue('5');
    } finally {
      await page.goto('/staff/dunning');
      await save('7', '3');
      await expect(page.getByText(/^Saved\./)).toBeVisible();
    }
  });

  test('a CSR can neither open the schedule nor extend grace', async ({ page }) => {
    await signIn(page, 'csr');
    const resp = await page.goto('/staff/dunning');
    expect(resp?.status()).toBe(403);
    await openLapsedBilling(page);
    await expect(page.locator('#grace')).toHaveCount(0);
  });
});

test.describe('Proforma invoices', () => {
  /** Issue a proforma from a subscriber's Billing tab; returns its row on the list it lands on. */
  async function issueProforma(page: Page, subscriberPath: string, what: string, amount: string) {
    await page.goto(subscriberPath);
    await page.getByRole('button', { name: 'Billing', exact: true }).click();
    const card = page.locator('#proforma');
    await card.locator('input[name="description"]').fill(what);
    await card.locator('input[name="amount"]').fill(amount);
    await card.getByRole('button', { name: 'Issue proforma' }).click();
    await expect(page.getByText(/^Proforma issued\./)).toBeVisible();
    return page.locator('tbody tr', { hasText: what });
  }

  test('billing issues a proforma, the customer pays, and converting it issues the tax invoice', async ({ page }) => {
    const what = `Installation ${Date.now()}`;
    await signIn(page, 'billing');
    let row = await issueProforma(page, '/staff/subscribers/1', what, '100.00');
    await expect(row).toContainText('PF-');
    await expect(row).toContainText('118.00'); // 100 + 9% + 9%, Tamil Nadu subscriber
    await expect(row).toContainText('open');

    // The document itself: a real PDF that calls itself a proforma.
    const pdfHref = await row.getByRole('link', { name: 'PDF' }).getAttribute('href');
    const pdf = await page.request.get(pdfHref!);
    expect(pdf.status()).toBe(200);
    expect(pdf.headers()['content-type']).toContain('application/pdf');
    const pdfBytes = await pdf.body();
    expect(pdfBytes.subarray(0, 5).toString()).toBe('%PDF-');
    fs.writeFileSync(path.join(SHOTS, 'proforma.pdf'), pdfBytes); // for the report, like the screenshots
    await snap(page, 'proformas-open');

    // The customer pays first: the tax invoice follows the payment.
    await page.goto('/staff/subscribers/1');
    await page.getByRole('button', { name: 'Billing', exact: true }).click();
    await page.fill('input[name="amount"]', '118.00');
    await page.selectOption('select[name="method"]', 'cash');
    page.once('dialog', (d) => d.accept());
    await page.getByRole('button', { name: 'Record payment' }).click();
    await expect(page.getByText(/Payment of .118\.00 recorded/)).toBeVisible();

    await page.goto('/staff/invoices/proformas?subscriber=1');
    row = page.locator('tbody tr', { hasText: what });
    page.once('dialog', (d) => d.accept());
    await row.getByRole('button', { name: 'Convert' }).click();
    await expect(page.getByText(/^Converted\./)).toBeVisible();
    // It leaves the Open list and shows on Converted with its invoice number.
    await expect(page.locator('tbody tr', { hasText: what })).toHaveCount(0);
    await page.getByRole('navigation', { name: 'Filter by status' }).getByRole('link', { name: 'Converted' }).click();
    row = page.locator('tbody tr', { hasText: what });
    await expect(row).toContainText('converted');
    const invoiceNo = (await row.getByRole('link', { name: /^INV-/ }).textContent())!.trim();

    // And the tax invoice is on the Invoices list, at 118.00.
    await page.goto('/staff/invoices?tab=invoices');
    await expect(page.locator('tbody tr', { hasText: invoiceNo })).toContainText('118.00');
  });

  test('converting before the customer has paid is refused, and a proforma can be cancelled', async ({ page }) => {
    const what = `Router ${Date.now()}`;
    await signIn(page, 'billing');
    await page.goto('/staff/subscribers');
    await page.getByPlaceholder('Username, ID, phone, email, CAF or IP').fill('lapsed_user');
    await page.getByRole('button', { name: 'Search' }).click();
    const subscriberPath = await page.locator('tbody tr', { hasText: 'lapsed_user' }).getByRole('link', { name: 'Open' }).getAttribute('href');

    let row = await issueProforma(page, subscriberPath!, what, '2500.00');
    page.once('dialog', (d) => d.accept());
    await row.getByRole('button', { name: 'Convert' }).click();
    await expect(page.getByText(/does not cover the invoice total/)).toBeVisible();
    row = page.locator('tbody tr', { hasText: what });
    await expect(row).toContainText('open');

    await row.locator('input[name="reason"]').fill('Customer chose another provider');
    page.once('dialog', (d) => d.accept());
    await row.getByRole('button', { name: 'Cancel' }).click();
    await expect(page.getByText(/^Proforma cancelled\./)).toBeVisible();
    await page.getByRole('navigation', { name: 'Filter by status' }).getByRole('link', { name: 'Cancelled' }).click();
    await expect(page.locator('tbody tr', { hasText: what })).toContainText('Customer chose another provider');
  });

  test('a CSR can see proformas but not issue, convert or cancel them', async ({ page }) => {
    await signIn(page, 'csr');
    await page.goto('/staff/invoices/proformas?status=all');
    await expect(page.getByRole('heading', { name: 'Invoices & payments' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Convert' })).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Cancel' })).toHaveCount(0);
    await page.goto('/staff/subscribers/1');
    await expect(page.locator('#proforma')).toHaveCount(0);
  });
});

test.describe('Billing diagnostics', () => {
  test('billing finds the seeded uninvoiced subscriber from the Finance menu and opens their record', async ({ page }) => {
    await signIn(page, 'billing');
    const finance = page.locator('.topbar .navgroup', { hasText: 'Finance' });
    await finance.locator('summary').click();
    await finance.getByRole('link', { name: 'Billing Diagnostics' }).click();
    await expect(page).toHaveURL(/\/staff\/diagnostics$/);

    const tile = page.locator('a.tile', { hasText: 'Served without an invoice' });
    expect(Number(await tile.locator('.tile-num').textContent())).toBeGreaterThanOrEqual(1);
    const row = page.locator('#uninvoiced tbody tr', { hasText: 'uninvoiced_user' });
    await expect(row).toContainText('never');
    await expect(row).toContainText('TN_Basic_50M');
    // The three checks are all on the page, each with its explanation.
    await expect(page.locator('#balances h3')).toContainText('disagrees with the ledger');
    await expect(page.locator('#prices h3')).toContainText('catalogue');
    await snap(page, 'billing-diagnostics');

    await row.getByRole('link', { name: 'uninvoiced_user' }).click();
    await expect(page).toHaveURL(/\/staff\/subscribers\/\d+$/);
    await expect(page.getByRole('heading', { name: /uninvoiced_user/ })).toBeVisible();
  });

  test('a CSR cannot open billing diagnostics', async ({ page }) => {
    await signIn(page, 'csr');
    const resp = await page.goto('/staff/diagnostics');
    expect(resp?.status()).toBe(403);
  });
});

test.describe('Payment receipts', () => {
  test('the front desk opens a payment receipt as a real PDF', async ({ page }) => {
    await signIn(page, 'csr');
    await page.goto('/staff/invoices?tab=payments&from=2000-01-01');
    const row = page.locator('tbody tr', { hasText: 'test_user' }).filter({ hasText: '799.00' }).first();
    const href = await row.getByRole('link', { name: 'Receipt' }).getAttribute('href');
    expect(href).toMatch(/^\/staff\/invoices\/payments\/\d+\/receipt$/);
    const res = await page.request.get(href!);
    expect(res.status()).toBe(200);
    expect(res.headers()['content-type']).toContain('application/pdf');
    const pdf = await res.body();
    expect(pdf.subarray(0, 5).toString()).toBe('%PDF-');
    fs.writeFileSync(path.join(SHOTS, 'receipt.pdf'), pdf); // for the report
  });
});

test.describe('Billing runs', () => {
  // Runs are written by the auto-renewal scanner in radiusd, every 15
  // minutes. A fresh stack may have recorded only passes that found nobody,
  // which the default view hides; "Every pass" shows them.
  test('billing reads the run history from the Finance menu and opens a run', async ({ page }) => {
    await signIn(page, 'billing');
    const finance = page.locator('.topbar .navgroup', { hasText: 'Finance' });
    await finance.locator('summary').click();
    await finance.getByRole('link', { name: 'Billing Runs' }).click();
    await expect(page).toHaveURL(/\/staff\/billing-runs$/);
    await expect(page.getByRole('heading', { name: 'Billing runs' })).toBeVisible();

    await page.getByRole('navigation', { name: 'Which runs' }).getByRole('link', { name: 'Every pass' }).click();
    await expect(page).toHaveURL(/idle=1/);
    const runs = page.locator('tbody tr a[href^="/staff/billing-runs/"]');
    if (await runs.count() === 0) {
      await expect(page.getByText('No runs recorded yet.')).toBeVisible();
      return;
    }
    await snap(page, 'billing-runs');
    await runs.first().click();
    await expect(page).toHaveURL(/\/staff\/billing-runs\/\d+$/);
    await expect(page.getByRole('heading', { name: /Billing run #\d+/ })).toBeVisible();
    await expect(page.getByText('Subscribers this run could not finish')).toBeVisible();
    await snap(page, 'billing-run');
  });

  test('a CSR cannot read billing runs', async ({ page }) => {
    await signIn(page, 'csr');
    expect((await page.goto('/staff/billing-runs'))?.status()).toBe(403);
    expect((await page.goto('/staff/billing-runs/1'))?.status()).toBe(403);
  });
});

test.describe('Credit notes', () => {
  test('billing requests a credit note, a second person approves it, and it is issued', async ({ page }) => {
    const reason = `Line down E2E ${Date.now()}`;
    await signIn(page, 'billing');
    await page.goto('/staff/invoices?from=2000-01-01');
    const invRow = page.locator('tbody tr', { hasText: 'test_user' }).first();
    const creditedBefore = (await invRow.locator('td').nth(6).innerText()).trim();
    await invRow.getByRole('link', { name: 'Credit note' }).click();
    await page.fill('input[name="amount"]', '1.00');
    await page.fill('input[name="reason"]', reason);
    await page.getByRole('button', { name: 'Request credit note' }).click();
    await expect(page.getByText('Nothing has been issued yet')).toBeVisible();
    await snap(page, 'credit-note-requested');

    // The requester sees it waiting but cannot approve it.
    await page.goto('/staff/tasks');
    const pending = page.locator('tr', { hasText: reason });
    await expect(pending).toContainText('Credit note');
    page.once('dialog', (d) => d.accept());
    await pending.getByRole('button', { name: 'Approve' }).click();
    await expect(page.locator('body')).toContainText(/cannot approve|own request|different staff/i);

    await page.getByRole('button', { name: 'Sign out' }).click();
    await signIn(page, 'owner');
    await page.goto('/staff/tasks');
    page.once('dialog', (d) => d.accept());
    await page.locator('tr', { hasText: reason }).getByRole('button', { name: 'Approve' }).click();
    await expect(page.locator('tr', { hasText: reason })).toHaveCount(0); // no longer pending

    // The invoice now shows 1.00 + 18% GST more credited.
    await page.goto('/staff/invoices?from=2000-01-01');
    const after = (await page.locator('tbody tr', { hasText: 'test_user' }).first().locator('td').nth(6).innerText()).trim();
    const num = (s: string) => (s === '—' ? 0 : Number(s));
    expect(num(after) - num(creditedBefore)).toBeCloseTo(1.18, 2);
  });

  test('a CSR is not offered credit notes', async ({ page }) => {
    await signIn(page, 'csr');
    await page.goto('/staff/invoices?from=2000-01-01');
    await expect(page.getByRole('link', { name: 'Credit note' })).toHaveCount(0);
    expect((await page.request.get('/staff/invoices/1/credit-note')).status()).toBe(403);
  });
});

test.describe('Message logs', () => {
  test('a CSR can search the message log but not see partner webhooks', async ({ page }) => {
    await signIn(page, 'csr');
    await page.goto('/staff/logs');
    await expect(page.getByRole('heading', { name: 'Message logs' })).toBeVisible();
    await expect(page.getByRole('link', { name: 'Partner webhooks' })).toHaveCount(0);
    await page.selectOption('select[name="channel"]', 'sms');
    await page.selectOption('select[name="status"]', 'failed');
    await page.getByRole('button', { name: 'Show' }).click();
    await expect(page).toHaveURL(/channel=sms/);
    await expect(page).toHaveURL(/status=failed/);
    // Either rows or the empty state; never an error banner.
    await expect(page.locator('.alert.bad')).toHaveCount(0);
    expect((await page.request.get('/staff/logs?tab=webhooks')).status()).toBe(403);
  });

  test('the owner sees the partner webhook tab', async ({ page }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/logs');
    await page.getByRole('link', { name: 'Partner webhooks' }).click();
    await expect(page).toHaveURL(/tab=webhooks/);
    await expect(page.locator('.alert.bad')).toHaveCount(0);
    await snap(page, 'logs-webhooks');
  });

  test('a technician cannot open the logs', async ({ page }) => {
    await signIn(page, 'tech');
    expect((await page.request.get('/staff/logs')).status()).toBe(403);
  });
});

test.describe('Announcements', () => {
  test('a draft is reviewed with its reach, sent once, and cannot be sent twice', async ({ page }) => {
    const title = `Maintenance E2E ${Date.now()}`;
    await signIn(page, 'billing');
    await page.goto('/staff/announcements');
    await page.locator('details.card summary').click();
    await page.fill('input[name="title"]', title);
    await page.fill('input[name="body"]', 'Service may drop between 1 and 3 am.');
    await page.selectOption('select[name="class"]', 'transactional');
    await page.selectOption('select[name="segment_status"]', 'active');
    await page.locator('input[name="channels"][value="sms"]').check();
    await page.getByRole('button', { name: 'Save draft' }).click();
    await expect(page.getByText('Draft saved.')).toBeVisible();

    const row = page.locator('tbody tr', { hasText: title });
    await expect(row).toContainText('draft');
    await expect(row).toContainText(/Reaches \d+ now/);
    await snap(page, 'announcements-draft');

    // Keep the send form's CSRF token, to replay the same send afterwards.
    const csrf = await row.locator('input[name="csrf_token"]').inputValue();
    const action = await row.locator('form').getAttribute('action');
    page.once('dialog', (d) => d.accept());
    await row.getByRole('button', { name: 'Send' }).click();
    await expect(page.getByText(/Sent to \d+ subscribers?: \d+ messages? queued/)).toBeVisible();
    await expect(page.locator('tbody tr', { hasText: title })).toContainText('sent');

    // A double-click, or a replayed request, must not broadcast again.
    const again = await page.request.post(action!, { form: { csrf_token: csrf } });
    expect(await again.text()).toContain('already been sent');
  });

  test('a CSR cannot open announcements', async ({ page }) => {
    await signIn(page, 'csr');
    expect((await page.request.get('/staff/announcements')).status()).toBe(403);
  });
});

test.describe('CPE devices', () => {
  test('a technician queues a reboot and sees it waiting for the device', async ({ page }) => {
    await signIn(page, 'tech');
    await page.goto('/staff/cpe');
    // seed_local.sql issues one TR-069 router to test_user.
    const row = page.locator('tbody tr', { hasText: 'HWTC-DEMO-0001' });
    await expect(row).toContainText('test_user');
    await row.getByRole('link', { name: 'HWTC-DEMO-0001' }).click();
    await expect(page.getByText(/next time this device checks in/)).toBeVisible();
    await snap(page, 'cpe-device');

    page.once('dialog', (d) => d.accept());
    await page.getByRole('button', { name: 'Queue reboot' }).click();
    await expect(page.getByText(/Queued\. It runs the next time the device checks in/)).toBeVisible();
    const task = page.locator('tbody tr', { hasText: 'Reboot' }).first();
    await expect(task).toContainText('pending');
    await expect(task).toContainText('tech');
  });

  test('a bad firmware URL is refused before anything is queued', async ({ page }) => {
    await signIn(page, 'noc');
    await page.goto('/staff/cpe/HWTC-DEMO-0001');
    const before = await page.locator('tbody tr', { hasText: 'Download' }).count();
    // The input is type=url; set it directly so the server-side check, not
    // the browser's, is what is exercised.
    await page.locator('input[name="arg"][type="url"]').evaluate((el: HTMLInputElement) => { el.type = 'text'; el.value = 'ftp://x/fw.bin'; });
    page.once('dialog', (d) => d.accept());
    await page.getByRole('button', { name: 'Queue upgrade' }).click();
    await expect(page.getByText(/Not queued: url must be an http/)).toBeVisible();
    expect(await page.locator('tbody tr', { hasText: 'Download' }).count()).toBe(before);
  });

  test('a CSR cannot open the CPE screen', async ({ page }) => {
    await signIn(page, 'csr');
    expect((await page.request.get('/staff/cpe')).status()).toBe(403);
  });
});

test.describe('Leads', () => {
  test('a lead is added, worked, and converted into a pending-payment subscriber', async ({ page }) => {
    const stamp = Date.now().toString().slice(-7);
    const name = `Lead E2E ${stamp}`;
    const mobile = `+91981${stamp}`;

    await signIn(page, 'csr');
    await page.goto('/staff/leads');
    await page.locator('details.card summary').click();
    await page.fill('input[name="full_name"]', name);
    await page.fill('input[name="mobile_number"]', mobile);
    await page.selectOption('select[name="source"]', 'referral');
    await page.getByRole('button', { name: 'Add lead' }).click();
    await expect(page.getByText(new RegExp(`Lead #\\d+ added for ${name}`))).toBeVisible();

    const row = page.locator('tbody tr', { hasText: name });
    await row.locator('select[name="status"]').selectOption('qualified');
    await row.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByText('Lead updated.')).toBeVisible();
    await expect(page.locator('tbody tr', { hasText: name })).toContainText('Qualified');
    // A CSR works leads but cannot turn one into a billable account.
    await expect(page.locator('tbody tr', { hasText: name }).getByRole('link', { name: 'Convert' })).toHaveCount(0);
    await snap(page, 'leads');

    await page.getByRole('button', { name: 'Sign out' }).click();
    await signIn(page, 'billing');
    await page.goto('/staff/leads');
    await page.locator('tbody tr', { hasText: name }).getByRole('link', { name: 'Convert' }).click();
    await expect(page.getByText(mobile)).toBeVisible(); // carried over, not retyped
    await page.fill('input[name="caf_number"]', `CAF-E2E-${stamp}`);
    await page.fill('input[name="username"]', `lead_e2e_${stamp}`);
    await page.fill('input[name="password"]', 'e2e-password-123');
    await page.fill('input[name="registered_state"]', 'TN');
    await page.getByRole('button', { name: 'Create subscriber' }).click();
    await expect(page.getByText('Subscriber created')).toBeVisible();
    await expect(page.locator('.pill', { hasText: 'pending payment' })).toBeVisible();
    await snap(page, 'lead-converted');

    // The new account really is pending payment, not on the network for free.
    await page.getByRole('link', { name: 'Open the subscriber' }).click();
    await expect(page.locator('body')).toContainText('pending_payment');
  });

  test('a lead cannot be closed as lost without a reason', async ({ page }) => {
    await signIn(page, 'csr');
    await page.goto('/staff/leads');
    // Its own lead, so the test never depends on what an earlier one left.
    const stamp = Date.now().toString().slice(-7);
    const name = `Lost E2E ${stamp}`;
    await page.locator('details.card summary').click();
    await page.fill('input[name="full_name"]', name);
    await page.fill('input[name="mobile_number"]', `+91982${stamp}`);
    await page.getByRole('button', { name: 'Add lead' }).click();
    const row = page.locator('tbody tr', { hasText: name });
    await row.locator('select[name="status"]').selectOption('lost');
    await row.locator('input[name="lost_reason"]').fill('');
    await row.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByText(/why the lead was lost/)).toBeVisible();
  });
});

test.describe('Invoices and payments', () => {
  test('billing sees this month\'s invoices with totals and a working PDF link', async ({ page }) => {
    await signIn(page, 'billing');
    const finance = page.locator('.topbar .navgroup', { hasText: 'Finance' });
    await finance.locator('summary').click();
    await finance.getByRole('link', { name: 'Invoices & Payments' }).click();
    await expect(page).toHaveURL(/\/staff\/invoices/);
    // seed_local.sql raises one invoice for test_user: 799 + 18% GST. Other
    // tests add newer ones (a converted proforma), so find it by its total.
    const row = page.locator('tbody tr', { hasText: 'test_user' }).filter({ hasText: '942.82' }).first();
    await expect(row).toBeVisible();
    await expect(row).toContainText(/INV-\d{6}/);
    await expect(page.locator('.summary-strip')).toContainText('Invoiced total');
    await snap(page, 'invoices');

    // The PDF needs a Chromium-based browser on the server. Either it renders
    // or the console says plainly that it cannot; it must not 500 or blank.
    const href = await row.getByRole('link', { name: 'PDF' }).getAttribute('href');
    const res = await page.request.get(href!);
    expect([200, 503]).toContain(res.status());
    if (res.status() === 200) {
      expect(res.headers()['content-type']).toBe('application/pdf');
      expect((await res.body()).subarray(0, 4).toString()).toBe('%PDF');
    }
  });

  test('payments list the seeded top-up and export as CSV', async ({ page }) => {
    await signIn(page, 'csr');
    await page.goto('/staff/invoices?tab=payments');
    // The seed's 799.00 top-up, wherever later tests' payments have put it.
    await expect(page.locator('tbody tr', { hasText: 'test_user' }).filter({ hasText: '799.00' }).first()).toBeVisible();
    const res = await page.request.get('/staff/invoices?tab=payments&format=csv');
    expect(res.status()).toBe(200);
    expect(res.headers()['content-disposition']).toMatch(/^attachment;/);
    const csv = await res.text();
    expect(csv.split('\n')[0]).toContain('payment_id,date,subscriber_id,username,amount');
    expect(csv).toContain('test_user');
  });

  test('a technician cannot open invoices or their PDFs', async ({ page }) => {
    await signIn(page, 'tech');
    expect((await page.request.get('/staff/invoices')).status()).toBe(403);
    expect((await page.request.get('/staff/invoices/1/pdf')).status()).toBe(403);
  });
});

test.describe('Sessions', () => {
  test('a CSR sees who is online, with no way to disconnect them', async ({ page }) => {
    await signIn(page, 'csr');
    await page.goto('/staff/sessions');
    const row = page.locator('tbody tr', { hasText: 'test_user' });
    await expect(row).toContainText('100.64.0.7');
    await expect(row).toContainText('10.10.0.1');
    await expect(page.getByRole('button', { name: 'Disconnect' })).toHaveCount(0);
    await snap(page, 'sessions-live-csr');
  });

  test('the dashboard Online tile opens the live list', async ({ page }) => {
    await signIn(page, 'owner');
    await page.locator('a.tile', { hasText: 'Online now' }).click();
    await expect(page).toHaveURL(/\/staff\/sessions$/);
    await expect(page.locator('tbody tr', { hasText: 'test_user' })).toBeVisible();
  });

  test('completed sessions list the seeded history for a date range', async ({ page }) => {
    await signIn(page, 'csr');
    await page.goto('/staff/sessions');
    await page.getByRole('link', { name: 'Completed' }).click();
    const weekAgo = new Date(Date.now() - 7 * 86_400_000).toISOString().slice(0, 10);
    await page.locator('input[name="from"]').fill(weekAgo);
    await page.getByRole('button', { name: 'Show' }).click();
    await expect(page).toHaveURL(/tab=completed/);
    // seed_local.sql writes four closed sessions for test_user this month.
    await expect(page.locator('tbody tr', { hasText: 'User-Request' }).first()).toBeVisible();
    await expect(page.locator('tbody tr', { hasText: 'demo-live-001' })).toHaveCount(0);
    await snap(page, 'sessions-completed');
  });

  test('a disconnect without the CSRF token is refused', async ({ page }) => {
    await signIn(page, 'noc');
    const res = await page.request.post('/staff/sessions/demo-live-001/disconnect', {
      form: { username: 'test_user' },
    });
    expect(res.status()).toBe(403);
  });

  test('a CSR posting a disconnect directly is refused', async ({ page }) => {
    await signIn(page, 'csr');
    // CSRs get no Disconnect form, so take a valid token from a page every
    // role has. With a good token the request gets past CSRF, so the 403 is
    // the role check itself, not the token check standing in for it.
    await page.goto('/staff/change-password');
    const csrf = await page.locator('input[name="csrf_token"]').first().inputValue();
    expect(csrf).not.toBe('');
    const res = await page.request.post('/staff/sessions/demo-live-001/disconnect', {
      form: { username: 'test_user', csrf_token: csrf },
    });
    expect(res.status()).toBe(403);
  });

  // Last in this block: it queues a real PoD for the seeded session.
  test('a NOC engineer disconnects a session after confirming', async ({ page }) => {
    await signIn(page, 'noc');
    await page.goto('/staff/sessions');
    page.once('dialog', (d) => d.accept());
    await page.locator('tbody tr', { hasText: 'test_user' }).getByRole('button', { name: 'Disconnect' }).click();
    await expect(page.getByText(/Disconnect sent for test_user/)).toBeVisible();
    await snap(page, 'sessions-disconnect-sent');
  });

  test('dismissing the confirmation sends nothing', async ({ page }) => {
    await signIn(page, 'noc');
    await page.goto('/staff/sessions');
    page.once('dialog', (d) => d.dismiss());
    await page.locator('tbody tr', { hasText: 'test_user' }).getByRole('button', { name: 'Disconnect' }).click();
    await expect(page.getByText(/Disconnect sent/)).toHaveCount(0);
    await expect(page).toHaveURL(/\/staff\/sessions$/);
  });
});

test.describe('Subscribers', () => {
  test('the directory lists subscribers and search filters them', async ({ page }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/subscribers');
    await expect(page.locator('table tbody tr').first()).toBeVisible();
    await snap(page, 'subscribers-list');

    const search = page.getByPlaceholder('Username, ID, phone, email, CAF or IP');
    await search.fill('test_user');
    await page.getByRole('button', { name: 'Search' }).click();
    await expect(page.locator('tbody').getByText('test_user', { exact: true })).toBeVisible();

    // A caller reads out a phone number, not a username.
    await search.fill('9876543211');
    await page.getByRole('button', { name: 'Search' }).click();
    await expect(page.locator('tbody').getByText('suspended_user', { exact: true })).toBeVisible();
    await expect(page.locator('tbody').getByText('test_user', { exact: true })).toHaveCount(0);

    // Or the IP a NOC engineer sees in a router log: the live session's.
    await search.fill('100.64.0.7');
    await page.getByRole('button', { name: 'Search' }).click();
    await expect(page.locator('tbody').getByText('test_user', { exact: true })).toBeVisible();
    await expect(page.locator('tbody').getByText('suspended_user', { exact: true })).toHaveCount(0);

    await search.fill('nothing-matches-this');
    await page.getByRole('button', { name: 'Search' }).click();
    await expect(page.getByText('No subscribers match.')).toBeVisible();
  });

  test('a status chip narrows the list to that status', async ({ page }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/subscribers');
    await page.getByRole('navigation', { name: 'Filter by status' }).getByRole('link', { name: 'Hard suspended' }).click();
    await expect(page).toHaveURL(/status=hard_suspended/);
    await expect(page.locator('tbody').getByText('suspended_user', { exact: true })).toBeVisible();
    await expect(page.locator('tbody').getByText('test_user', { exact: true })).toHaveCount(0);
  });

  test('the 360 view shows account, connection and health', async ({ page }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/subscribers/1');

    await expect(page.getByRole('heading', { name: /test_user/ })).toBeVisible();
    await expect(page.getByText('CAF-0001')).toBeVisible();
    // Seeded with a live Redis session, so the connection panel must show real
    // figures rather than the offline state.
    await expect(page.getByText('online', { exact: true })).toBeVisible();
    await expect(page.getByText(/GB of .*GB/)).toBeVisible();
    await snap(page, 'subscriber-detail-owner');
  });

  test('a missing subscriber is reported, not rendered blank', async ({ page }) => {
    await signIn(page, 'owner');
    const res = await page.request.get('/staff/subscribers/99999');
    expect(res.status()).toBe(404);
  });

  test('the ledger panel is shown to billing roles and withheld from others', async ({ page }) => {
    // A CSR is entitled to see money.
    await signIn(page, 'csr');
    await page.goto('/staff/subscribers/1');
    // The 360 view is tabbed; the ledger lives on the Billing tab.
    await page.getByRole('button', { name: 'Billing', exact: true }).click();
    await expect(page.getByText(/Recent ledger/)).toBeVisible();
    await snap(page, 'subscriber-detail-csr');

    // A ground technician is not, and the panel must be absent from the page
    // rather than merely hidden by styling.
    await page.getByRole('button', { name: 'Sign out' }).click();
    await signIn(page, 'tech');
    await page.goto('/staff/subscribers/1');
    await expect(page.getByRole('button', { name: 'Billing', exact: true })).toHaveCount(0);
    await expect(page.getByText(/Recent ledger/)).toHaveCount(0);
    await expect(page.getByRole('heading', { name: /test_user/ })).toBeVisible();
    await snap(page, 'subscriber-detail-tech');
  });
});

test.describe('Billing', () => {
  test('a lookup shows the balance and ledger', async ({ page }) => {
    await signIn(page, 'billing');
    await page.goto('/staff/billing');
    await page.getByPlaceholder('Subscriber id').fill('1');
    await page.getByRole('button', { name: 'Look up' }).click();

    await expect(page.getByText('Wallet balance')).toBeVisible();
    // Other tests keep adding payments and charges, so the seed's ₹799.00
    // credit scrolls out of the recent ledger. What must always hold: the
    // balance shown is the newest ledger row's balance-after.
    const shown = (await page.locator('p.big').textContent())!.replace(/[₹,\s]/g, '');
    const newest = page.locator('tbody tr').first();
    await expect(newest.locator('td').last()).toHaveText(new RegExp(`${shown.replace('.', '\\.')}$`));
    await snap(page, 'billing');
  });

  test('a non-numeric id is rejected with a readable message', async ({ page }) => {
    await signIn(page, 'billing');
    await page.goto('/staff/billing?subscriber_id=abc');
    await expect(page.getByText('Enter a numeric subscriber id.')).toBeVisible();
  });
});

test.describe('Support tickets', () => {
  test('a ticket can be moved through the workflow', async ({ page }) => {
    // Raise one through the subscriber portal first, so there is something
    // real to work rather than a row the test invented in the database.
    await page.goto('/ui/login');
    await page.getByLabel('Username').fill('test_user');
    await page.getByLabel('Password').fill('testpassword');
    await page.getByRole('button', { name: 'Sign in' }).click();
    await page.goto('/ui/tickets');
    const description = `Console E2E ${Date.now()}`;
    await page.selectOption('select[name="category"]', { index: 0 });
    await page.fill('textarea[name="description"]', description);
    await page.getByRole('button', { name: /Submit Ticket/i }).click();
    await expect(page.getByText(description)).toBeVisible({ timeout: 10_000 });

    await signIn(page, 'csr');
    await page.goto('/staff/tickets?subscriber_id=1');
    await expect(page.getByText(description)).toBeVisible();
    await snap(page, 'tickets');

    const row = page.locator('tr', { hasText: description });
    await row.locator('select[name="status"]').selectOption('in_progress');
    await row.getByRole('button', { name: 'Update' }).click();

    await expect(page.getByText('Ticket updated.')).toBeVisible();
    // Scoped to the row's pills: 'in_progress' also appears as an <option> in
    // the row's own dropdown, and matching both is ambiguous rather than wrong.
    // The row carries two pills (priority, then status), so the status one is
    // picked by its text rather than by being the only pill.
    await expect(page.locator('tr', { hasText: description }).locator('span.pill', { hasText: 'in_progress' })).toBeVisible();
    await snap(page, 'tickets-updated');
  });

  test('the queue spans subscribers, and assignment lands in the assignee\'s own list', async ({ page }) => {
    // A ticket raised through the portal, so the row is real.
    await page.goto('/ui/login');
    await page.getByLabel('Username').fill('test_user');
    await page.getByLabel('Password').fill('testpassword');
    await page.getByRole('button', { name: 'Sign in' }).click();
    await page.goto('/ui/tickets');
    const description = `Queue E2E ${Date.now()}`;
    await page.selectOption('select[name="category"]', { index: 0 });
    await page.fill('textarea[name="description"]', description);
    await page.getByRole('button', { name: /Submit Ticket/i }).click();
    await expect(page.getByText(description)).toBeVisible({ timeout: 10_000 });

    // No subscriber id typed: the queue is the default view.
    await signIn(page, 'csr');
    await page.goto('/staff/tickets');
    const row = page.locator('tr', { hasText: description });
    await expect(row).toBeVisible();
    await expect(page.locator('.summary-strip')).toContainText('Unassigned');
    await snap(page, 'tickets-queue');

    await row.locator('select[name="assigned_to"]').selectOption({ label: 'Suresh Babu (Ground Tech)' });
    await row.getByRole('button', { name: 'Update' }).click();
    await expect(page.getByText('Ticket updated.')).toBeVisible();
    await expect(page.locator('tr', { hasText: description })).toContainText('Suresh Babu');
    // Assignment alone leaves the status where it was.
    await expect(page.locator('tr', { hasText: description }).locator('span.pill', { hasText: 'open' })).toBeVisible();

    await page.getByRole('button', { name: 'Sign out' }).click();
    await signIn(page, 'tech');
    await page.goto('/staff/tickets');
    await page.getByRole('link', { name: 'Assigned to me' }).click();
    await expect(page.locator('tr', { hasText: description })).toBeVisible();
  });

  test('a status change without the CSRF token is refused', async ({ page }) => {
    await signIn(page, 'csr');
    await page.goto('/staff/tickets?subscriber_id=1');
    // Carries the session cookie but no token — the shape a cross-site forgery
    // takes, since the cookie rides along automatically and the token does not.
    const res = await page.request.post('/staff/tickets/1/status', {
      form: { status: 'closed', subscriber_id: '1' },
    });
    expect(res.status()).toBe(403);
  });

  test('an invalid status is rejected rather than persisted', async ({ page }) => {
    await signIn(page, 'csr');
    await page.goto('/staff/tickets?subscriber_id=1');
    const csrf = await page.locator('input[name="csrf_token"]').first().inputValue();
    const res = await page.request.post('/staff/tickets/1/status', {
      form: { status: 'not-a-status', subscriber_id: '1', csrf_token: csrf },
    });
    // The column has no CHECK constraint, so an arbitrary string would persist
    // and then render as an unknown badge everywhere it appears.
    expect(res.status()).toBe(422);
  });
});

test.describe('Revenue', () => {
  test('the owner dashboard shows the reconciliation figures', async ({ page }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/revenue');
    await expect(page.getByText('Unbilled active subscribers')).toBeVisible();
    await expect(page.getByText('Ledger variance')).toBeVisible();
    await expect(page.getByText('Total wallet balance')).toBeVisible();
    // The variance verdict uses the same ₹0.01 threshold the nightly job
    // alerts on, so the screen and the alert cannot disagree.
    await expect(page.locator('.pill.ok, .pill.bad').first()).toBeVisible();
    await snap(page, 'revenue');
  });
});

test.describe('LEA lookup', () => {
  test('the form warns that every lookup is recorded', async ({ page }) => {
    await signIn(page, 'noc');
    await page.goto('/staff/lea');
    await expect(page.getByText(/recorded against your name/i)).toBeVisible();
    await snap(page, 'lea-form');
  });

  test('a lookup resolves a subscriber from an IP and time', async ({ page }) => {
    await signIn(page, 'noc');
    await page.goto('/staff/lea');

    // The seeded session windows move with the seed date, so the window is
    // read from the page's own data rather than hard-coded to a date that
    // silently stops matching tomorrow.
    const at = new Date();
    at.setUTCDate(at.getUTCDate() - 1);
    const stamp = at.toISOString().slice(0, 11) + '06:00';

    await page.fill('input[name="public_ip"]', '100.64.0.14');
    await page.fill('input[name="timestamp"]', stamp);
    await page.getByRole('button', { name: 'Look up' }).click();

    // Either outcome is correct depending on the seed window; what must not
    // happen is an error or a blank page.
    const matched = await page.getByText('Match').isVisible().catch(() => false);
    const missed = await page.getByText(/No subscriber held that address/).isVisible().catch(() => false);
    expect(matched || missed).toBeTruthy();
    if (matched) {
      await expect(page.getByText('CAF-0001')).toBeVisible();
    }
    await snap(page, 'lea-result');
  });

  test('a lookup without the CSRF token is refused', async ({ page }) => {
    await signIn(page, 'noc');
    await page.goto('/staff/lea');
    const res = await page.request.post('/staff/lea', {
      form: { public_ip: '100.64.0.14', timestamp: '2026-08-10T06:00' },
    });
    expect(res.status()).toBe(403);
  });

  test('a malformed timestamp is reported, not swallowed', async ({ page }) => {
    await signIn(page, 'noc');
    await page.goto('/staff/lea');
    await page.fill('input[name="public_ip"]', '100.64.0.14');
    await page.fill('input[name="timestamp"]', 'yesterday');
    await page.getByRole('button', { name: 'Look up' }).click();
    await expect(page.getByText(/Enter the time as/)).toBeVisible();
  });
});
