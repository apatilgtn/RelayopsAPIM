// Ports and credentials of the isolated end-to-end stack.
module.exports = {
  console: Number(process.env.E2E_CONSOLE_PORT || 29090),
  gateway: Number(process.env.E2E_GATEWAY_PORT || 28080),
  status: Number(process.env.E2E_STATUS_PORT || 29091),
  upstream: Number(process.env.E2E_UPSTREAM_PORT || 27080),
  adminToken: 'e2e-admin-token-not-a-secret-0123456789',
};
