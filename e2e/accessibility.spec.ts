// WCAG 2.1 AA, checked by axe-core on every console section and every portal
// page.
//
// What this does and does not prove. axe-core catches roughly a third to a
// half of WCAG failures — the mechanical ones: contrast, missing names,
// broken ARIA, unlabelled controls, bad landmark structure. It cannot judge
// whether focus order makes sense, whether link text means anything out of
// context, or whether an error message tells somebody how to fix the
// problem. A clean run here is a floor, not a certificate, and the
// hand-written checks in mobile-accessibility.spec.ts cover the parts of
// 2.4.1 and 4.1.3 that axe cannot see.
//
// Added 2026-11-23, when the portal and console were brought to AA. The sweep
// that day found five colour tokens below 4.5:1 and one tile label at 4.46:1
// that only showed up because the active dashboard tile paints muted text on
// its own lighter background — which is exactly the kind of pairing a human
// reading the stylesheet does not think to check.
import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';

const AA_TAGS = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'];

// Every section in staffui.sections that an isp_owner can open.
const consoleSections = [
  'dashboard', 'subscribers', 'leads', 'billing', 'tickets', 'invoices',
  'revenue', 'catalogue', 'products', 'bandwidth', 'groups', 'fair-use',
  'sessions', 'cpe', 'nas', 'ip-pools', 'auth-log', 'plan-attributes',
  'network', 'lea', 'demo', 'accounts', 'documents', 'audit', 'franchise',
  'procurement', 'vendors', 'portal-branding', 'ledger', 'dunning',
  'diagnostics', 'billing-runs', 'gateway-log', 'collections', 'inventory',
  'reports', 'announcements', 'logs', 'tasks',
];

const portalPages = [
  '/ui/dashboard', '/ui/usage', '/ui/invoices',
  '/ui/tickets', '/ui/notifications', '/ui/renew',
];

// Reported as one list rather than one failure per page: a contrast change
// usually breaks every screen at once, and forty near-identical failures bury
// the one that is different.
function describe(violations: Awaited<ReturnType<AxeBuilder['analyze']>>['violations'], where: string): string {
  return violations
    .map((v) => {
      const nodes = v.nodes.map((n) => `        ${n.target.join(' ')}\n          ${(n.failureSummary ?? '').split('\n').slice(1).join(' ').trim()}`).join('\n');
      return `  [${v.impact}] ${v.id} on ${where}: ${v.help}\n${nodes}`;
    })
    .join('\n');
}

test.describe('WCAG 2.1 AA', () => {
  test('every console section', async ({ page }) => {
    // Forty screens, each loaded and then scanned by axe in-page. It runs in
    // about 25s alone and over the 30s default when the whole suite is
    // competing for the machine, which showed up as a timeout rather than as
    // a violation — a failure that reads like an accessibility regression and
    // is not one.
    test.setTimeout(180_000);
    await page.goto('/staff/login');
    await page.fill('input[name="username"]', 'owner');
    await page.fill('input[name="password"]', 'staffpassword');
    await page.click('button[type="submit"]');
    await page.waitForURL(/\/staff\/dashboard/);

    const failures: string[] = [];
    for (const section of consoleSections) {
      await page.goto(`/staff/${section}`);
      const r = await new AxeBuilder({ page }).withTags(AA_TAGS).analyze();
      if (r.violations.length) failures.push(describe(r.violations, `/staff/${section}`));
    }

    // The 360 view is the screen a CSR lives in, and the only one with a
    // JavaScript tab widget, so it is checked by name rather than left out
    // because it is not in the section list.
    await page.goto('/staff/subscribers/1');
    const detail = await new AxeBuilder({ page }).withTags(AA_TAGS).analyze();
    if (detail.violations.length) failures.push(describe(detail.violations, '/staff/subscribers/1'));
    expect(failures.join('\n') || 'clean').toBe('clean');
  });

  test('the subscriber portal, signed in and signed out', async ({ page }) => {
    const failures: string[] = [];

    await page.goto('/ui/login');
    let r = await new AxeBuilder({ page }).withTags(AA_TAGS).analyze();
    if (r.violations.length) failures.push(describe(r.violations, '/ui/login'));

    await page.fill('input[name="username"]', 'test_user');
    await page.fill('input[name="password"]', 'testpassword');
    await page.click('button[type="submit"]');
    await page.waitForURL(/\/ui\/dashboard/);

    for (const path of portalPages) {
      await page.goto(path);
      r = await new AxeBuilder({ page }).withTags(AA_TAGS).analyze();
      if (r.violations.length) failures.push(describe(r.violations, path));
    }
    expect(failures.join('\n') || 'clean').toBe('clean');
  });

  test('the portal at phone width, where the layout differs', async ({ page }) => {
    // The phone breakpoint restyles the nav and puts tables in scroll boxes,
    // so it is a different rendering and needs its own sweep — a desktop-only
    // audit would not see a contrast or naming problem introduced there.
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto('/ui/login');
    await page.fill('input[name="username"]', 'test_user');
    await page.fill('input[name="password"]', 'testpassword');
    await page.click('button[type="submit"]');
    await page.waitForURL(/\/ui\/dashboard/);

    const failures: string[] = [];
    for (const path of portalPages) {
      await page.goto(path);
      const r = await new AxeBuilder({ page }).withTags(AA_TAGS).analyze();
      if (r.violations.length) failures.push(describe(r.violations, `${path} @390px`));
    }
    expect(failures.join('\n') || 'clean').toBe('clean');
  });
});
