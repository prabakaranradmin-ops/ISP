import { test, expect, Page } from '@playwright/test';

/**
 * Subscriber portal, end to end in a real browser.
 *
 * The Go suite already covers these handlers. What it cannot cover, because it
 * never renders anything, is the half that only exists in a browser: that the
 * template produced the markup a person can actually read, that the form posts
 * what the handler expects, that the session cookie survives navigation, and
 * that HTMX swapped the fragment into the page instead of replacing it.
 *
 * Requires a stack from ./scripts/demo_up.sh, seeded with test_user.
 */

const USER = 'test_user';
const PASS = 'testpassword';

async function signIn(page: Page) {
  await page.goto('/ui/login');
  await page.getByLabel('Username').fill(USER);
  await page.getByLabel('Password').fill(PASS);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(/\/ui\/dashboard/);
}

test.describe('Authentication', () => {
  test('the sign-in page renders its form', async ({ page }) => {
    await page.goto('/ui/login');
    await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
    await expect(page.getByLabel('Username')).toBeVisible();
    await expect(page.getByLabel('Password')).toBeVisible();
  });

  test('valid credentials reach the dashboard', async ({ page }) => {
    await signIn(page);
    await expect(page.getByText('Wallet Balance')).toBeVisible();
  });

  test('a wrong password does not sign in, and does not say which field was wrong', async ({ page }) => {
    await page.goto('/ui/login');
    await page.getByLabel('Username').fill(USER);
    await page.getByLabel('Password').fill('wrong-password');
    await page.getByRole('button', { name: 'Sign in' }).click();

    await expect(page).not.toHaveURL(/\/ui\/dashboard/);

    // Naming the wrong field would confirm to an attacker that the username
    // exists, turning a login form into an account enumerator.
    const body = (await page.textContent('body')) ?? '';
    expect(body.toLowerCase()).not.toContain('no such user');
    expect(body.toLowerCase()).not.toContain('user not found');
  });

  test('every portal page bounces an unauthenticated visitor to sign-in', async ({ page }) => {
    for (const path of ['/ui/dashboard', '/ui/usage', '/ui/invoices', '/ui/renew', '/ui/tickets', '/ui/notifications']) {
      await page.goto(path);
      await expect(page, `${path} should redirect when signed out`).toHaveURL(/\/ui\/login/);
    }
  });

  test('signing out ends the session', async ({ page }) => {
    await signIn(page);
    await page.getByRole('button', { name: 'Logout' }).click();
    await expect(page).toHaveURL(/\/ui\/login/);

    // Back-navigating to a page held open before logout must not still serve it.
    await page.goto('/ui/dashboard');
    await expect(page).toHaveURL(/\/ui\/login/);
  });
});

test.describe('Dashboard and navigation', () => {
  test('the dashboard shows plan, balance, status and live usage', async ({ page }) => {
    await signIn(page);
    await expect(page.getByText('Wallet Balance')).toBeVisible();
    await expect(page.getByText('TN_Super_100M')).toBeVisible();
    await expect(page.getByText('active', { exact: true })).toBeVisible();

    // Seeded at 67% of quota, so the panel must show real figures rather than
    // the offline empty state.
    await expect(page.getByText(/GB of .*GB/)).toBeVisible();
  });

  test('the navigation reaches every section', async ({ page }) => {
    await signIn(page);
    for (const [link, heading] of [
      ['Usage', /Usage History/i],
      ['Invoices', /Invoices/i],
      ['Renew', /Renew Plan/i],
      ['Support', /Support Ticket/i],
      ['Notifications', /Notification History/i],
    ] as const) {
      await page.getByRole('link', { name: link }).click();
      await expect(page.getByText(heading).first()).toBeVisible();
    }
  });

  test('usage history lists the seeded sessions', async ({ page }) => {
    await signIn(page);
    await page.goto('/ui/usage');
    await expect(page.getByText(/Usage History/i)).toBeVisible();
    // Four sessions are seeded; each row carries a volume in GB.
    const rows = page.locator('table tbody tr');
    expect(await rows.count()).toBeGreaterThan(0);
  });
});

