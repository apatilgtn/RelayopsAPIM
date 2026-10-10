// Orbit renders model output into the console. Model text is untrusted: it can
// echo request paths, API descriptions or anything a caller sent through the
// gateway, so the renderer must never let it create markup.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function loadOrbit() {
  const listeners = [];
  const window = { RelayUI: { icon: () => '', features: () => ({}) }, addEventListener: () => {} };
  const document = { querySelector: () => null, getElementById: () => null, addEventListener: (...a) => listeners.push(a), body: { classList: { add() {}, remove() {} } } };
  const context = vm.createContext({ window, document, navigator: { platform: 'Linux' }, location: { hash: '#/live' }, sessionStorage: { getItem: () => null, setItem() {} }, requestAnimationFrame: () => {}, setTimeout: () => 0, setInterval: () => 0, Event: class {} });
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'static', 'orbit.js'), 'utf8'), context);
  const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  window.initOrbit({ esc, api: async () => ({}), toast: () => {} });
  return window.RelayOrbit;
}

test('model output cannot inject markup', () => {
  const orbit = loadOrbit();
  const html = orbit.render('Path was <img src=x onerror=alert(1)> and **<script>alert(2)</script>** with `<b>code</b>`');
  assert.ok(!/<img|<script|<b>/i.test(html), html);
  assert.match(html, /&lt;img src=x onerror=alert\(1\)&gt;/);
  assert.match(html, /<strong>&lt;script&gt;alert\(2\)&lt;\/script&gt;<\/strong>/);
  assert.match(html, /<code>&lt;b&gt;code&lt;\/b&gt;<\/code>/);
});

test('drops model source markers', () => {
  const orbit = loadOrbit();
  const html = orbit.render('Only relayops-mcp is open 【exposure:open】. Settings 【list_apis】.');
  assert.ok(!html.includes('【') && !html.includes('list_apis'), html);
  assert.match(html, /is open\. Settings\./);
});

test('renders tables and emphasis, escaped', () => {
  const orbit = loadOrbit();
  const html = orbit.render('*Reason*: busy.\n\n| API | Limit |\n|---|---|\n| catalog | <b>0</b> |\n| orders | 600 |\n\nAfter.');
  assert.match(html, /<em>Reason<\/em>: busy\./);
  assert.match(html, /<table class="orbit-table"><thead><tr><th>API<\/th><th>Limit<\/th><\/tr><\/thead><tbody><tr><td>catalog<\/td><td>&lt;b&gt;0&lt;\/b&gt;<\/td><\/tr><tr><td>orders<\/td><td>600<\/td><\/tr><\/tbody><\/table>/);
  assert.match(html, /<p>After\.<\/p>/);
  assert.ok(!orbit.render('a * b * c').includes('<em>'), 'multiplication-like text must not become italics');
});

test('renders lists, steps and headings', () => {
  const orbit = loadOrbit();
  const html = orbit.render('## Cause\nIt failed.\n\n- one\n- two\n\n- 1. first step\n- 2. second step\n\n3. third');
  assert.match(html, /<p class="orbit-h">Cause<\/p>/);
  assert.match(html, /<ul><li>one<\/li><li>two<\/li><\/ul>/);
  assert.match(html, /<ol><li>first step<\/li><li>second step<\/li><\/ol>/);
  assert.ok(!html.includes('1. first'), 'numbered bullets must not be numbered twice');
});
