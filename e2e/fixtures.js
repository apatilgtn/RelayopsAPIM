// Shared fixtures: a signed-in console page that fails the test on any
// browser error, and clients for the admin API and the gateway.
const base = require('@playwright/test');
const ports = require('./ports');

const gatewayURL = `http://127.0.0.1:${ports.gateway}`;
const upstreamURL = `http://127.0.0.1:${ports.upstream}`;

// unique returns a short suffix so tests never collide in the shared database.
const unique = () => Date.now().toString(36) + Math.random().toString(36).slice(2, 5);

const test = base.test.extend({
  // Every page in a test collects uncaught errors and console errors.
  page: async ({ page }, use) => {
    const errors = [];
    page.on('pageerror', (err) => errors.push('pageerror: ' + err.message));
    page.on('console', (msg) => { if (msg.type() === 'error' && !/favicon/.test(msg.text())) errors.push('console: ' + msg.text()); });
    await use(page);
    base.expect(errors, 'browser errors during the test').toEqual([]);
  },
  // admin calls the control-plane API as the platform administrator.
  admin: async ({ playwright }, use) => {
    const ctx = await playwright.request.newContext({
      baseURL: `http://127.0.0.1:${ports.console}`,
      extraHTTPHeaders: { Authorization: `Bearer ${ports.adminToken}` },
    });
    await use(ctx);
    await ctx.dispose();
  },
  // gateway calls the data plane like an API consumer.
  gateway: async ({ playwright }, use) => {
    const ctx = await playwright.request.newContext({ baseURL: gatewayURL });
    await use(ctx);
    await ctx.dispose();
  },
});

// signIn uses the real sign-in page with the administrator token.
async function signIn(page) {
  await page.goto('/login');
  await page.getByRole('button', { name: 'Use an administrator token' }).click();
  await page.locator('#admin-token').fill(ports.adminToken);
  await page.getByRole('button', { name: /Sign in with administrator token/ }).click();
  await page.waitForURL(/#\/live/);
  await base.expect(page.locator('#view')).not.toBeEmpty();
}

// goTo opens a console page and waits for its content.
async function goTo(page, view) {
  await page.evaluate((v) => { location.hash = '#/' + v; }, view);
  await base.expect(page.locator('#view .product-page-header, #view .card').first()).toBeVisible();
}

// eventually retries an async check until it passes (gateways apply
// configuration within a second or two).
async function eventually(fn, timeout = 10_000) {
  const end = Date.now() + timeout;
  let last;
  while (Date.now() < end) {
    try { return await fn(); } catch (err) { last = err; await new Promise((r) => setTimeout(r, 300)); }
  }
  throw last;
}

module.exports = { test, expect: base.expect, signIn, goTo, eventually, unique, gatewayURL, upstreamURL, ports };
