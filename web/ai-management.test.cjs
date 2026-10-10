const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const source = fs.readFileSync(__dirname + '/static/ai-management.js', 'utf8');

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
        remove: cls => { el.className = (el.className || '').split(' ').filter(c => c !== cls).join(' '); },
        toggle: (cls, force) => {
          if (force !== undefined) {
            if (force) el.classList.add(cls); else el.classList.remove(cls);
          } else {
            if (el.classList.contains(cls)) el.classList.remove(cls); else el.classList.add(cls);
          }
        }
      },
      innerHTML: '',
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
    window: {},
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
    }
  });

  vm.runInContext(source, context);
  return { context, container };
}

test('AI Management module exports initAIManagement and registers views.ai', () => {
  const { context } = setupTestEnv();
  assert.equal(typeof context.window.initAIManagement, 'function');

  const views = {};
  const mockApi = async (method, path) => [];
  const mockToast = () => {};
  const mockOpenModal = () => {};

  context.window.initAIManagement({
    views,
    api: mockApi,
    toast: mockToast,
    esc: context.esc,
    openModal: mockOpenModal,
    statusPill: context.statusPill,
    methodBadge: context.methodBadge,
    fmtMs: context.fmtMs,
    fmtDate: context.fmtDate,
    fmtNum: context.fmtNum,
    $: context.$
  });

  assert.equal(typeof views.ai, 'function');
});

test('views.ai renders toolbar and services tab content without errors', async () => {
  const { context, container } = setupTestEnv();
  const views = {};
  const mockApi = async (method, path) => {
    if (path === '/api/ai/providers') {
      return [{ id: 'p1', name: 'OpenAI Test', provider_type: 'openai', base_url: 'https://api.openai.com', api_key_secret_ref: '${secret:KEY}' }];
    }
    if (path === '/api/ai/models') {
      return [{ id: 'm1', deployment_name: 'gpt-4o', model_name: 'gpt-4o', context_window_tokens: 128000, max_output_tokens: 4096, input_price_per_million: 2.5, output_price_per_million: 10.0 }];
    }
    if (path === '/api/ai/services') {
      return [{ id: 's1', name: 'Chat AI', alias: '/v1/chat/completions', routing_policy: 'fallback', primary_model_deployment_id: 'm1', created_at: new Date().toISOString() }];
    }
    return [];
  };

  context.window.initAIManagement({
    views,
    api: mockApi,
    toast: () => {},
    esc: context.esc,
    openModal: () => {},
    statusPill: context.statusPill,
    methodBadge: context.methodBadge,
    fmtMs: context.fmtMs,
    fmtDate: context.fmtDate,
    fmtNum: context.fmtNum,
    $: context.$
  });

  await views.ai('services');
  assert.ok(container.innerHTML.includes('Services &amp; models'));
  assert.ok(container.innerHTML.includes('Budget ledger'));
  assert.ok(container.innerHTML.includes('Evaluated releases'));
});