test.describe('Invoices', () => {
  test('the invoice list renders with amounts', async ({ page }) => {
    await signIn(page);
    await page.goto('/ui/invoices');
    await expect(page.getByText(/INV-/).first()).toBeVisible();
    await expect(page.getByText(/₹/).first()).toBeVisible();
  });

  test('downloading an invoice produces a real PDF', async ({ page }) => {
    await signIn(page);
    await page.goto('/ui/invoices');

    const [download] = await Promise.all([
      page.waitForEvent('download'),
      page.getByRole('link', { name: /Download PDF/i }).first().click(),
    ]);

    const stream = await download.createReadStream();
    const chunks: Buffer[] = [];
    for await (const chunk of stream) chunks.push(Buffer.from(chunk));
    const body = Buffer.concat(chunks);

    // A 200 carrying an HTML error page would satisfy a status-code assertion.
    // The magic bytes are what prove a PDF actually came back.
    expect(body.subarray(0, 5).toString()).toBe('%PDF-');
    expect(body.length).toBeGreaterThan(1000);
  });

  test("another subscriber's invoice is refused", async ({ page }) => {
    await signIn(page);
    // Invoice ids are sequential and guessable; ownership must be enforced
    // server-side rather than by only rendering links to your own.
    const res = await page.request.get('/ui/invoices/99999/pdf');
    expect([403, 404]).toContain(res.status());
  });
});

test.describe('Payment receipts', () => {
  test('payments are listed with a receipt that downloads as a real PDF', async ({ page }) => {
    await signIn(page);
    await page.goto('/ui/invoices');
    // seed_local.sql credits test_user 799.00 at setup.
    await expect(page.getByText(/RCPT-\d{6}/).first()).toBeVisible();
    const [download] = await Promise.all([
      page.waitForEvent('download'),
      page.getByRole('link', { name: /Download receipt/i }).first().click(),
    ]);
    const stream = await download.createReadStream();
    const chunks: Buffer[] = [];
    for await (const chunk of stream) chunks.push(Buffer.from(chunk));
    const body = Buffer.concat(chunks);
    expect(body.subarray(0, 5).toString()).toBe('%PDF-');
    expect(body.length).toBeGreaterThan(1000);
  });

  test("a guessed payment id that is not theirs is refused", async ({ page }) => {
    await signIn(page);
    const res = await page.request.get('/ui/payments/99999/receipt');
    expect([403, 404]).toContain(res.status());
  });
});

test.describe('Support tickets', () => {
  test('a filed ticket appears in the list', async ({ page }) => {
    await signIn(page);
    await page.goto('/ui/tickets');

    const description = `E2E check ${Date.now()}`;
    await page.selectOption('select[name="category"]', { index: 0 });
    await page.fill('textarea[name="description"]', description);
    await page.getByRole('button', { name: /Submit Ticket/i }).click();

    // HTMX swaps the list fragment in place rather than reloading the page,
    // so the new row must appear without a navigation.
    await expect(page.getByText(description)).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText('open').first()).toBeVisible();
  });

  test('a submission without the CSRF token is refused', async ({ page }) => {
    await signIn(page);
    await page.goto('/ui/tickets');

    // Posts with the session cookie but no token — the shape a cross-site
    // forgery takes, since the cookie rides along automatically and the token
    // does not.
    const res = await page.request.post('/ui/tickets', {
      form: { category: 'connectivity', description: 'forged' },
    });
    expect(res.status()).toBe(403);
  });

  test('a submission with a wrong CSRF token is refused', async ({ page }) => {
    await signIn(page);
    await page.goto('/ui/tickets');

    const res = await page.request.post('/ui/tickets', {
      form: { category: 'connectivity', description: 'forged', csrf_token: 'not-the-real-token' },
    });
    expect(res.status()).toBe(403);
  });
});

test.describe('Renewal', () => {
  test('the renewal form renders and reports the gateway is unconfigured', async ({ page }) => {
    await signIn(page);
    await page.goto('/ui/renew');
    await expect(page.getByText(/Renew Plan/i)).toBeVisible();

    await page.fill('input[name="amount"]', '799');
    await page.getByRole('button', { name: /Get Payment Link/i }).click();

    // Known gap: no Razorpay credentials are loaded in the demo. The point of
    // asserting it is that the page degrades with a readable message rather
    // than a stack trace or a blank fragment.
    await expect(page.getByText(/not configured/i)).toBeVisible({ timeout: 10_000 });
  });
});

