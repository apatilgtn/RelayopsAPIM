// The core journey, through the console as an operator would do it:
// publish an API, see the gateway serve it, protect it with API keys, issue
// a key to a consumer, and find the refused and allowed requests in the logs
// with their decision trail.
const { test, expect, signIn, goTo, eventually, unique, upstreamURL } = require('../fixtures');

test('publish an API, protect it with a key, and trace requests', async ({ page, gateway }) => {
  const id = unique();
  const name = `orders-${id}`;
  const base = `/orders-${id}`;
  await signIn(page);

  // 1. Publish an open API from the New API form.
  await goTo(page, 'apis');
  await page.getByRole('button', { name: 'New API' }).click();
  const form = page.locator('.modal');
  await expect(form.getByRole('heading', { name: 'New API' })).toBeVisible();
  await form.locator('[name=name]').fill(name);
  await form.locator('[name=base_path]').fill(base);
  await form.locator('[name=upstream_url]').fill(upstreamURL);
  await form.locator('[name=description]').fill('Orders for the e2e storefront');
  await form.getByRole('button', { name: 'Create API' }).click();
  await expect(form).toBeHidden();
  await expect(page.locator('#view')).toContainText(name);

  // 2. The gateway serves it.
  await eventually(async () => {
    const res = await gateway.get(`${base}/items`);
    expect(res.status()).toBe(200);
  });

  // 3. Require API keys (Edit), and the gateway refuses anonymous calls.
  const row = page.locator('tr', { hasText: name });
  await row.locator('.row-menu summary').click();
  await page.locator('.row-menu[open] button', { hasText: 'Edit' }).click();
  await expect(form.getByRole('heading', { name: /Edit API/ })).toBeVisible();
  await form.locator('[name=auth_type]').selectOption('api_key');
  await form.getByRole('button', { name: 'Save changes' }).click();
  await expect(form).toBeHidden();
  await eventually(async () => {
    const res = await gateway.get(`${base}/items`);
    expect(res.status()).toBe(401);
  });

  // 4. A consumer gets a key and a subscription.
  await goTo(page, 'consumers');
  await page.getByRole('button', { name: 'New consumer' }).click();
  await form.locator('[name=name]').fill(`acme-${id}`);
  await form.locator('[name=email]').fill(`dev-${id}@acme.example`);
  await form.getByRole('button', { name: 'Create consumer' }).click();
  await expect(page.locator('#consumer-detail')).toContainText(`acme-${id}`);

  await page.getByRole('button', { name: 'Issue key' }).click();
  await form.getByRole('button', { name: 'Generate Key' }).click();
  const key = (await page.locator('#raw-key').innerText()).trim();
  expect(key).toMatch(/^rk_/);
  await page.locator('.modal footer').getByRole('button', { name: 'Cancel' }).click();
  await expect(page.locator('.modal')).toBeHidden();

  await page.getByRole('button', { name: 'Subscribe to API' }).click();
  const apiOption = form.locator('[name=api_id] option', { hasText: name });
  await form.locator('[name=api_id]').selectOption(await apiOption.getAttribute('value'));
  await form.getByRole('button', { name: 'Create Subscription' }).click();
  await expect(form).toBeHidden();
  await expect(page.locator('#consumer-detail')).toContainText(name);

  // 5. The key works; a wrong key does not.
  await eventually(async () => {
    const ok = await gateway.get(`${base}/items`, { headers: { 'X-API-Key': key } });
    expect(ok.status()).toBe(200);
  });
  const bad = await gateway.get(`${base}/items`, { headers: { 'X-API-Key': 'rk_not-a-real-key' } });
  expect(bad.status()).toBe(401);

  // 6. Request logs show the refused request; its decision trail explains it.
  await goTo(page, 'logs');
  await page.locator('#lf-q').fill(base);
  const refused = page.locator('#log-body tr[data-req-id]', { hasText: '401' }).first();
  await eventually(async () => { await page.locator('#lf-refresh').click(); await expect(refused).toBeVisible({ timeout: 2000 }); }, 20_000);
  await refused.click();
  const trail = page.locator('.modal');
  await expect(trail.locator('.callout')).toContainText('Refused');
  await expect(trail.locator('.decision-step.fail')).toContainText('Authentication');
  await expect(trail.locator('.decision-step.skip').first()).toContainText('Not evaluated');
  await trail.locator('footer').getByRole('button', { name: 'Close' }).click();
  await expect(trail).toBeHidden();
});
