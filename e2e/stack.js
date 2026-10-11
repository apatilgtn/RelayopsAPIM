// Starts an isolated RelayOps stack for the end-to-end tests: a fresh
// database, the mock upstream and a RelayOps node, on ports that do not
// clash with a developer's own instance (8080/9090). Playwright runs this as
// its web server and stops it when the tests finish.
const { spawn, execFileSync } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');
const { Client } = require('pg');
const ports = require('./ports');

const root = path.resolve(__dirname, '..');
const binDir = path.join(__dirname, '.bin');
const exe = process.platform === 'win32' ? '.exe' : '';
const adminURL = process.env.E2E_POSTGRES_URL || 'postgres://postgres@localhost:5432/postgres?sslmode=disable';
const dbName = process.env.E2E_DATABASE || 'relayops_e2e';

async function freshDatabase() {
  const admin = new Client({ connectionString: adminURL });
  await admin.connect();
  await admin.query(`DROP DATABASE IF EXISTS ${dbName} WITH (FORCE)`);
  await admin.query(`CREATE DATABASE ${dbName}`);
  await admin.end();
  const u = new URL(adminURL);
  u.pathname = '/' + dbName;
  return u.toString();
}

function build(name) {
  fs.mkdirSync(binDir, { recursive: true });
  const out = path.join(binDir, name + exe);
  execFileSync('go', ['build', '-o', out, './cmd/' + name], { cwd: root, stdio: 'inherit' });
  return out;
}

(async () => {
  const dbURL = await freshDatabase();
  const children = [];
  const stop = () => { for (const c of children) { try { c.kill(); } catch { /* already gone */ } } process.exit(0); };
  process.on('SIGINT', stop);
  process.on('SIGTERM', stop);
  process.on('exit', () => { for (const c of children) { try { c.kill(); } catch { /* already gone */ } } });

  const upstream = spawn(build('mockupstream'), ['-addr', `127.0.0.1:${ports.upstream}`, '-jitter-ms', '0'], { stdio: 'inherit' });
  children.push(upstream);
  const relayops = spawn(build('relayops'), [], {
    cwd: binDir,
    stdio: 'inherit',
    env: {
      ...process.env,
      RELAYOPS_DATABASE_URL: dbURL,
      RELAYOPS_PROXY_ADDR: `127.0.0.1:${ports.gateway}`,
      RELAYOPS_ADMIN_ADDR: `127.0.0.1:${ports.console}`,
      RELAYOPS_STATUS_ADDR: `127.0.0.1:${ports.status}`,
      RELAYOPS_ADMIN_TOKEN: ports.adminToken,
      RELAYOPS_REDIS_URL: 'disabled',
      RELAYOPS_NODE_ID: 'e2e',
      RELAYOPS_PREVIEW_AI: 'true',
      RELAYOPS_PREVIEW_APIOPS: 'true',
      RELAYOPS_PUBLIC_URL: `http://127.0.0.1:${ports.console}`,
      RELAYOPS_PUBLIC_GATEWAY_URL: `http://127.0.0.1:${ports.gateway}`,
      // Orbit stays unconfigured: the tests check its no-model experience.
      RELAYOPS_ORBIT_BASE_URL: '',
      RELAYOPS_ORBIT_MODEL: '',
    },
  });
  children.push(relayops);
  relayops.on('exit', (code) => { console.error(`relayops exited (${code})`); stop(); });
})().catch((err) => { console.error(err); process.exit(1); });
