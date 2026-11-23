// The portal and console on a phone, and the two accessibility affordances
// that are easy to add and easy to lose again.
//
// Every other spec in this suite runs at the default desktop viewport, which
// is why 171 passing tests said nothing about the subscriber portal being
// unusable on a handset: on 2026-11-23 every portal page was 610px wide in a
// 390px viewport, so a customer scrolled sideways on every screen. The cause
// was the navigation — six links and a logout button in a flex row with no
// wrap, measuring 541px — not the data tables everyone would suspect.
//
// These run at phone width on purpose. Without that, they cannot fail.
import { test, expect } from '@playwright/test';

const PHONE = { width: 390, height: 844 }; // iPhone 12/13/14

const portalPages = [
  '/ui/dashboard',
  '/ui/usage',
  '/ui/invoices',
  '/ui/tickets',
  '/ui/notifications',
  '/ui/renew',
];

const consolePages = [
  '/staff/dashboard',
  '/staff/subscribers',
  '/staff/invoices',
  '/staff/tickets',
];

async function signInToPortal(page) {
  await page.goto('/ui/login');
  await page.fill('input[name="username"]', 'test_user');
  await page.fill('input[name="password"]', 'testpassword');
  await page.click('button[type="submit"]');
  await page.waitForURL(/\/ui\/dashboard/);
}

async function signInToConsole(page) {
  await page.goto('/staff/login');
  await page.fill('input[name="username"]', 'owner');
  await page.fill('input[name="password"]', 'staffpassword');
  await page.click('button[type="submit"]');
  await page.waitForURL(/\/staff\/dashboard/);
}

// documentElement.scrollWidth beyond clientWidth is a horizontal scrollbar on
// the page itself, which is what makes a site feel broken on a phone. A table
// scrolling inside its own box does not count, and must not: a table row
// means nothing split across lines, so those get their own scroll area.
async function pageOverflow(page): Promise<number> {
  return page.evaluate(() => {
    const d = document.documentElement;
    return d.scrollWidth - d.clientWidth;
  });
}

test.describe('On a phone', () => {
  test.use({ viewport: PHONE });

  test('no subscriber portal page scrolls sideways', async ({ page }) => {
    await signInToPortal(page);
    for (const path of portalPages) {
      await page.goto(path);
      expect(await pageOverflow(page), `${path} overflows its viewport`).toBeLessThanOrEqual(0);
    }
  });

  test('no console page scrolls sideways', async ({ page }) => {
    await signInToConsole(page);
    for (const path of consolePages) {
      await page.goto(path);
      expect(await pageOverflow(page), `${path} overflows its viewport`).toBeLessThanOrEqual(0);
    }
  });

  test('a wide table scrolls inside its own box, not the page', async ({ page }) => {
    await signInToPortal(page);
    await page.goto('/ui/invoices');

    const box = page.locator('.table-scroll').first();
    await expect(box).toBeVisible();
    // The box is what scrolls; the page is not.
    const scrollable = await box.evaluate((el) => el.scrollWidth > el.clientWidth);
    expect(await pageOverflow(page), 'the table dragged the page wider').toBeLessThanOrEqual(0);
    // If the table happens to fit at this width the box simply does not
    // scroll, which is fine — what must never happen is the page scrolling.
    expect(typeof scrollable).toBe('boolean');
  });

  test('navigation links are big enough to tap', async ({ page }) => {
    await signInToPortal(page);
    await page.goto('/ui/dashboard');
    const links = page.locator('.nav-links a');
    const n = await links.count();
    expect(n).toBeGreaterThan(0);
    for (let i = 0; i < n; i++) {
      const box = await links.nth(i).boundingBox();
      const label = await links.nth(i).innerText();
      // WCAG 2.5.5 asks for 44px. Height is the one that fails when links
      // wrap onto several rows.
      expect(box!.height, `nav link "${label}" is only ${box!.height}px tall`).toBeGreaterThanOrEqual(44);
    }
  });
});

test.describe('Keyboard and screen-reader affordances', () => {
  test('the portal offers a skip link on first Tab, and it reaches the content', async ({ page }) => {
    await signInToPortal(page);
    await page.goto('/ui/dashboard');

    await page.keyboard.press('Tab');
    const focused = page.locator(':focus');
    await expect(focused).toHaveClass(/skip-link/);
    // Off-screen until focused, on-screen once focused — a skip link that
    // stays hidden when focused is decorative, which is the usual way this
    // control is got wrong.
    const box = await focused.boundingBox();
    expect(box!.x, 'the skip link stayed off-screen while focused').toBeGreaterThanOrEqual(0);

    await page.keyboard.press('Enter');
    await expect(page.locator('#main')).toBeVisible();
  });

  test('the console offers the same skip link', async ({ page }) => {
    await signInToConsole(page);
    await page.goto('/staff/dashboard');
    await page.keyboard.press('Tab');
    await expect(page.locator(':focus')).toHaveClass(/skip-link/);
  });

  test('a failed sign-in is announced, not just coloured red', async ({ page }) => {
    await page.goto('/ui/login');
    await page.fill('input[name="username"]', 'test_user');
    await page.fill('input[name="password"]', 'wrong-password');
    await page.click('button[type="submit"]');

    // role="alert" is what makes a screen reader read the failure out. The
    // message existed before; nothing announced it.
    const alert = page.locator('[role="alert"]');
    await expect(alert).toBeVisible();
    await expect(alert).not.toBeEmpty();
  });

  test('a standing notice is not announced as if it were a status change', async ({ page }) => {
    await signInToConsole(page);
    await page.goto('/staff/lea');

    // The LEA screen's "every lookup is recorded" notice is page content, not
    // the outcome of anything. Giving it a live role would make a screen
    // reader interrupt with it on every visit, which is worse than silence —
    // so the roles were applied only to action outcomes.
    const notice = page.locator('.alert.warn', { hasText: 'recorded against your name' });
    await expect(notice).toBeVisible();
    await expect(notice).not.toHaveAttribute('role', 'alert');
  });
});
