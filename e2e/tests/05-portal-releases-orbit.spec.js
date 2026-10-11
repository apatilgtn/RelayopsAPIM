// The developer portal as an API consumer sees it, releases and canaries,
// and Orbit's experience without a model.
const { test, expect, signIn, goTo, eventually, unique, upstreamURL, gatewayURL } = require('../fixtures');

test('portal: public APIs are listed with references that point at the public gateway', async ({ page, admin }) => {
  const id = unique();
  const api = await (await admin.post('/api/apis', { data: { name: `catalog-${id}`, description: 'Public product catalog', base_path: `/catalog-${id}`, upstream_url: upstreamURL } })).json();
  await page.goto('/portal');
  await expect(page.locator('#catalog-grid')).toContainText(`catalog-${id}`);
  const ref = await (await page.request.get(`/portal/api/apis/${api.id}/openapi.json`)).json();
  expect(ref.servers[0].url).toBe(`${gatewayURL}/catalog-${id}`);
  const catalog = await (await page.request.get('/portal/api/catalog')).json();
  expect(catalog.gateway_url).toBe(gatewayURL);
});

test('releases: a new release can go to canary and be aborted, restoring the baseline', async ({ page, admin, gateway }) => {
  const id = unique();
  await admin.post('/api/apis', { data: { name: `canary-${id}`, base_path: `/canary-${id}`, upstream_url: upstreamURL } });
  await eventually(async () => expect((await gateway.get(`/canary-${id}/x`)).status()).toBe(200));
  await signIn(page);
  await goTo(page, 'fleet');
  const latest = page.locator('.release-history-card').first();
  await expect(latest).toContainText('Live on all gateways');
  await latest.getByRole('button', { name: 'Move to canary' }).click();
  const dialog = page.locator('.modal');
  await expect(dialog).toContainText('becomes the baseline again');
  await dialog.locator('#cn-pct').fill('10');
  await dialog.getByRole('button', { name: 'Start canary' }).click();
  await expect(dialog).toBeHidden();
  await expect(page.locator('.release-history-card').first()).toContainText('Canary');

  // Abort from the canary overview; the dialog asks for confirmation.
  await page.getByRole('button', { name: 'Abort canary' }).first().click();
  await page.locator('.modal footer button').last().click();
  await eventually(async () => {
    const revs = await (await admin.get('/api/revisions')).json();
    expect(revs.some((r) => r.status === 'canary')).toBe(false);
  });
});

test('Orbit without a model explains how to connect one, and signals still work', async ({ page }) => {
  await signIn(page);
  // Signals come from rules, not a model, so they render either way.
  await expect(page.locator('#orbit-signals .orbit-signals-card')).toBeVisible();
  await page.keyboard.press(process.platform === 'darwin' ? 'Meta+K' : 'Control+K');
  const panel = page.locator('#orbit-panel');
  await expect(panel).toBeVisible();
  await expect(panel).toContainText('not connected to a model');
  // Asking without a model is a 503 the browser logs; that is expected here.
  page.removeAllListeners('console');
  await panel.locator('.orbit-prompt').first().click();
  await expect(panel.locator('.orbit-error')).toContainText('not connected to a model');
  await page.keyboard.press('Escape');
  await expect(panel).toBeHidden();
});
