// @ts-check
const { defineConfig, devices } = require('@playwright/test');
const ports = require('./ports');

// Locally, use the installed Chrome (no browser download); in CI, the
// Playwright-managed Chromium.
const browser = process.env.CI ? {} : { channel: 'chrome' };

module.exports = defineConfig({
  testDir: './tests',
  // One stack and one database: tests run in order and build on each other.
  fullyParallel: false,
  workers: 1,
  timeout: 60_000,
  expect: { timeout: 10_000 },
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : [['list']],
  use: {
    baseURL: `http://127.0.0.1:${ports.console}`,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    ...browser,
  },
  projects: [
    { name: 'desktop', use: { ...devices['Desktop Chrome'], ...browser, viewport: { width: 1440, height: 900 } } },
  ],
  webServer: {
    command: 'node stack.js',
    url: `http://127.0.0.1:${ports.console}/healthz`,
    timeout: 240_000,
    reuseExistingServer: false,
    stdout: 'ignore',
    stderr: 'pipe',
  },
});
