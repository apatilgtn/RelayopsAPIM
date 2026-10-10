const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/static/app.js', 'utf8').replace(/\r\n/g, '\n');
const start = source.indexOf('  async function loadAnalytics() {');
const end = source.indexOf('\n  // -------------------------------------------------------------------------', start);
assert.ok(start >= 0 && end > start, 'analytics renderer must exist');
const renderer = source.slice(start, end);

async function render(summary) {
  const body = { innerHTML: '' };
  const context = vm.createContext({
    api: async () => summary, anWindow: '1h', currentView: 'analytics',
    $: id => id === '#an-body' ? body : {},
    esc: value => String(value ?? ''), fmtNum: value => String(value ?? 0),
    fmtMs: value => value == null ? '–' : value + ' ms', drawChart: () => {}
  });
  await vm.runInContext(renderer + '\nloadAnalytics()', context);
  return body.innerHTML;
}

test('analytics renders null and absent rankings without crashing', async () => {
  for (const lists of [{top_apis:null, top_consumers:null, top_models:null}, {}]) {
    const html = await render({total:0, window:'1h', ...lists});
    assert.equal((html.match(/No activity recorded in this time window\./g) || []).length, 2);
    assert.ok(html.includes('Total Requests'));
    assert.ok(!html.includes('LLM Tokens Metered'));
    assert.ok(!html.includes('Top AI Models Metered'));
  }
});

test('analytics preserves populated API rankings beside empty consumer rankings', async () => {
  const html = await render({total:12, window:'1h', top_apis:[{name:'Payments',count:12}],top_consumers:[],top_models:null});
  assert.ok(html.includes('Payments'));
  assert.ok(html.includes('<td>12</td>'));
  assert.equal((html.match(/No activity recorded in this time window\./g) || []).length, 1);
});
