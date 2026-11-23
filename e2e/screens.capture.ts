// Captures every screen as a PNG, at desktop and at phone width, and writes
// an HTML index over them.
//
// Not part of the test suite — it asserts almost nothing and is run through
// playwright.capture.config.ts on purpose. It exists because "the suite is
// green" and "the UI looks right" are different claims, and the suite can
// only make the first one: `screenshot: 'only-on-failure'` means a fully
// passing run produces no images at all, so there is nothing to show anybody
// precisely when everything works.
//
// The phone pass is the one worth looking at. Until 2026-11-23 every portal
// page was 610px wide in a 390px viewport and 171 passing tests said nothing
// about it, because they all ran at the default desktop size.
//
//   PORTAL_URL=https://127.0.0.1:18443 \
//     npx playwright test --config=playwright.capture.config.ts
import { test, expect } from '@playwright/test';
import fs from 'fs';
import path from 'path';

const OUT = path.join('e2e', 'screenshots');
const SRS = path.join('specification_docs_v2', '02_SRS_System_Requirements.md');

// Which console screen demonstrates each requirement area.
//
// By area rather than per requirement, deliberately. A per-FR mapping would
// have to be maintained by hand against 143 rows and would be quietly wrong
// within a month; the area is stable and checkable. Areas with no screen are
// marked so, because a screenshot cannot show RADIUS answering on :1812.
const areaScreens: Record<string, string[]> = {
  CON: ['dashboard', 'subscribers', 'sessions', 'leads'],
  SUB: ['subscribers', 'subscriber-360'],
  BIL: ['billing', 'invoices', 'billing-runs', 'diagnostics', 'dunning', 'products', 'collections'],
  CAT: ['catalogue', 'bandwidth', 'fair-use'],
  SUP: ['tickets'],
  CRM: ['leads'],
  ANN: ['announcements'],
  GRP: ['groups'],
  RPT: ['reports'],
  REV: ['revenue'],
  FRN: ['franchise'],
  LC: ['franchise'],
  INV: ['inventory', 'procurement', 'vendors'],
  CPE: ['cpe'],
  NAS: ['nas', 'plan-attributes'],
  NET: ['network', 'ip-pools'],
  HSP: ['portal-branding'],
  DOC: ['documents'],
  OBS: ['audit', 'logs', 'auth-log'],
  COL: ['collections'],
  SEC: ['accounts', 'lea'],
  MOB: ['portal-dashboard'],
  PAY: ['gateway-log'],
  WFL: ['tasks'],
  // No console screen: these are daemons, protocols and background work.
  AAA: [],
  FUP: [],
  NOTIF: [],
  API: [],
};

type Requirement = { id: string; title: string; area: string };

