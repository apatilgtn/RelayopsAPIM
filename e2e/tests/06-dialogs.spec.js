// Opens every dialog reachable from every console page and tab, and checks
// each one: it opens without browser errors, and a dialog with inputs has a
// way to submit (not only Cancel). This catches dialogs whose submit button
// is missing or that crash on open. Destructive or immediate actions are
// skipped, and no dialog is ever confirmed.
const { test, expect, signIn } = require('../fixtures');

const views = ['live', 'analytics', 'logs', 'refusals', 'apis', 'ai', 'tests', 'consumers', 'subscriptions', 'plans', 'fleet', 'apiops', 'approvals', 'audit', 'team', 'tenants', 'providers'];
const SKIP = /^(refresh|retry|delete|remove|revoke|suspend|activate|reactivate|promote|abort|approve|reject|sign out|export|download|copy|publish|unpublish|disable|enable|run|evaluate|rollback|restore|roll back|resolve|discover|developer hub|close|cancel|issue key|rotate|ask orbit|new chat|dismiss|apply change|move to canary|verify|promote and seal)/i;

// show opens a page (and tab) with no dialog open.
async function show(page, view, tab) {
  await page.evaluate(() => document.querySelectorAll('#modal-root > *').forEach((m) => m.remove()));
  await page.evaluate((v) => { location.hash = '#/x'; location.hash = '#/' + v; }, view);
  await expect(page.locator('#view .product-page-header').first()).toBeVisible();
  if (tab) await page.evaluate((t) => document.querySelector(`#view [data-tab="${t}"]`)?.click(), tab);
  await page.waitForTimeout(300);
}

// labels lists the buttons that may open a dialog on the current page.
const labels = (page) => page.evaluate((skip) => {
  const re = new RegExp(skip, 'i');
  const seen = new Set();
  return [...document.querySelectorAll('#view button')]
    .filter((b) => !b.matches('[data-tab], .filter, [role=tab]') && (b.offsetParent !== null || b.closest('.row-menu-items')))
    .map((b) => b.textContent.trim().replace(/\s+/g, ' '))
    .filter((t) => t && !re.test(t) && !seen.has(t) && seen.add(t));
}, SKIP.source);

for (const view of views) {
  test(`dialogs on #/${view} open cleanly and can be submitted`, async ({ page }) => {
    test.setTimeout(180_000);
    await signIn(page);
    await show(page, view);
    const tabs = await page.evaluate(() => [...new Set([...document.querySelectorAll('#view [data-tab]')].map((b) => b.dataset.tab))]);
    let checked = 0;
    for (const tab of [null, ...tabs]) {
      await show(page, view, tab);
      for (const label of await labels(page)) {
        if (!(await page.evaluate((v) => location.hash === '#/' + v, view))) await show(page, view, tab);
        const clicked = await page.evaluate((text) => {
          const b = [...document.querySelectorAll('#view button')].find((x) => x.textContent.trim().replace(/\s+/g, ' ') === text);
          if (!b) return false;
          const menu = b.closest('details');
          if (menu) menu.open = true;
          b.click();
          return true;
        }, label);
        if (!clicked) continue;
        const dialog = page.locator('#modal-root .modal').last();
        if (!(await dialog.isVisible({ timeout: 1200 }).catch(() => false))) continue;
        const where = `#/${view}${tab ? '/' + tab : ''} → "${label}"`;
        const footer = (await dialog.locator('footer button').allInnerTexts()).map((t) => t.trim());
        const inputs = await dialog.locator('.body input:not([type=checkbox]):not([type=hidden]), .body textarea, .body select').count();
        expect(footer.length, `${where}: dialog has no footer buttons`).toBeGreaterThan(0);
        if (inputs > 0) {
          expect(footer.filter((t) => !/^cancel$/i.test(t)).length, `${where}: a dialog with inputs has no submit button`).toBeGreaterThan(0);
        }
        checked++;
        await page.evaluate(() => document.querySelectorAll('#modal-root > *').forEach((m) => m.remove()));
      }
    }
    test.info().annotations.push({ type: 'dialogs checked', description: String(checked) });
  });
}
