// Quickstart step 3, as a new developer would do it: open the developer portal,
// register an application for the Demo API through the real form, and read the
// one-time key it shows. Prints the key (local quickstart stack only).
const { chromium } = require('@playwright/test');
(async () => {
  const base = process.env.BASE || 'http://localhost:9090';
  const browser = await chromium.launch({ channel: 'chrome' });
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  const errors = [];
  page.on('pageerror', e => errors.push(String(e)));
  await page.goto(base + '/portal/');
  await page.getByRole('link', { name: /Get access/i }).first().click();
  await page.locator('#reg-name').fill('Quickstart app');
  await page.locator('#reg-email').fill('dev@example.com');
  const demo = page.locator('#reg-api-list').getByText('Demo API', { exact: false }).first();
  await demo.waitFor({ state: 'visible', timeout: 15000 });
  await demo.click();
  await page.locator('#register-submit').click();
  await page.locator('#reg-result').waitFor({ state: 'visible', timeout: 15000 });
  const key = await page.locator('#reg-key').inputValue();
  if (!key) throw new Error('no key shown after registration');
  await page.screenshot({ path: '../test-results/quickstart-portal-key.png' });
  console.log('KEY=' + key);
  console.log('script errors:', errors.length ? errors : 'none');
  await browser.close();
})().catch(e => { console.error('FAILED:', e.message); process.exit(1); });