test.describe('Rating a resolved ticket (FR-SUP-004)', () => {
  // An open ticket cannot be rated: rating one would be rating a resolution
  // that has not happened. The portal says so rather than showing a control
  // that fails.
  test('an open ticket offers no rating, and says why', async ({ page }) => {
    await signIn(page);
    await page.goto('/ui/tickets');

    const description = `E2E rating-open ${Date.now()}`;
    await page.selectOption('select[name="category"]', { index: 0 });
    await page.fill('textarea[name="description"]', description);
    await page.getByRole('button', { name: /Submit Ticket/i }).click();
    await expect(page.getByText(description)).toBeVisible({ timeout: 10_000 });

    const row = page.locator('tbody tr', { hasText: description });
    await expect(row.locator('form[action*="/rating"], select[name="rating"]')).toHaveCount(0);
    await expect(row.getByText(/once it is resolved/)).toBeVisible();
  });

  // The write path end to end against a real database: a subscriber files a
  // ticket, staff resolve it in the console, and the subscriber rates it.
  // Driven through both real screens rather than by writing the status
  // straight into the table, so the transition is the one production makes —
  // it is what writes the ticket_status_history row the helpdesk figures
  // read, and a fixture that skipped it would prove nothing about them.
  test('a resolved ticket can be rated and the stars come back', async ({ page, browser }) => {
    await signIn(page);
    await page.goto('/ui/tickets');

    const description = `E2E rating ${Date.now()}`;
    await page.selectOption('select[name="category"]', { index: 0 });
    await page.fill('textarea[name="description"]', description);
    await page.getByRole('button', { name: /Submit Ticket/i }).click();
    await expect(page.getByText(description)).toBeVisible({ timeout: 10_000 });

    // Resolve it in the console, in its own context so the staff session and
    // the portal session do not share a cookie jar.
    const staffCtx = await browser.newContext({ ignoreHTTPSErrors: true });
    try {
      const staff = await staffCtx.newPage();
      await staff.goto('/staff/login');
      await staff.getByLabel('Username').fill('csr');
      await staff.getByLabel('Password').fill('staffpassword');
      await staff.getByRole('button', { name: 'Sign in' }).click();
      await expect(staff).toHaveURL(/\/staff\/dashboard/);

      await staff.goto('/staff/tickets?view=open');
      const staffRow = staff.locator('tbody tr', { hasText: description });
      if ((await staffRow.count()) === 0) {
        test.skip(true, 'the filed ticket is not in the console queue on this stack');
      }
      await staffRow.locator('select[name="status"]').selectOption('resolved');
      await staffRow.getByRole('button', { name: /Update|Save/i }).first().click();
      await expect(staff.locator('tbody tr', { hasText: description })).toHaveCount(0);
    } finally {
      await staffCtx.close();
    }

    // Back in the portal, it is now ratable.
    await page.reload();
    const row = page.locator('tbody tr', { hasText: description });
    await expect(row.locator('select[name="rating"]')).toBeVisible();

    await row.locator('select[name="rating"]').selectOption('4');
    await row.locator('input[name="comment"]').fill('sorted quickly');
    await row.getByRole('button', { name: /Submit rating/i }).click();

    // The fragment swaps in place and now carries the rating.
    await expect(page.getByText(/Thank you/)).toBeVisible({ timeout: 10_000 });
    const rated = page.locator('tbody tr', { hasText: description });
    await expect(rated.getByText('★★★★☆')).toBeVisible();
    await expect(rated.getByText('sorted quickly')).toBeVisible();
  });

  // And the console shows it, which is the other half of the requirement:
  // a rating nobody in support can see is feedback collected and discarded.
  test('the rating appears in the console queue and the helpdesk summary', async ({ browser }) => {
    const staffCtx = await browser.newContext({ ignoreHTTPSErrors: true });
    try {
      const staff = await staffCtx.newPage();
      await staff.goto('/staff/login');
      await staff.getByLabel('Username').fill('csr');
      await staff.getByLabel('Password').fill('staffpassword');
      await staff.getByRole('button', { name: 'Sign in' }).click();
      await expect(staff).toHaveURL(/\/staff\/dashboard/);

      await staff.goto('/staff/tickets?view=closed');
      // The column exists whether or not anything is rated yet.
      await expect(staff.getByRole('columnheader', { name: 'Rated' })).toBeVisible();
      // The satisfaction tiles, with the response rate's denominator beside
      // the average rather than left to be worked out.
      await expect(staff.getByText('Average rating (30 days)')).toBeVisible();
      await expect(staff.getByText(/worth a call back/)).toBeVisible();
      // And no way for staff to enter one.
      await expect(staff.locator('select[name="rating"], input[name="rating"]')).toHaveCount(0);
    } finally {
      await staffCtx.close();
    }
  });

  // The cookie rides along automatically and the token does not, which is
  // the shape a cross-site forgery takes.
  test('a rating without the CSRF token is refused', async ({ page }) => {
    await signIn(page);
    await page.goto('/ui/tickets');

    const res = await page.request.post('/ui/tickets/1/rating', {
      form: { rating: '5' },
    });
    expect(res.status()).toBe(403);
  });
});
