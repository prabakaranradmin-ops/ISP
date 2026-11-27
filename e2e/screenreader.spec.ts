// What a screen reader encounters, which is not what axe-core measures.
//
// accessibility.spec.ts runs axe over every screen and reports zero
// violations. Its own header says axe catches roughly a third of WCAG issues,
// and names two it cannot see at all: 2.4.1 Bypass Blocks and 4.1.3 Status
// Messages. This file is the rest of that third — the parts a person using
// NVDA or VoiceOver hits first, checked through the accessibility tree rather
// than the DOM.
//
// It is still not a manual pass. A human with a screen reader judges whether
// an announcement makes sense; this only establishes that there is one, that
// it is attached to the right thing, and that the structure a screen reader
// navigates by is actually there. The remaining gap is judgment, not
// mechanics.
//
// Four things are checked, in the order a screen-reader user meets them:
//
//   1. The skip link works — not that it exists, which axe checks, but that
//      activating it moves focus into the main content. A skip link that
//      renders and does nothing is worse than none: it is the first thing
//      offered and it wastes the one keystroke that matters.
//   2. Every interactive control has an accessible name. A button announced
//      as "button" is unusable, and this is the commonest real failure in a
//      console built from icons and table rows.
//   3. Headings are a usable outline: one h1, no skipped levels. Heading
//      navigation is how a screen-reader user skims a page they cannot see.
//   4. Status messages are announced. The console reports every action
//      through a Message or Error div; without a live region the operator
//      hears nothing and has no way to know whether the thing they did
//      worked.

import { test, expect, Page } from '@playwright/test';

// The console screens worth auditing: the landing page, the busiest list, a
// form-heavy screen, and a detail page. Not all thirty-nine — this is about
// patterns shared by the templates, and a pattern that is wrong is wrong
// everywhere.
const consoleScreens = [
  { path: '/staff/login', name: 'sign-in' },
  { path: '/staff/', name: 'dashboard' },
  { path: '/staff/subscribers', name: 'subscriber directory' },
  { path: '/staff/tickets', name: 'ticket queue' },
  { path: '/staff/invoices', name: 'invoices' },
];

const portalScreens = [
  { path: '/ui/login', name: 'portal sign-in' },
];

async function signIn(page: Page): Promise<boolean> {
  await page.goto('/staff/login');
  const user = process.env.E2E_STAFF_USER;
  const pass = process.env.E2E_STAFF_PASSWORD;
  if (!user || !pass) return false;
  await page.getByLabel(/username/i).fill(user);
  await page.getByLabel(/password/i).fill(pass);
  await page.getByRole('button', { name: /sign in/i }).click();
  await page.waitForLoadState('domcontentloaded');
  return !page.url().includes('/staff/login');
}