function readRequirements(): Requirement[] {
  if (!fs.existsSync(SRS)) return [];
  const out: Requirement[] = [];
  const seen = new Set<string>();
  for (const line of fs.readFileSync(SRS, 'utf8').split(/\r?\n/)) {
    // Requirement rows only. The leading pipe keeps NFR rows out, and \b
    // before FR is what stops FR-AVAIL-001 matching inside NFR-AVAIL-001.
    const m = /^\|\s*(FR-([A-Z]+)-\d{3})\s*\|\s*(.+?)\s*\|/.exec(line);
    if (!m || seen.has(m[1])) continue;
    seen.add(m[1]);
    out.push({ id: m[1], area: m[2], title: m[3].replace(/[*`~]/g, '').trim() });
  }
  return out;
}
const DESKTOP = { width: 1440, height: 900 };
const PHONE = { width: 390, height: 844 };

// Every section an isp_owner can open, in nav order.
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
  'dashboard', 'usage', 'invoices', 'tickets', 'notifications', 'renew',
];

type Shot = { file: string; title: string; group: string; width: number };
const shots: Shot[] = [];

function dir(...parts: string[]) {
  const p = path.join(OUT, ...parts);
  fs.mkdirSync(p, { recursive: true });
  return p;
}

async function capture(page, group: string, name: string, width: number) {
  const folder = width === PHONE.width ? 'phone' : 'desktop';
  const file = path.join(folder, `${group}-${name}.png`);
  // fullPage so a long list is captured in full rather than cut at the fold —
  // the point is to see the screen, not the viewport.
  await page.screenshot({ path: path.join(OUT, file), fullPage: true });
  shots.push({ file: file.split(path.sep).join('/'), title: name, group, width });
}

async function signInToConsole(page) {
  await page.goto('/staff/login');
  await page.fill('input[name="username"]', 'owner');
  await page.fill('input[name="password"]', 'staffpassword');
  await page.click('button[type="submit"]');
  await page.waitForURL(/\/staff\/dashboard/);
}

async function signInToPortal(page) {
  await page.goto('/ui/login');
  await page.fill('input[name="username"]', 'test_user');
  await page.fill('input[name="password"]', 'testpassword');
  await page.click('button[type="submit"]');
  await page.waitForURL(/\/ui\/dashboard/);
}

test.beforeAll(() => {
  fs.rmSync(OUT, { recursive: true, force: true });
  dir('desktop');
  dir('phone');
});

test('console at desktop width', async ({ page }) => {
  await page.setViewportSize(DESKTOP);
  await signInToConsole(page);
  for (const section of consoleSections) {
    await page.goto(`/staff/${section}`);
    await capture(page, 'console', section, DESKTOP.width);
  }
  // The 360 view, which is the screen a CSR lives in.
  await page.goto('/staff/subscribers/1');
  await capture(page, 'console', 'subscriber-360', DESKTOP.width);
});

test('console at phone width', async ({ page }) => {
  await page.setViewportSize(PHONE);
  await signInToConsole(page);
  for (const section of ['dashboard', 'subscribers', 'invoices', 'tickets', 'collections']) {
    await page.goto(`/staff/${section}`);
    await capture(page, 'console', section, PHONE.width);
  }
});

test('subscriber portal at desktop and phone width', async ({ page }) => {
  for (const size of [DESKTOP, PHONE]) {
    await page.setViewportSize(size);
    await page.context().clearCookies();
    await page.goto('/ui/login');
    await capture(page, 'portal', 'login', size.width);
    await signInToPortal(page);
    for (const p of portalPages) {
      await page.goto(`/ui/${p}`);
      await capture(page, 'portal', p, size.width);
    }
  }
});

test('captive portal', async ({ page }) => {
  for (const size of [DESKTOP, PHONE]) {
    await page.setViewportSize(size);
    await page.goto('/hotspot/');
    await capture(page, 'hotspot', 'landing', size.width);
  }
});

// The index is written last, from whatever was captured, so a partial run
// still produces something readable.
test.afterAll(() => {
  const desktop = shots.filter((s) => s.width === DESKTOP.width);
  const phone = shots.filter((s) => s.width === PHONE.width);

  const section = (title: string, list: Shot[], note: string) => `
    <h2>${title} <span class="count">${list.length}</span></h2>
    <p class="note">${note}</p>
    <div class="grid">
      ${list
        .map(
          (s) => `<figure>
        <a href="${s.file}"><img src="${s.file}" alt="${s.group} ${s.title}" loading="lazy"></a>
        <figcaption>${s.group} / ${s.title}</figcaption>
      </figure>`,
        )
        .join('\n')}
    </div>`;

  // Requirement coverage, read from the SRS rather than kept in a second
  // list here. A requirement whose area has no screen says so: that is not a
  // gap, it is a daemon or a protocol, and the Go suite is what covers it.
  const captured = new Set(shots.map((s) => `${s.group}-${s.title}`));
  const reqs = readRequirements();
  const covRows = reqs
    .map((r) => {
      const screens = areaScreens[r.area];
      let status: string;
      let cls: string;
      if (screens === undefined) {
        status = 'area not mapped';
        cls = 'unknown';
      } else if (screens.length === 0) {
        status = 'no screen — daemon or protocol, covered by the Go suite';
        cls = 'noui';
      } else {
        const shown = screens.filter(
          (s) => captured.has(`console-${s}`) || captured.has(`portal-${s.replace('portal-', '')}`),
        );
        status = shown.length
          ? shown.map((s) => `<a href="desktop/console-${s}.png">${s}</a>`).join(', ')
          : screens.join(', ') + ' (not captured in this run)';
        cls = shown.length ? 'ok' : 'miss';
      }
      return `<tr class="${cls}"><td><code>${r.id}</code></td><td>${r.title.slice(0, 120)}</td><td>${status}</td></tr>`;
    })
    .join('\n');

  const noUI = reqs.filter((r) => (areaScreens[r.area] ?? []).length === 0).length;

  const html = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ISP BSS — screens</title>
<style>
  :root { --bg:#f5f6f8; --surface:#fff; --ink:#17212b; --muted:#616e7b; --rule:#d6dde4; }
  body { font:15px/1.55 system-ui,sans-serif; margin:0; padding:32px; background:var(--bg); color:var(--ink); }
  h1 { margin:0 0 4px; font-size:1.5rem; }
  h2 { margin:40px 0 4px; font-size:1.1rem; }
  .count { color:var(--muted); font-weight:400; font-size:.9rem; }
  .note, .meta { color:var(--muted); font-size:.9rem; margin:0 0 16px; }
  .grid { display:grid; grid-template-columns:repeat(auto-fill,minmax(300px,1fr)); gap:18px; }
  figure { margin:0; background:var(--surface); border:1px solid var(--rule); border-radius:8px; overflow:hidden; }
  img { display:block; width:100%; height:auto; border-bottom:1px solid var(--rule); }
  figcaption { padding:8px 10px; font-size:.85rem; color:var(--muted); }
  a { color:inherit; }
  table { border-collapse:collapse; width:100%; background:var(--surface); font-size:.88rem; }
  th, td { text-align:left; padding:6px 10px; border-bottom:1px solid var(--rule); vertical-align:top; }
  th { background:#eef1f4; position:sticky; top:0; }
  tr.noui td { color:var(--muted); }
  tr.miss td, tr.unknown td { background:#fdf1da; }
  code { font-size:.85rem; }
</style></head><body>
<h1>ISP BSS — every screen</h1>
<p class="meta">Captured ${new Date().toISOString().slice(0, 16).replace('T', ' ')} ·
  desktop 1440&times;900, phone 390&times;844 (iPhone 12) · full-page</p>
${section('Desktop', desktop, 'Console sections, the subscriber 360 view, the portal and the captive portal.')}
${section('Phone', phone, 'The portal is the surface most customers use. Every page here was 610px wide in a 390px viewport until 2026-11-23 — the navigation did not wrap.')}

<h2>Requirement coverage <span class="count">${reqs.length} functional requirements</span></h2>
<p class="note">Read from the SRS at capture time, not kept in a second list.
  ${noUI} of them have no console screen at all — RADIUS, FUP enforcement,
  notification fan-out and the partner API are daemons and protocols, and a
  screenshot cannot show one answering on :1812. Those are what the Go suite
  is for; this table says which is which rather than implying a gap.</p>
<table>
  <thead><tr><th>FR</th><th>Requirement</th><th>Shown on</th></tr></thead>
  <tbody>${covRows}</tbody>
</table>
</body></html>`;

  fs.writeFileSync(path.join(OUT, 'index.html'), html);
  expect(shots.length).toBeGreaterThan(0);
  console.log(`\n${shots.length} screens captured → ${path.join(OUT, 'index.html')}`);
});
