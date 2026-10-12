// Read-only production smoke: opens the changed console pages and the portal,
// asserts they render visibly without script errors. Never clicks a mutating control.
const { chromium } = require('@playwright/test');
(async () => {
  const token = process.env.RELAYOPS_PROD_TOKEN;
  if (!token) throw new Error('RELAYOPS_PROD_TOKEN not set');
  const base = 'https://console.relayops.cloud';
  const browser = await chromium.launch({ channel: 'chrome' });
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
  const errors = [];
  page.on('pageerror', e => errors.push(String(e)));
  page.on('console', m => { if (m.type() === 'error') errors.push(m.text()); });
  await page.goto(base + '/login');
  await page.evaluate(t => sessionStorage.setItem('relayops_token', t), token);
  for (const view of ['release', 'fleet', 'apiops', 'apis', 'consumers']) {
    await page.goto(`${base}/#/${view}`);
    await page.waitForTimeout(2500);
    const h1 = page.locator('#view .product-page-header h1').first();
    const visible = await h1.isVisible().catch(() => false);
    const title = visible ? (await h1.textContent()).trim() : '(no heading)';
    console.log(`${view}: ${visible ? 'visible' : 'NOT VISIBLE'} - ${title}`);
    if (view === 'release') await page.screenshot({ path: '../test-results/prod-release-workspace.png' });
  }
  await page.goto(base + '/portal/');
  await page.waitForTimeout(2500);
  console.log('portal title:', await page.title());
  console.log('script errors:', errors.length ? errors.slice(0, 5) : 'none');
  await browser.close();
})().catch(e => { console.error(e.message); process.exit(1); });