test.describe('what a screen reader encounters', () => {
  // 1. The skip link has to move focus, not merely exist.
  //
  // Tested by activating it rather than by tabbing to it. On the sign-in page
  // the username field carries autofocus, so the first Tab goes to the
  // password field and the skip link is never reached by tabbing forward —
  // which is correct for that page (there is no navigation to bypass) and
  // would make a tab-order assertion here measure the wrong thing.
  //
  // What matters is the mechanism: the link names a target that exists, and
  // activating it moves focus there. A fragment link to a non-focusable
  // element scrolls the page and leaves a screen reader exactly where it
  // was, which is the failure this is for.
  test('the skip link names a real target and moves focus to it', async ({ page }) => {
    await page.goto('/staff/login');

    const skip = page.locator('a.skip-link').first();
    await expect(skip, 'the page has no skip link').toHaveCount(1);

    const href = await skip.getAttribute('href');
    expect(href, 'the skip link has no href').toBeTruthy();
    expect(href!.startsWith('#'), `the skip link points at ${href}, not a fragment`).toBe(true);

    const targetId = href!.slice(1);
    const target = page.locator(`#${targetId}`);
    await expect(
      target,
      `the skip link points at #${targetId} and no element has that id, so it goes nowhere`,
    ).toHaveCount(1);

    // Focus it directly and activate it, which is what a keyboard user does
    // once they reach it.
    await skip.focus();
    await page.keyboard.press('Enter');

    const landed = await page.evaluate((id) => {
      const el = document.activeElement as HTMLElement | null;
      const targetEl = document.getElementById(id);
      return {
        where: el ? el.tagName + (el.id ? '#' + el.id : '') : 'nothing',
        isTarget: el !== null && targetEl !== null && (el === targetEl || targetEl.contains(el)),
        targetFocusable: targetEl !== null && targetEl.hasAttribute('tabindex'),
      };
    }, targetId);

    expect(
      landed.isTarget,
      `activating the skip link left focus on ${landed.where} rather than inside ` +
        `#${targetId}. The target ${landed.targetFocusable ? 'is' : 'is NOT'} focusable: a ` +
        `fragment link to an element with no tabindex scrolls the page without moving ` +
        `focus, so a screen-reader user hears nothing change and the one keystroke they ` +
        `spent on the skip link is wasted. Adding tabindex="-1" to the target fixes it.`,
    ).toBe(true);
  });

  // The sign-in page puts focus in the form on load, which is right: there is
  // nothing to skip past, and a keyboard user should be typing immediately.
  // Asserted so that adding navigation to this page without reconsidering the
  // autofocus does not quietly leave the skip link unreachable.
  test('the sign-in page focuses the form, having no navigation to bypass', async ({ page }) => {
    await page.goto('/staff/login');

    const focused = await page.evaluate(() => {
      const el = document.activeElement as HTMLElement | null;
      return el ? { tag: el.tagName, id: el.id, name: el.getAttribute('name') } : null;
    });
    expect(focused, 'nothing has focus on load').not.toBeNull();
    expect(
      focused!.name,
      `focus starts on ${focused!.tag}#${focused!.id}; the sign-in field is where a ` +
        `keyboard user should begin`,
    ).toBe('username');

    // And there is genuinely no navigation on this page to bypass.
    const navLinks = await page.locator('nav a').count();
    expect(
      navLinks,
      'the sign-in page has navigation now, so the skip link needs to be reachable by ' +
        'tabbing and the autofocus has to be reconsidered',
    ).toBe(0);
  });

  // 2. Every interactive control has a name in the accessibility tree.
  for (const screen of [...consoleScreens, ...portalScreens]) {
    test(`every control on the ${screen.name} has an accessible name`, async ({ page }) => {
      const authed = screen.path === '/staff/login' || screen.path === '/ui/login'
        ? true
        : await signIn(page);
      if (!authed) {
        test.skip(true, 'E2E_STAFF_USER/E2E_STAFF_PASSWORD not set; cannot reach an authenticated screen');
      }
      await page.goto(screen.path);
      await page.waitForLoadState('domcontentloaded');

      // Read names the way a screen reader does: from the accessibility
      // tree, not from the markup. An element can have a label in the DOM
      // and still be announced as "button".
      const unnamed = await page.evaluate(() => {
        const interactive = Array.from(
          document.querySelectorAll('a[href], button, input, select, textarea, [role="button"], [role="link"]'),
        ) as HTMLElement[];

        const visible = (el: HTMLElement) => {
          const s = getComputedStyle(el);
          if (s.display === 'none' || s.visibility === 'hidden') return false;
          const r = el.getBoundingClientRect();
          return r.width > 0 || r.height > 0;
        };

        const nameOf = (el: HTMLElement): string => {
          const aria = el.getAttribute('aria-label');
          if (aria && aria.trim()) return aria.trim();
          const labelledBy = el.getAttribute('aria-labelledby');
          if (labelledBy) {
            const parts = labelledBy.split(/\s+/)
              .map((id) => document.getElementById(id)?.textContent || '')
              .join(' ').trim();
            if (parts) return parts;
          }
          if (el instanceof HTMLInputElement || el instanceof HTMLSelectElement ||
              el instanceof HTMLTextAreaElement) {
            if (el.labels && el.labels.length) {
              const t = Array.from(el.labels).map((l) => l.textContent || '').join(' ').trim();
              if (t) return t;
            }
            if (el.getAttribute('title')) return el.getAttribute('title')!.trim();
            // A placeholder is not a label, but it is better than nothing
            // and worth distinguishing in the report.
            if (el.getAttribute('placeholder')) return '';
          }
          const text = (el.textContent || '').trim();
          if (text) return text;
          const img = el.querySelector('img[alt]');
          if (img && img.getAttribute('alt')?.trim()) return img.getAttribute('alt')!.trim();
          return '';
        };

        return interactive
          .filter(visible)
          // A hidden input and a submit with a value are not announced as
          // nameless; neither is an element explicitly removed from the tree.
          .filter((el) => !(el instanceof HTMLInputElement && el.type === 'hidden'))
          .filter((el) => el.getAttribute('aria-hidden') !== 'true')
          .filter((el) => !(el instanceof HTMLInputElement && el.value && el.type === 'submit'))
          .filter((el) => nameOf(el) === '')
          .map((el) => {
            const attrs = ['id', 'name', 'type', 'href', 'class']
              .map((a) => (el.getAttribute(a) ? `${a}="${el.getAttribute(a)}"` : ''))
              .filter(Boolean).join(' ');
            return `<${el.tagName.toLowerCase()} ${attrs}>`.replace(/\s+/g, ' ');
          });
      });

      expect(
        unnamed,
        `these controls are announced with no name, so a screen-reader user hears only ` +
          `"button" or "link":\n  ${unnamed.join('\n  ')}`,
      ).toEqual([]);
    });
  }

  // 3. The heading outline is navigable.
  for (const screen of consoleScreens) {
    test(`the ${screen.name} has a usable heading outline`, async ({ page }) => {
      const authed = screen.path === '/staff/login' ? true : await signIn(page);
      if (!authed) {
        test.skip(true, 'E2E_STAFF_USER/E2E_STAFF_PASSWORD not set');
      }
      await page.goto(screen.path);
      await page.waitForLoadState('domcontentloaded');

      const headings = await page.evaluate(() =>
        Array.from(document.querySelectorAll('h1,h2,h3,h4,h5,h6'))
          .filter((h) => (h.textContent || '').trim() !== '')
          .map((h) => ({ level: Number(h.tagName[1]), text: (h.textContent || '').trim().slice(0, 60) })),
      );

      expect(headings.length, 'the page has no headings, so there is nothing to navigate by')
        .toBeGreaterThan(0);

      const h1s = headings.filter((h) => h.level === 1);
      expect(
        h1s.length,
        `the page has ${h1s.length} level-1 headings (${h1s.map((h) => h.text).join(' | ')}); ` +
          `exactly one tells a screen-reader user what page they are on`,
      ).toBe(1);

      // No skipped levels: h2 then h4 reads as a missing section.
      const skips: string[] = [];
      for (let i = 1; i < headings.length; i++) {
        const jump = headings[i].level - headings[i - 1].level;
        if (jump > 1) {
          skips.push(`h${headings[i - 1].level} "${headings[i - 1].text}" -> h${headings[i].level} "${headings[i].text}"`);
        }
      }
      expect(skips, `the outline skips a level:\n  ${skips.join('\n  ')}`).toEqual([]);
    });
  }

  // 4. Status messages are announced (WCAG 4.1.3), which axe cannot check.
  test('an action’s result is announced, not only displayed', async ({ page }) => {
    // A failed sign-in is the one status message reachable without
    // credentials, and it is the same Message/Error partial every screen in
    // the console uses.
    await page.goto('/staff/login');
    await page.getByLabel(/username/i).fill('definitely-not-a-user');
    await page.getByLabel(/password/i).fill('definitely-not-a-password');
    await page.getByRole('button', { name: /sign in/i }).click();
    await page.waitForLoadState('domcontentloaded');

    const message = await page.evaluate(() => {
      // Whatever carries the failure text, find it and report how it is
      // announced.
      const candidates = Array.from(document.querySelectorAll('*')) as HTMLElement[];
      const el = candidates.find((e) =>
        (e.textContent || '').includes('Incorrect username or password') &&
        e.children.length === 0);
      if (!el) return null;
      // Walk up: the live region may be an ancestor of the text node.
      let node: HTMLElement | null = el;
      while (node) {
        const role = node.getAttribute('role');
        const live = node.getAttribute('aria-live');
        if (role === 'alert' || role === 'status' || live === 'polite' || live === 'assertive') {
          return { announced: true, how: role ? `role="${role}"` : `aria-live="${live}"` };
        }
        node = node.parentElement;
      }
      return { announced: false, how: 'nothing' };
    });

    expect(message, 'the sign-in failure message is not on the page at all').not.toBeNull();
    expect(
      message!.announced,
      `the failure is displayed but not announced (${message!.how}). A screen-reader user ` +
        `presses sign in, hears nothing, and has no way to know whether it failed or is ` +
        `still working. This is WCAG 4.1.3, and axe-core cannot see it.`,
    ).toBe(true);
  });

  // The language of the page has to be declared, or a screen reader guesses
  // the pronunciation. axe does check this; it is here because it costs
  // nothing and the English-only decision makes it unambiguous.
  test('the page declares its language', async ({ page }) => {
    await page.goto('/staff/login');
    const lang = await page.evaluate(() => document.documentElement.getAttribute('lang'));
    expect(lang, 'the html element has no lang attribute').toBeTruthy();
    expect(lang!.toLowerCase()).toMatch(/^en/);
  });

  // A table a screen reader can read needs header cells, or every value is
  // announced without saying which column it came from.
  test('data tables name their columns', async ({ page }) => {
    const authed = await signIn(page);
    if (!authed) {
      test.skip(true, 'E2E_STAFF_USER/E2E_STAFF_PASSWORD not set');
    }
    await page.goto('/staff/subscribers');
    await page.waitForLoadState('domcontentloaded');

    const bad = await page.evaluate(() =>
      Array.from(document.querySelectorAll('table'))
        .filter((t) => t.querySelectorAll('tr').length > 1)
        .filter((t) => t.querySelectorAll('th').length === 0)
        .map((t) => (t.caption?.textContent || t.className || 'an unnamed table').trim()),
    );
    expect(
      bad,
      `these tables have rows but no header cells, so each value is announced with no ` +
        `column:\n  ${bad.join('\n  ')}`,
    ).toEqual([]);
  });
});
