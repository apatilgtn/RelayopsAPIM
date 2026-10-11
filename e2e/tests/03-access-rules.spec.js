// Access rules at the gateway, set up through the admin API: a valid key is
// not enough without a subscription, "no plan" subscriptions work, and
// revoking a key or suspending a subscription takes effect immediately.
const { test, expect, eventually, unique, upstreamURL } = require('../fixtures');

test('a key needs a subscription; revocation and suspension apply at once', async ({ admin, gateway }) => {
  const id = unique();
  const base = `/billing-${id}`;
  const api = await (await admin.post('/api/apis', { data: { name: `billing-${id}`, base_path: base, upstream_url: upstreamURL, auth_type: 'api_key' } })).json();
  expect(api.id).toBeTruthy();
  const consumer = await (await admin.post('/api/consumers', { data: { name: `partner-${id}`, email: `p-${id}@example.test` } })).json();
  const { key, id: keyID } = await (await admin.post(`/api/consumers/${consumer.id}/keys`, { data: { name: 'ci' } })).json();
  const call = (k) => gateway.get(`${base}/invoices`, { headers: k ? { 'X-API-Key': k } : {} });

  // Valid key, no subscription: refused with the reason.
  await eventually(async () => {
    const res = await call(key);
    expect(res.status()).toBe(403);
    expect(res.headers()['x-relayops-decision-reason']).toBe('consumer_not_subscribed');
  });

  // Subscribing with "no plan" (an empty plan_id, as the console sends) works.
  const sub = await admin.post('/api/subscriptions', { data: { consumer_id: consumer.id, api_id: api.id, plan_id: '' } });
  expect(sub.status(), await sub.text()).toBe(201);
  const subID = (await sub.json()).id;
  await eventually(async () => expect((await call(key)).status()).toBe(200));

  // Suspending the subscription refuses the key again.
  // (the console's Suspend button: PUT with active=false and the current, empty plan)
  const suspend = await admin.put(`/api/subscriptions/${subID}`, { data: { plan_id: '', active: false } });
  expect(suspend.ok(), await suspend.text()).toBeTruthy();
  await eventually(async () => expect((await call(key)).status()).toBe(403));
  expect((await admin.put(`/api/subscriptions/${subID}`, { data: { plan_id: '', active: true } })).ok()).toBeTruthy();
  await eventually(async () => expect((await call(key)).status()).toBe(200));

  // Revoking the key refuses it at once.
  expect((await admin.post(`/api/keys/${keyID}/revoke`)).ok()).toBeTruthy();
  await eventually(async () => expect((await call(key)).status()).toBe(401));
});
