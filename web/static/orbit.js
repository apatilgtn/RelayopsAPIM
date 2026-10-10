/* Orbit AI: the operations assistant panel.
 * Answers come from POST /api/orbit/ask, which uses read-only tools under the
 * signed-in user's permissions. This file only renders; it never decides
 * what data a user may see.
 */
(() => {
  'use strict';

  const STORE_KEY = 'relayops_orbit_conversation';
  const toolLabels = {
    get_overview: () => 'Gateway overview',
    get_traffic_summary: (a) => `Traffic · ${a.window || '1h'}`,
    search_request_logs: (a) => 'Request logs' + (a.status ? ` · ${a.status}` : '') + (a.q ? ` · “${a.q}”` : ''),
    get_policy_refusals: (a) => `Refusals · ${a.window || '24h'}` + (a.protocol ? ` · ${a.protocol}` : ''),
    diagnose_request: (a) => `Diagnosis · ${String(a.request_id || '').slice(0, 10)}`,
    list_apis: () => 'APIs',
    get_fleet_status: () => 'Gateway fleet',
    list_releases: () => 'Releases',
    get_canary_status: (a) => `Canary · rev ${a.revision}`,
    get_auto_rollback: () => 'Auto-rollback',
    list_recent_changes: () => 'Audit log',
    get_signals: () => 'Signals',
    propose_api_change: () => 'Drafted a change',
  };
  const severityLabel = { critical: 'Critical', warning: 'Warning', info: 'Info' };
  const prompts = {
    live: ['Give me a health summary of the gateway', 'Is anything failing right now?', 'Which API is busiest in the last hour?'],
    analytics: ['Which API had the most errors today?', 'Is latency higher than usual in the last hour?', 'Who are the top consumers and are they healthy?'],
    logs: ['Summarize the 5xx errors in the last hour', 'Why are requests being refused?', 'Which consumers hit rate limits today?'],
    refusals: ['Why are requests being refused today?', 'Are any refusals likely to be misconfiguration?', 'Which API refuses the most requests?'],
    apis: ['Which APIs have no rate limit?', 'Which APIs are public without authentication?', 'Which APIs have errors today?'],
    fleet: ['Is the current release healthy?', 'What changed in the latest releases?', 'Are all gateways on the same revision?'],
    consumers: ['Which consumers send the most traffic?', 'Are any consumers being refused?'],
    subscriptions: ['Which consumers are refused for missing subscriptions?'],
    default: ['Give me a health summary of the gateway', 'What should I look at first today?', 'Why are requests being refused today?'],
  };

  let ctx = null;
  let panel = null;
  let messages = []; // {role, content, steps?, meta?, error?}
  let busy = false;
  let pendingContext = null;
  let signals = null; // last fetched list, or null before the first fetch
  let signalsAt = 0;

  const view = () => (location.hash.replace(/^#\//, '').split('/')[0] || 'live');
  const load = () => { try { messages = JSON.parse(sessionStorage.getItem(STORE_KEY) || '[]'); } catch { messages = []; } };
  const save = () => { try { sessionStorage.setItem(STORE_KEY, JSON.stringify(messages.slice(-24))); } catch { /* storage unavailable */ } };
  const features = () => window.RelayUI?.features?.() || {};

  // Small, safe Markdown subset: HTML is escaped first, then **bold**,
  // `code`, headings, bullet and numbered lists, and paragraphs.
  function render(text) {
    const esc = ctx.esc;
    const inline = (s) => esc(s)
      .replace(/`([^`]+)`/g, '<code>$1</code>')
      .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
      .replace(/(^|[\s(])\*([^*\s][^*]*?)\*(?=[\s).,:;!?]|$)/g, '$1<em>$2</em>');
    const cells = (row) => row.trim().replace(/^\||\|$/g, '').split('|').map((c) => c.trim());
    const out = [];
    let list = null;
    const close = () => { if (list) { out.push(`</${list}>`); list = null; } };
    // Some models append source markers such as 【list_apis】; drop them.
    const cleaned = String(text || '').replace(/【[^】]*】/g, '').replace(/[ \t]+([.,;:])/g, '$1');
    const lines = cleaned.split(/\r?\n/);
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i].trimEnd();
      let m;
      // Markdown table: a header row, a |---| separator, then body rows.
      if (/^\s*\|.*\|\s*$/.test(line) && /^\s*\|?[\s:|-]+\|?\s*$/.test(lines[i + 1] || '') && (lines[i + 1] || '').includes('-')) {
        close();
        const head = cells(line);
        const body = [];
        i += 2;
        while (i < lines.length && /^\s*\|.*\|\s*$/.test(lines[i])) body.push(cells(lines[i++]));
        i--;
        out.push(`<div class="orbit-table-wrap"><table class="orbit-table"><thead><tr>${head.map((h) => `<th>${inline(h)}</th>`).join('')}</tr></thead><tbody>${body.map((r) => `<tr>${r.map((c) => `<td>${inline(c)}</td>`).join('')}</tr>`).join('')}</tbody></table></div>`);
        continue;
      }
      if (!line.trim()) { close(); continue; }
      if ((m = line.match(/^\s*#{1,4}\s+(.*)$/))) { close(); out.push(`<p class="orbit-h">${inline(m[1])}</p>`); continue; }
      // "- 1. text" is a numbered step written as a bullet: render it once, as a step.
      if ((m = line.match(/^\s*[-*•]\s+\d+[.)]\s+(.*)$/))) { if (list !== 'ol') { close(); out.push('<ol>'); list = 'ol'; } out.push(`<li>${inline(m[1])}</li>`); continue; }
      if ((m = line.match(/^\s*[-*•]\s+(.*)$/))) { if (list !== 'ul') { close(); out.push('<ul>'); list = 'ul'; } out.push(`<li>${inline(m[1])}</li>`); continue; }
      if ((m = line.match(/^\s*\d+[.)]\s+(.*)$/))) { if (list !== 'ol') { close(); out.push('<ol>'); list = 'ol'; } out.push(`<li>${inline(m[1])}</li>`); continue; }
      close();
      out.push(`<p>${inline(line)}</p>`);
    }
    close();
    return out.join('');
  }

  function stepChips(steps) {
    if (!steps?.length) return '';
    return `<div class="orbit-steps" aria-label="Data Orbit checked"><span class="orbit-steps-label">Checked</span>${steps.map((s) => {
      const label = (toolLabels[s.tool] || (() => s.tool))(s.args || {});
      return `<span class="orbit-chip ${s.ok ? '' : 'failed'}" title="${ctx.esc(s.ok ? s.tool : (s.error || 'failed'))}">${ctx.esc(label)}${s.ok ? '' : ' · unavailable'}</span>`;
    }).join('')}</div>`;
  }

  const fmtValue = (v) => (v === true ? 'On' : v === false ? 'Off' : v === 0 ? '0 (unlimited)' : v === null || v === undefined || v === '' ? '—' : String(v));

  // A drafted change: what changes, its impact on recent traffic, and the
  // person's decision. Nothing is applied until they click Apply.
  function proposalHTML(p, mi, pi) {
    const esc = ctx.esc;
    const state = p.state || '';
    const impacts = Array.isArray(p.impacts) ? p.impacts : [];
    const traffic = p.recent_requests != null ? `<p class="orbit-prop-traffic">Checked against ${esc(p.recent_requests)} recent requests ${Number(p.active_consumers) > 0 ? `from ${esc(p.active_consumers)} consumer${Number(p.active_consumers) === 1 ? '' : 's'}` : 'from anonymous callers'}.</p>` : '';
    const actions = state === 'applied'
      ? `<p class="orbit-prop-done">${window.RelayUI.icon('check')} Applied. A new revision is live on the gateways.</p>`
      : state === 'dismissed'
        ? '<p class="orbit-prop-muted">Dismissed.</p>'
        : `${state.startsWith('error:') ? `<p class="orbit-prop-error">${esc(state.slice(6))}</p>` : ''}
           <div class="orbit-prop-actions"><button type="button" class="button small ghost" data-dismiss="${mi}:${pi}">Dismiss</button><button type="button" class="button small primary" data-apply="${mi}:${pi}">Apply change</button></div>`;
    return `<div class="orbit-prop ${state === 'applied' ? 'applied' : ''}">
      <div class="orbit-prop-head"><span class="tag blue">Proposed change</span><strong>${esc(p.api_name || p.api_id)}</strong></div>
      ${p.reason ? `<p class="orbit-prop-reason">${esc(p.reason)}</p>` : ''}
      <table class="orbit-prop-table"><tbody>${(p.changes || []).map((c) => `<tr><td>${esc(c.label)}</td><td class="from">${esc(fmtValue(c.from))}</td><td class="arrow" aria-label="to">→</td><td class="to">${esc(fmtValue(c.to))}</td></tr>`).join('')}</tbody></table>
      ${impacts.length ? `<ul class="orbit-prop-impacts">${impacts.map((i) => `<li class="${esc(i.severity)}">${esc(i.message)}${i.metric_value ? ` <span>${esc(i.metric_value)}</span>` : ''}</li>`).join('')}</ul>` : '<p class="orbit-prop-muted">No impact on recent traffic found.</p>'}
      ${traffic}
      ${actions}
    </div>`;
  }

  function messageHTML(m, mi) {
    if (m.role === 'user') return `<div class="orbit-msg user"><div class="orbit-bubble">${ctx.esc(m.content)}</div></div>`;
    if (m.error) return `<div class="orbit-msg orbit"><div class="orbit-error">${m.error}</div></div>`;
    // Model and token usage are tracked in the audit log and /metrics, not shown here.
    const meta = m.meta?.mode === 'context' ? '<div class="orbit-meta">Answered from a summary of gateway data</div>' : '';
    const props = (m.proposals || []).map((p, pi) => proposalHTML(p, mi, pi)).join('');
    return `<div class="orbit-msg orbit"><div class="orbit-answer">${render(m.content)}</div>${props}${stepChips(m.steps)}${meta}</div>`;
  }

  async function applyProposal(mi, pi, btn) {
    const p = messages[mi]?.proposals?.[pi];
    if (!p || p.state === 'applied') return;
    btn.disabled = true;
    btn.textContent = 'Applying…';
    try {
      await ctx.api('POST', '/api/orbit/proposals/apply', { token: p.token });
      p.state = 'applied';
      ctx.toast(`Applied the change to ${ctx.esc(p.api_name || 'the API')}`, 'success');
      fetchSignals(true);
    } catch (err) {
      p.state = 'error:' + (String(err.message || err).replace(/^HTTP \d+:\s*/, '') || 'Could not apply the change.');
    }
    save();
    paint();
  }

  // ---- Signals: found by rules on the server, explained by Orbit on request.
  async function fetchSignals(force) {
    if (!force && signals && Date.now() - signalsAt < 60_000) return signals;
    try {
      const res = await ctx.api('GET', '/api/orbit/signals' + (force ? '?refresh=1' : ''));
      signals = res.signals || [];
      signalsAt = Date.now();
    } catch {
      signals = signals || [];
    }
    updateBadge();
    return signals;
  }

  function updateBadge() {
    const btn = document.getElementById('orbit-btn');
    if (!btn) return;
    const urgent = (signals || []).filter((s) => s.severity !== 'info').length;
    let badge = btn.querySelector('.orbit-badge');
    if (!urgent) { badge?.remove(); btn.removeAttribute('data-urgent'); return; }
    if (!badge) { badge = document.createElement('span'); badge.className = 'orbit-badge'; btn.appendChild(badge); }
    badge.textContent = urgent;
    btn.setAttribute('data-urgent', String(urgent));
    btn.setAttribute('aria-label', `Ask Orbit (${urgent} item${urgent === 1 ? '' : 's'} need attention)`);
  }

  function signalHTML(s) {
    const esc = ctx.esc;
    return `<li class="orbit-signal ${esc(s.severity)}">
      <span class="orbit-sev" title="${esc(severityLabel[s.severity] || s.severity)}" aria-label="${esc(severityLabel[s.severity] || s.severity)}"></span>
      <div class="orbit-signal-body"><strong>${esc(s.title)}</strong><p>${esc(s.detail)}</p></div>
      <div class="orbit-signal-actions">
        <button type="button" class="button small" data-ask="${esc(s.ask)}">${window.RelayUI.icon('sparkles')}Ask Orbit</button>
        ${s.link ? `<a class="button small ghost" href="${esc(s.link)}">Open</a>` : ''}
      </div>
    </li>`;
  }

  // renderSignals fills a container (the Live traffic page) with the list.
  async function renderSignals(el) {
    if (!el) return;
    el.innerHTML = '<div class="card orbit-signals-card"><div class="orbit-signals-loading">Checking what needs attention…</div></div>';
    const list = await fetchSignals(false);
    if (!el.isConnected) return;
    const when = new Date(signalsAt || Date.now()).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    el.innerHTML = `<section class="card orbit-signals-card" aria-labelledby="orbit-signals-title">
      <div class="card-header"><div><h2 id="orbit-signals-title">Needs attention</h2><p class="muted small">Found by fixed rules over live gateway data · ${esc2(when)}</p></div>
        <button type="button" class="button small ghost" data-refresh-signals>Refresh</button></div>
      ${list.length ? `<ul class="orbit-signal-list">${list.map(signalHTML).join('')}</ul>`
        : `<div class="orbit-signals-clear">${window.RelayUI.icon('check')}<div><strong>Nothing needs attention</strong><p class="muted small">No error or latency spikes, refusal surges, rollbacks, drifting gateways or held MCP definitions.</p></div></div>`}
    </section>`;
    el.onclick = async (e) => {
      const ask = e.target.closest('[data-ask]');
      if (ask) open({ send: true, prompt: ask.dataset.ask });
      if (e.target.closest('[data-refresh-signals]')) { await fetchSignals(true); renderSignals(el); }
    };
  }
  const esc2 = (s) => ctx.esc(s);

  function intro() {
    const list = prompts[view()] || prompts.default;
    const off = features().orbit === false;
    return `<div class="orbit-intro">
      <p class="orbit-lede">Ask about traffic, errors, refusals, releases and policies. Orbit checks live gateway data with your permissions and shows what it looked at.</p>
      ${off ? `<div class="orbit-setup"><strong>Orbit is not connected to a model yet.</strong><p>An operator sets <code>RELAYOPS_ORBIT_BASE_URL</code>, <code>RELAYOPS_ORBIT_MODEL</code> and <code>RELAYOPS_ORBIT_API_KEY</code> on the control plane. Any OpenAI-compatible endpoint works, including a RelayOps AI route.</p></div>` : ''}
      ${signals && signals.length ? `<p class="orbit-section">Needs attention</p><ul class="orbit-signal-list compact">${signals.slice(0, 3).map(signalHTML).join('')}</ul>` : ''}
      <p class="orbit-section">Suggested for this page</p>
      <div class="orbit-prompts">${list.map((p) => `<button type="button" class="orbit-prompt" data-prompt="${ctx.esc(p)}">${ctx.esc(p)}</button>`).join('')}</div>
    </div>`;
  }

  function paint() {
    const log = panel.querySelector('.orbit-log');
    log.innerHTML = (messages.length ? messages.map((m, i) => messageHTML(m, i)).join('') : intro()) +
      (busy ? '<div class="orbit-msg orbit"><div class="orbit-thinking" role="status"><span></span><span></span><span></span> Checking gateway data…</div></div>' : '');
    log.scrollTop = log.scrollHeight;
    panel.querySelector('.orbit-new').hidden = !messages.length;
    const send = panel.querySelector('.orbit-send');
    send.disabled = busy;
  }

  async function ask(text, context) {
    text = String(text || '').trim();
    if (!text || busy) return;
    messages.push({ role: 'user', content: text });
    busy = true;
    paint();
    const history = messages.filter((m) => !m.error).slice(-12).map((m) => ({ role: m.role === 'user' ? 'user' : 'assistant', content: m.content }));
    try {
      const ans = await ctx.api('POST', '/api/orbit/ask', { messages: history, context: { view: view(), ...(context || {}) } });
      messages.push({ role: 'assistant', content: ans.text || 'Orbit returned an empty answer.', steps: ans.steps || [], proposals: ans.proposals || [],
        meta: { mode: ans.mode } });
    } catch (err) {
      const msg = String(err.message || err);
      const notConfigured = /orbit_not_configured|no model configured/i.test(msg);
      messages.push({ role: 'assistant', content: '', error: notConfigured
        ? '<strong>Orbit is not connected to a model.</strong> Ask an operator to set <code>RELAYOPS_ORBIT_BASE_URL</code>, <code>RELAYOPS_ORBIT_MODEL</code> and <code>RELAYOPS_ORBIT_API_KEY</code>.'
        : `<strong>Orbit could not answer.</strong> ${ctx.esc(msg)}` });
    } finally {
      busy = false;
      save();
      paint();
      panel.querySelector('textarea').focus();
    }
  }

  function build() {
    panel = document.createElement('aside');
    panel.className = 'orbit-panel';
    panel.id = 'orbit-panel';
    panel.setAttribute('role', 'dialog');
    panel.setAttribute('aria-modal', 'false');
    panel.setAttribute('aria-labelledby', 'orbit-title');
    panel.hidden = true;
    panel.innerHTML = `
      <header class="orbit-head">
        <span class="orbit-mark" aria-hidden="true">${window.RelayUI.icon('sparkles')}</span>
        <div class="orbit-title"><h2 id="orbit-title">Orbit AI</h2><p>Operations assistant · read-only</p></div>
        <button type="button" class="button small ghost orbit-new" hidden>New chat</button>
        <button type="button" class="button small ghost orbit-close" aria-label="Close Orbit">${window.RelayUI.icon('x')}</button>
      </header>
      <div class="orbit-log" aria-live="polite"></div>
      <form class="orbit-input">
        <textarea rows="1" placeholder="Ask Orbit about your gateway…" aria-label="Ask Orbit" maxlength="4000"></textarea>
        <button type="submit" class="orbit-send" aria-label="Send">${window.RelayUI.icon('arrow-up')}</button>
      </form>
      <p class="orbit-foot">Orbit reads live data but cannot change configuration. Check its evidence before acting.</p>`;
    document.body.appendChild(panel);

    const ta = panel.querySelector('textarea');
    const grow = () => { ta.style.height = 'auto'; ta.style.height = Math.min(ta.scrollHeight, 160) + 'px'; };
    ta.addEventListener('input', grow);
    ta.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); panel.querySelector('form').requestSubmit(); }
    });
    panel.querySelector('form').addEventListener('submit', (e) => {
      e.preventDefault();
      const text = ta.value;
      ta.value = '';
      grow();
      const c = pendingContext;
      pendingContext = null;
      ask(text, c);
    });
    panel.addEventListener('click', (e) => {
      const p = e.target.closest('[data-prompt]');
      if (p) ask(p.dataset.prompt);
      const sig = e.target.closest('[data-ask]');
      if (sig) ask(sig.dataset.ask);
      const apply = e.target.closest('[data-apply]');
      if (apply) { const [mi, pi] = apply.dataset.apply.split(':').map(Number); applyProposal(mi, pi, apply); }
      const dismiss = e.target.closest('[data-dismiss]');
      if (dismiss) {
        const [mi, pi] = dismiss.dataset.dismiss.split(':').map(Number);
        const prop = messages[mi]?.proposals?.[pi];
        if (prop) { prop.state = 'dismissed'; save(); paint(); }
      }
      if (e.target.closest('.orbit-panel a[href^="#/"]')) close();
      if (e.target.closest('.orbit-close')) close();
      if (e.target.closest('.orbit-new')) { messages = []; save(); paint(); ta.focus(); }
    });
    panel.addEventListener('keydown', (e) => { if (e.key === 'Escape') { e.stopPropagation(); close(); } });
  }

  let opener = null;
  function open(opts = {}) {
    if (!panel) build();
    opener = document.activeElement;
    panel.hidden = false;
    document.body.classList.add('orbit-open');
    document.getElementById('orbit-btn')?.setAttribute('aria-expanded', 'true');
    paint();
    if (!messages.length) fetchSignals(false).then(() => { if (!panel.hidden && !messages.length && !busy) paint(); });
    const ta = panel.querySelector('textarea');
    if (opts.prompt) {
      if (opts.send) ask(opts.prompt, opts.context);
      else { ta.value = opts.prompt; pendingContext = opts.context || null; ta.dispatchEvent(new Event('input')); }
    }
    requestAnimationFrame(() => ta.focus());
  }
  function close() {
    if (!panel || panel.hidden) return;
    panel.hidden = true;
    document.body.classList.remove('orbit-open');
    document.getElementById('orbit-btn')?.setAttribute('aria-expanded', 'false');
    if (opener?.isConnected) opener.focus();
  }
  const toggle = () => (panel && !panel.hidden ? close() : open());

  window.initOrbit = function (c) {
    ctx = c;
    load();
    const right = document.querySelector('.topbar-right');
    if (right && !document.getElementById('orbit-btn')) {
      const mac = /Mac|iPhone|iPad/.test(navigator.platform);
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.id = 'orbit-btn';
      btn.className = 'button small orbit-trigger';
      btn.setAttribute('aria-expanded', 'false');
      btn.setAttribute('aria-controls', 'orbit-panel');
      btn.innerHTML = `${window.RelayUI.icon('sparkles')}<span>Ask Orbit</span><kbd>${mac ? '⌘' : 'Ctrl'} K</kbd>`;
      btn.addEventListener('click', toggle);
      right.prepend(btn);
    }
    document.addEventListener('keydown', (e) => {
      if ((e.metaKey || e.ctrlKey) && !e.altKey && e.key.toLowerCase() === 'k') { e.preventDefault(); toggle(); }
    });
    // Suggested prompts follow the page.
    window.addEventListener('hashchange', () => { if (panel && !panel.hidden && !messages.length) paint(); });
    // The badge on the button counts warnings and critical signals.
    setTimeout(() => fetchSignals(false), 1500);
    setInterval(() => { if (!document.hidden) fetchSignals(false); }, 120_000);
  };
  window.RelayOrbit = Object.freeze({ open, close, renderSignals, render: (text) => render(text) });
})();
