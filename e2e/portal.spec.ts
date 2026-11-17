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
