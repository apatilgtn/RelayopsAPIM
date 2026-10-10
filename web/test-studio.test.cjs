const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const source = fs.readFileSync(__dirname + '/static/test-studio.js', 'utf8');

function setupTestEnv() {
  const elements = new Map();
  const makeElem = (tag, id = '') => {
    const el = {
      tagName: tag.toUpperCase(),
      id: id,
      className: '',
      classList: {
        contains: cls => (el.className || '').split(' ').includes(cls),
        add: cls => { el.className = (el.className + ' ' + cls).trim(); },
        remove: cls => { el.className = (el.className || '').split(' ').filter(c => c !== cls).join(' '); }
      },
      innerHTML: '',
      children: [],
      attributes: {},
      setAttribute: (k, v) => { el.attributes[k] = String(v); },
      getAttribute: k => el.attributes[k],
      querySelector: sel => {
        if (sel.startsWith('#')) {
          const id = sel.slice(1);
          if (!elements.has(id)) elements.set(id, makeElem('div', id));
          return elements.get(id);
        }
        return makeElem('div');
      },
      querySelectorAll: sel => []
    };
    if (id) elements.set(id, el);
    return el;
  };

  const container = makeElem('div', 'view');

  const context = vm.createContext({
    clearInterval,
    setInterval,
    clearTimeout,
    setTimeout,
    window: {
      RelayUI: {
        icon: name => `<svg data-icon="${name}"></svg>`,
        replaceIcons: () => {}
      }
    },
    esc: v => String(v ?? '').replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('"', '&quot;'),
    statusPill: s => `<span class="pill">${s}</span>`,
    methodBadge: m => `<span class="badge">${m}</span>`,
    fmtMs: v => `${v}ms`,
    fmtDate: v => String(v),
    fmtNum: v => String(v ?? 0),
    $: sel => {
      if (sel === '#view') return container;
      if (sel.startsWith('#')) return elements.get(sel.slice(1)) || makeElem('div', sel.slice(1));
      return makeElem('div');
    },
    views: {},
    api: async () => {},
    toast: () => {},
    openModal: () => {}
  });

  vm.runInContext(source, context);
  return { context, container };
}

test('initTestStudio registers views.tests', () => {
  const { context } = setupTestEnv();
  context.window.initTestStudio(context);
  assert.equal(typeof context.views.tests, 'function');
});

test('views.tests handles 403 forbidden with explicit permission message', async () => {
  const { context, container } = setupTestEnv();
  context.api = async () => {
    const err = new Error('tenant access denied');
    err.status = 403;
    throw err;
  };
  context.window.initTestStudio(context);
  await context.views.tests();

  assert.match(container.innerHTML, /Access Denied/);
  assert.match(container.innerHTML, /do not have the required permissions/);
  assert.doesNotMatch(container.innerHTML, /Service Unavailable/);
});

test('views.tests handles backend connection error with Service Unavailable and retry', async () => {
  const { context, container } = setupTestEnv();
  context.api = async () => {
    throw new Error('connection refused: 127.0.0.1:9090');
  };
  context.window.initTestStudio(context);
  await context.views.tests();

  assert.match(container.innerHTML, /Service Unavailable/);
  assert.match(container.innerHTML, /connection refused/);
  assert.match(container.innerHTML, /Retry/);
});

test('views.tests renders tabs and toolbar on successful load', async () => {
  const { context, container } = setupTestEnv();
  context.api = async (method, path) => {
    if (path.includes('/suites')) return [{ id: 's1', name: 'Auth Suite' }];
    if (path.includes('/environments')) return [{ id: 'e1', name: 'Staging' }];
    if (path.includes('/runs')) return [];
    if (path.includes('/apis')) return [];
    return [];
  };
  context.window.initTestStudio(context);
  await context.views.tests();

  assert.match(container.innerHTML, /Test suites \(1\)/);
  assert.match(container.innerHTML, /Environments \(1\)/);
  assert.match(container.innerHTML, /Run history \(0\)/);
  assert.match(container.innerHTML, /New test suite/);
  assert.match(container.innerHTML, /Promotion gates/);
});

// Explicit HTTP status coverage complements the generic connection-error case.
test('views.tests handles HTTP 503 with unavailable state and retry', async () => {
  const { context, container } = setupTestEnv();
  context.api = async () => { const err = new Error('database_unavailable'); err.status = 503; throw err; };
  context.window.initTestStudio(context);
  await context.views.tests();
  assert.match(container.innerHTML, /Service Unavailable/);
  assert.match(container.innerHTML, /Retry/);
  assert.doesNotMatch(container.innerHTML, /Access Denied/);
});
