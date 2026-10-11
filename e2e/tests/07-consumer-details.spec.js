// "Manage Keys & Subs" with a realistic number of consumers. The details
// render below the list; with many consumers that is off-screen, and the
// click looked like it did nothing. Checks visibility in the viewport, not
// just presence in the page.
const { test, expect, signIn, goTo, unique } = require('../fixtures');

test('Manage Keys & Subs brings the consumer details into view with many consumers', async ({ page, admin }) => {
  const id = unique();
  for (let i = 0; i < 35; i++) {
    const res = await admin.post('/api/consumers', { data: { name: `bulk-${id}-${String(i).padStart(2, '0')}`, email: `bulk-${id}-${i}@example.test` } });
    expect(res.ok()).toBeTruthy();
  }
  await page.setViewportSize({ width: 1371, height: 754 }); // the size in the bug report
  await signIn(page);
  await goTo(page, 'consumers');

  const target = `bulk-${id}-30`;
  const row = page.locator('#view tr', { hasText: target });
  await row.getByRole('link', { name: 'Manage Keys & Subs' }).click();

  const heading = page.locator('#consumer-detail h2', { hasText: `API keys · ${target}` });
  await expect(heading).toBeInViewport();
  await expect(heading).toBeFocused();
  await expect(page.locator('#consumer-detail')).toContainText(`Subscriptions · ${target}`);

  // Opening another consumer from the list does the same.
  await page.evaluate(() => window.scrollTo(0, 0));
  const other = `bulk-${id}-05`;
  await page.locator('#view tr', { hasText: other }).getByRole('link', { name: other }).click();
  await expect(page.locator('#consumer-detail h2', { hasText: `API keys · ${other}` })).toBeInViewport();
});
