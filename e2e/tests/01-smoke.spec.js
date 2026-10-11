// Every console page loads after a real sign-in, without browser errors and
// without horizontal scrolling, on desktop and phone widths.
const { test, expect, signIn, goTo } = require('../fixtures');

const pages = [
  ['live', 'Live traffic'], ['analytics', 'Analytics'], ['logs', 'Request logs'], ['refusals', 'Policy refusals'],
  ['apis', 'APIs'], ['ai', 'AI Management'], ['tests', 'API Test Studio'], ['consumers', 'Consumers'],
  ['subscriptions', 'Subscriptions'], ['plans', 'Plans'], ['fleet', 'Releases & fleet'], ['apiops', 'APIOps'],
  ['approvals', 'Approvals'], ['audit', 'Audit log'], ['team', 'Users & roles'], ['tenants', 'Tenants'], ['providers', 'OIDC'],
];

test('sign-in rejects a wrong token and accepts the administrator token', async ({ page }) => {
  await page.goto('/login');
  await page.getByRole('button', { name: 'Use an administrator token' }).click();
  await page.locator('#admin-token').fill('wrong-token');
  // A rejected sign-in is a 401 the browser logs; that is expected here.
  page.removeAllListeners('console');
  await page.getByRole('button', { name: /Sign in with administrator token/ }).click();
  await expect(page.locator('#form-message')).toBeVisible();
  await expect(page).toHaveURL(/\/login/);
  await signIn(page);
});

for (const [width, label] of [[1440, 'desktop'], [390, 'phone']]) {
  test(`every console page loads cleanly (${label})`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await signIn(page);
    for (const [view, title] of pages) {
      await goTo(page, view);
      await expect(page.locator('#view h1').first(), `page title on #/${view}`).toContainText(title);
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth + 1);
      expect(overflow, `horizontal scrolling on #/${view} at ${width}px`).toBe(false);
    }
  });
}
