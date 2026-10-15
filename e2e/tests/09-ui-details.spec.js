// Details a design review found: the top-bar revision on any page, the
// portal's registration confirmation, and dates that read the same in every
// locale.
const { test, expect, signIn, eventually, unique, upstreamURL } = require('../fixtures');

const dayFirst = /\b\d{1,2} [A-Z][a-z]{2} \d{4}, \d{2}:\d{2}:\d{2}\b/;

test('the top-bar revision is shown on a page other than Overview', async ({ page }) => {
  await signIn(page);
  await page.goto('/#/fleet');
  await page.reload(); // a fresh console that never visits Overview
  await expect(page.locator('#config-badge')).toHaveText(/^rev_\d{6}$/);
});

test('a request detail shows a day-first date with a named month', async ({ page, admin, gateway }) => {
  const id = unique();
  const path = `/dates-${id}`;
  await admin.post('/api/apis', { data: { name: `Dates ${id}`, base_path: path, upstream_url: upstreamURL, auth_type: 'none' } });
  await eventually(async () => expect((await gateway.get(path + '/items')).status()).toBe(200));
  await signIn(page);
  await page.goto('/#/logs');
  const row = page.locator(`#log-body tr[data-req-id]`, { hasText: path }).first();
  await eventually(async () => {
    await page.reload();
    await expect(row).toBeVisible({ timeout: 2000 });
  }, 30_000);
  await row.click();
  await expect(page.locator('.trail-head .muted').first()).toHaveText(dayFirst);
});

test('portal registration confirms with a visible check mark', async ({ page, admin }) => {
  const id = unique();
  await admin.post('/api/apis', { data: { name: `Register ${id}`, base_path: `/register-${id}`, upstream_url: upstreamURL, auth_type: 'api_key' } });
  await page.goto('/portal#register');
  await page.locator('#reg-name').fill(`App ${id}`);
  await page.locator('#reg-email').fill(`dev-${id}@example.test`);
  await page.locator('#reg-api-list label', { hasText: `Register ${id}` }).locator('input').check();
  await page.locator('#register-submit').click();
  await expect(page.locator('#reg-result')).toBeVisible();
  const icon = page.locator('#reg-result .success-icon svg');
  await expect(icon).toBeVisible();
  const box = await icon.boundingBox();
  expect(box.width).toBeGreaterThan(10);
});
