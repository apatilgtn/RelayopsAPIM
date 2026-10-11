// Administration forms submitted through the console, then checked through
// the admin API, so a form that looks fine but saves the wrong data fails.
const { test, expect, signIn, goTo, unique } = require('../fixtures');

test('plans, tenants, identity providers and users save what the form says', async ({ page, admin }) => {
  const id = unique();
  await signIn(page);
  const form = page.locator('.modal');

  // Plan
  await goTo(page, 'plans');
  await page.getByRole('button', { name: 'New plan' }).click();
  await form.locator('[name=name]').fill(`Gold-${id}`);
  await form.locator('[name=rate_limit_per_minute]').fill('1200');
  await form.locator('[name=quota_per_day]').fill('50000');
  await form.locator('footer').getByRole('button', { name: 'Save' }).click();
  await expect(form).toBeHidden();
  const plan = (await (await admin.get('/api/plans')).json()).find((p) => p.name === `Gold-${id}`);
  expect(plan).toMatchObject({ rate_limit_per_minute: 1200, quota_per_day: 50000 });

  // Tenant: create, then rename
  await goTo(page, 'tenants');
  await page.getByRole('button', { name: 'Create tenant' }).click();
  await form.locator('[name=name]').fill(`Acme ${id}`);
  await form.locator('[name=slug]').fill(`acme-${id}`);
  await form.getByRole('button', { name: 'Create tenant' }).click();
  await expect(form).toBeHidden();
  await expect(page.locator('#view')).toContainText(`acme-${id}`);
  // Rows with three actions collapse into an Actions menu.
  const tenantRow = page.locator('tr', { hasText: `acme-${id}` });
  await tenantRow.locator('.row-menu summary').click();
  await page.locator(`.row-menu[open] [data-edittenant="acme-${id}"]`).click();
  await expect(form.locator('[name=slug]')).toHaveAttribute('readonly', '');
  await form.locator('[name=name]').fill(`Acme Corporation ${id}`);
  await form.getByRole('button', { name: 'Save changes' }).click();
  await expect(form).toBeHidden();
  const tenants = await (await admin.get('/api/tenants')).json();
  expect(tenants.find((t) => t.slug === `acme-${id}`)?.name).toBe(`Acme Corporation ${id}`);

  // OIDC identity provider
  await goTo(page, 'providers');
  await page.getByRole('button', { name: 'Add OIDC provider' }).click();
  await form.locator('[name=name]').fill(`okta-${id}`);
  await form.locator('[name=issuer]').fill('https://example.okta.com');
  await form.locator('[name=client_id]').fill('client-123');
  await form.locator('[name=allowed_domains]').fill('acme.example, corp.example');
  await form.getByRole('button', { name: 'Add provider' }).click();
  await expect(form).toBeHidden();
  const idp = (await (await admin.get('/api/admin/oidc-providers')).json()).find((p) => p.name === `okta-${id}`);
  expect(idp).toMatchObject({ issuer: 'https://example.okta.com', client_id: 'client-123' });
  expect(idp.allowed_domains).toEqual(['acme.example', 'corp.example']);

  // Administrator
  await goTo(page, 'team');
  await page.getByRole('button', { name: 'Add user' }).click();
  await form.locator('[name=name]').fill(`Riley ${id}`);
  await form.locator('[name=email]').fill(`riley-${id}@acme.example`);
  await form.locator('[name=role]').selectOption('auditor');
  await form.getByRole('button', { name: 'Add administrator' }).click();
  await expect(form).toBeHidden();
  const users = await (await admin.get('/api/admin/users')).json();
  expect(users.find((u) => u.email === `riley-${id}@acme.example`)?.role).toBe('auditor');
});

test('APIOps: a production environment is saved as production, and a deployment can be planned', async ({ page, admin }) => {
  const id = unique();
  await signIn(page);
  const form = page.locator('.modal');
  await goTo(page, 'apiops');
  await page.locator('#view [data-tab="environments"]').click();
  await page.locator('#apiops-tab-actions').getByRole('button', { name: /New environment/ }).click();
  await form.locator('#env-name').fill(`Production ${id}`);
  await form.locator('#env-desc').fill('Customer-facing gateways');
  await form.locator('#env-prod').check();
  await form.getByRole('button', { name: 'Create environment' }).click();
  await expect(form).toBeHidden();
  const env = (await (await admin.get('/api/apiops/environments')).json()).find((e) => e.name === `Production ${id}`);
  expect(env).toMatchObject({ is_production: true, description: 'Customer-facing gateways' });

  await page.locator('#view [data-tab="deployments"]').click();
  await page.locator('#btn-plan-dep').click();
  await expect(form.getByRole('heading', { name: 'Plan deployment' })).toBeVisible();
  await form.locator('#plan-env-id').selectOption(env.id);
  // Validation keeps the dialog open.
  await form.getByRole('button', { name: 'Plan deployment' }).click();
  await expect(form).toBeVisible();
  await form.locator('#plan-commit-sha').fill(`abc${id}`);
  await form.getByRole('button', { name: 'Plan deployment' }).click();
  await expect(form).toBeHidden();
  await expect(page.locator('#view')).toContainText(`abc${id}`.slice(0, 7));

  // Verifying a deployment that has no candidate release yet is a clear
  // precondition failure, not a server error.
  const dep = (await (await admin.get('/api/apiops/deployments')).json()).find((d) => d.commit_sha === `abc${id}`);
  const verify = await admin.post(`/api/apiops/deployments/${dep.id}/verify`);
  expect(verify.status()).toBe(412);
  expect((await verify.json()).message).toContain('no active candidate revision');
});
