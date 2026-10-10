const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/static/app.js', 'utf8');
const renderer = source.slice(source.indexOf('  function renderCanaryOverview('), source.indexOf('  views.fleet = async'));
const context = vm.createContext({esc:v=>String(v??'').replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('"','&quot;'),fmtNum:v=>String(v??0)});
vm.runInContext(renderer,context);
const render=(status,policy)=>context.renderCanaryOverview(status,12,policy);
const status={traffic_percent:10,canary_nodes_count:0,standard_nodes_count:2,standard_nodes_serving_split:2,comparison:{window_seconds:60,canary:{requests:0,error_rate_pct:0},baseline:{requests:20,error_rate_pct:0}}};
test('ready gateways with no traffic do not imply a healthy release',()=>{
 const html=render(status,{enabled:true,min_requests:5});
 assert.match(html,/Collecting evidence/);assert.match(html,/Too little candidate traffic/);assert.match(html,/10% candidate/);assert.match(html,/90% baseline/);
});
test('missing comparison and status stay explicit, rather than displaying healthy zeroes',()=>{
 const html=render(null,null);assert.match(html,/Status unavailable/);assert.match(html,/Do not infer release health/);assert.doesNotMatch(html,/Ready for review/);
});
test('node-only canary with no dedicated gateways explains missing traffic',()=>{
 const html=render({...status,traffic_percent:0},{});assert.match(html,/No dedicated canary gateways/);assert.match(html,/Waiting for gateways/);
});
test('header routing is escaped and allocation bounded',()=>{
 const html=render({...status,traffic_percent:110,header:'<script>'},{});assert.match(html,/100% candidate/);assert.match(html,/&lt;script>/);assert.doesNotMatch(html,/<script>/);
});
test('candidate evidence unlocks review language, not a claim of automatic safety',()=>{
 const html=render({...status,comparison:{...status.comparison,canary:{requests:40,error_rate_pct:20}}},{enabled:true,min_requests:5});
 assert.match(html,/Ready for review/);assert.match(html,/20.00%/);assert.match(html,/Gateway readiness alone does not prove release health/);
});