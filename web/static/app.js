/* RelayOps APIM Control Plane — dependency-free, real-time via Server-Sent Events.
 * Adheres to Developer Hub aesthetic, design tokens, and safe operations paradigm.
 */
(() => {
  'use strict';

  // -------------------------------------------------------------------------
  // utilities & formatters
  // -------------------------------------------------------------------------
  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];
  const actionData = (e) => e.target.closest('[data-edit],[data-del],[data-publish],[data-preview],[data-safety],[data-mcp],[data-policies],[data-plan],[data-toggle],[data-compare],[data-canary-status],[data-canary],[data-abort],[data-promote],[data-rollback],[data-edituser],[data-deluser],[data-members],[data-edittenant],[data-deltenant],[data-delmember],[data-editprov],[data-delprov],[data-approve],[data-reject],[data-revoke],[data-activate],[data-delkey],[data-delsub],[data-account-approve]')?.dataset || {};
  const previewAI = () => !!(window.RelayUI && typeof window.RelayUI.features === 'function' && window.RelayUI.features().preview_ai);
  const esc = (v) => String(v ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  const fmtNum = (n) => (n ?? 0).toLocaleString();
  const fmtMs = (n) => (n == null ? '–' : n < 10 ? n.toFixed(2) + ' ms' : n < 1000 ? n.toFixed(0) + ' ms' : (n / 1000).toFixed(2) + ' s');
  const fmtBytes = (b) => { b = b || 0; const u = ['B', 'KB', 'MB', 'GB']; let i = 0; while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; } return b.toFixed(i ? 1 : 0) + ' ' + u[i]; };
  const fmtTime = (t) => new Date(t).toLocaleTimeString([], { hour12: false });
  const fmtDate = (t) => new Date(t).toLocaleString([], { hour12: false });
  const fmtUptime = (s) => { const d = Math.floor(s / 86400), h = Math.floor(s % 86400 / 3600), m = Math.floor(s % 3600 / 60); return d ? `${d}d ${h}h` : h ? `${h}h ${m}m` : `${m}m ${s % 60}s`; };

  const statusPill = (s) => {
    const cls = s >= 500 ? 'tag error' : s >= 400 ? 'tag warn' : s >= 300 ? 'tag warn' : s === 101 ? 'tag blue' : 'tag success';
    return `<span class="${cls}">${s}</span>`;
  };

  const authPill = (a) => ({
    none: '<span class="tag gray">open</span>',
    api_key: '<span class="tag blue">API key</span>',
    jwt: '<span class="tag warn">JWT</span>',
    oidc: '<span class="tag warn">OIDC</span>'
  }[a] || `<span class="tag">${esc(a)}</span>`);

  const methodBadge = (m) => `<span class="method ${(m || 'get').toLowerCase()}">${esc(m || 'GET')}</span>`;
  const limitText = (n) => (n > 0 ? `${fmtNum(n)}/min` : 'unlimited');

  function toast(msg, kind = '') {
    kind = { alert: 'error', danger: 'error', warn: 'warning' }[kind] || kind;
    const root = $('#toasts');
    if (!root) return;
    const el = document.createElement('div');
    el.className = 'toast ' + kind;
    el.innerHTML = msg;
    root.appendChild(el);
    setTimeout(() => {
      el.style.opacity = '0';
      el.style.transform = 'translateY(10px)';
      el.style.transition = 'all 0.2s';
      setTimeout(() => el.remove(), 250);
    }, 4500);
  }

  // -------------------------------------------------------------------------
  // auth & API client
  // -------------------------------------------------------------------------
  let token = sessionStorage.getItem('relayops_token') || localStorage.getItem('relayops_token') || '';
  if (!token) { location.replace('/login'); return; }

  async function api(method, path, body) {
    const res = await fetch(path, {
      method,
      headers: {
        'Authorization': 'Bearer ' + token,
        ...(body !== undefined ? { 'Content-Type': 'application/json' } : {})
      },
      body: body !== undefined ? (typeof body === 'string' ? body : JSON.stringify(body)) : undefined,
    });
    if (res.status === 401) {
      askToken('Admin token rejected or session expired.');
      throw new Error('unauthorized');
    }
    if (res.status === 204) return null;
    const data = await res.json().catch(() => ({}));
    if (!res.ok) { const err = new Error(data.message || data.error || res.statusText); err.status = res.status; throw err; }
    return data;
  }

  async function apiRaw(method, path, body) {
    const res = await fetch(path, {
      method,
      headers: { 'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : (typeof body === 'string' ? body : JSON.stringify(body)),
    });
    if (res.status === 401) {
      askToken('Admin token rejected or session expired.');
      throw new Error('unauthorized');
    }
    const data = await res.json().catch(() => ({}));
    return { ok: res.ok, status: res.status, data };
  }

  async function askToken() {
    const tok = sessionStorage.getItem('relayops_token') || localStorage.getItem('relayops_token');
    if (tok) {
      try {
        await fetch('/api/auth/logout', {
          method: 'POST',
          headers: { 'Authorization': 'Bearer ' + tok }
        });
      } catch (_) {}
    }
    sessionStorage.removeItem('relayops_token');
    localStorage.removeItem('relayops_token');
    location.assign('/login');
  }

  // -------------------------------------------------------------------------
  // modal & form dialogs
  // -------------------------------------------------------------------------
  function openModal({ id = '', title, body, actions = [], dismissable = true, large = false, onOpen, confirmText, onConfirm, danger = false }) {
    // Shorthand used by some views: one confirm button whose handler returns
    // false to keep the dialog open (validation or a failed request).
    if (confirmText && onConfirm && !actions.length) {
      actions = [{ label: confirmText, primary: !danger, danger, onClick: async (close, wrap, btn) => {
        btn.disabled = true;
        try { if ((await onConfirm(wrap)) !== false) close(); } finally { if (btn.isConnected) btn.disabled = false; }
      } }];
    }
    const root = $('#modal-root');
    const wrap = document.createElement('div');
    wrap.className = 'modal-backdrop';
    if (id) wrap.id = id;
    wrap.innerHTML = `
      <div class="modal ${large ? 'large' : ''}" role="dialog" aria-modal="true">
        <header>
          <h2>${esc(title)}</h2>
          ${dismissable ? '<button class="button ghost small" data-close aria-label="Close"></button>' : ''}
        </header>
        <div class="body">${body}</div>
        <footer>
          ${!dismissable ? '' : !actions.length ? '<button class="button" data-close>Close</button>' : actions.some((a) => /^(close|done)\b/i.test(a.label.trim())) ? '' : '<button class="button ghost" data-close>Cancel</button>'}
          ${actions.map((a, i) =>
            `<button class="button ${a.primary ? 'primary' : ''} ${a.danger ? 'danger' : ''}" data-action="${i}">${esc(a.label)}</button>`
          ).join('')}
        </footer>
      </div>
    `;
    const previousFocus = document.activeElement;
    const behind = [...document.querySelectorAll('body>.rail,body>.workspace,#modal-root>.modal-backdrop')].map(el=>({el,inert:el.inert}));
    behind.forEach(({el})=>el.inert=true);
    const close = () => { wrap.remove(); behind.forEach(({el,inert})=>el.inert=inert); if (previousFocus?.isConnected) previousFocus.focus(); else $('#main')?.focus(); };
    const focusable = () => [...wrap.querySelectorAll('button:not(:disabled),a[href],input:not(:disabled),select:not(:disabled),textarea:not(:disabled),[tabindex="0"]')].filter(el => el.getClientRects().length);
    wrap.querySelector('[data-close]')?.insertAdjacentHTML('afterbegin', RelayUI.icon('x'));
    wrap.addEventListener('click', (e) => {
      if (e.target.closest('[data-close]') || (e.target === wrap && dismissable)) close();
      const act = e.target.closest('[data-action]');
      if (act) actions[+act.dataset.action].onClick(close, wrap, act);
    });
    wrap.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && e.target.tagName === 'INPUT' && !e.target.matches('textarea') && actions[0]) {
        e.preventDefault(); wrap.querySelector('[data-action="'+actions.findIndex(a=>a.primary)+'"]')?.click();
      }
      if (e.key === 'Escape' && dismissable) { e.stopPropagation(); close(); }
      if (e.key === 'Tab') {
        const items = focusable(), first = items[0], last = items.at(-1);
        if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last?.focus(); }
        else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first?.focus(); }
      }
    });
    root.appendChild(wrap);
    onOpen?.(wrap);
    requestAnimationFrame(() => (wrap.querySelector('input,select,textarea') || focusable()[0])?.focus());
    close.close = close; // callers use either close() or modal.close()
    close.element = wrap;
    return close;
  }

  function fieldHTML(f) {
    const v = f.value ?? '';
    const name = `name="${f.name}"`;
    let input;
    switch (f.type) {
      case 'checkbox':
        return `
          <div class="field check" data-field="${f.name}">
            <input type="checkbox" ${name} id="f-${f.name}" ${v ? 'checked' : ''}>
            <label for="f-${f.name}">${esc(f.label)}</label>
            ${f.help ? `<div class="help">${esc(f.help)}</div>` : ''}
          </div>`;
      case 'select':
        input = `<select ${name} ${f.disabled ? 'disabled' : ''}>${f.options.map((o) => (typeof o === 'object' ? o : { value: o, label: o[0].toUpperCase() + o.slice(1) })).map((o) => `<option value="${esc(o.value)}" ${String(o.value) === String(v) ? 'selected' : ''}>${esc(o.label)}</option>`).join('')}</select>`;
        break;
      case 'textarea':
        input = `<textarea ${name} rows="${f.rows || 3}" placeholder="${esc(f.placeholder || '')}">${esc(v)}</textarea>`;
        break;
      default:
        // A locked field is read-only, not disabled, so its value is still submitted.
        input = `<input type="${f.type || 'text'}" ${name} value="${esc(v)}" placeholder="${esc(f.placeholder || '')}" ${f.required ? 'required' : ''} ${f.disabled ? 'readonly aria-readonly="true"' : ''}>`;
    }
    return `
      <div class="field" data-field="${f.name}">
        <label>${esc(f.label)}</label>
        ${input}
        ${f.help ? `<div class="help">${esc(f.help)}</div>` : ''}
      </div>`;
  }

  function openForm({ title, fields, submitLabel = 'Save', onSubmit, onChange, large = false }) {
    const rows = fields.map((f) => Array.isArray(f) ? `<div class="form-row">${f.map(fieldHTML).join('')}</div>` : fieldHTML(f)).join('');
    const flat = fields.flat();
    openModal({
      title,
      large,
      body: `<div class="form-error" hidden></div>${rows}`,
      onOpen: (wrap) => {
        if (onChange) {
          wrap.addEventListener('input', () => onChange(read(wrap), wrap));
          onChange(read(wrap), wrap);
        }
      },
      actions: [{
        label: submitLabel,
        primary: true,
        onClick: async (close, wrap, btn) => {
          const err = $('.form-error', wrap);
          err.hidden = true;
          if (btn) btn.disabled = true;
          try {
            await onSubmit(read(wrap), () => {}); // the form closes itself afterwards
            close();
            route(true);
          } catch (e) {
            err.textContent = e.message;
            err.hidden = false;
          } finally {
            if (btn) btn.disabled = false;
          }
        }
      }],
    });
    function read(wrap) {
      const out = {};
      for (const f of flat) {
        const el = $(`[name="${f.name}"]`, wrap);
        if (!el) continue;
        if (f.type === 'checkbox') out[f.name] = el.checked;
        else if (f.type === 'number') out[f.name] = el.value === '' ? 0 : Number(el.value);
        else out[f.name] = el.value;
      }
      return out;
    }
  }

  function confirmDialog(title, message, onYes, label = 'Delete') {
    openModal({
      title,
      body: `<div>${message}</div>`,
      actions: [{
        label,
        danger: true,
        onClick: async (close) => {
          try {
            await onYes();
            close();
            route(true);
          } catch (e) {
            toast(esc(e.message), 'error');
          }
        }
      }]
    });
  }

  // -------------------------------------------------------------------------
  // real-time SSE stream & cluster coordination
  // -------------------------------------------------------------------------
  const MAX_TICKS = 120;
  const live = { ticks: [], tail: [], last: null, configVersion: null };
  let es = null;

  function connectStream() {
    es?.close();
    if (!token) return;
    const conn = $('#conn');
    es = new EventSource('/api/stream?token=' + encodeURIComponent(token));
    es.addEventListener('hello', (e) => {
      conn.className = 'conn live';
      $('#conn-text').textContent = 'Live SSE';
      const d = JSON.parse(e.data);
      $('#node-info').textContent = 'Node: ' + d.node;
    });
    es.addEventListener('tick', (e) => {
      const t = JSON.parse(e.data);
      live.last = t;
      live.ticks.push(t);
      if (live.ticks.length > MAX_TICKS) live.ticks.shift();
      for (const r of t.recent) live.tail.unshift(r);
      live.tail.length = Math.min(live.tail.length, 150);
      if (currentView === 'live') renderLiveUpdate(t);
    });
    es.addEventListener('alert', (e) => {
      const a = JSON.parse(e.data);
      toast(`<b>${esc(a.title)}</b><div class="small">${esc(a.text || '')}</div>${a.type && a.type.startsWith('mcp_') ? '<a href="#/approvals">Review in Approvals</a>' : ''}`, a.severity === 'critical' ? 'error' : 'warn');
      if (currentView === 'approvals') route(true);
      updateBadges?.();
    });
    es.addEventListener('config', (e) => {
      const c = JSON.parse(e.data);
      setConfigVersion(c.version);
      if (c.reason !== 'periodic_resync') {
        toast(` Gateway revision <b>rev_${String(c.version).padStart(10, '0')}</b> applied in ${c.took_ms.toFixed(1)} ms <span class="muted">(${esc(c.reason)})</span>`, 'success');
        if (['apis', 'consumers', 'subscriptions', 'plans', 'approvals', 'fleet', 'tenants', 'providers'].includes(currentView)) route(true);
      }
      for (const err of c.errors || []) toast(esc(err), 'error');
    });
    es.onerror = () => {
      conn.className = 'conn down';
      $('#conn-text').textContent = 'Reconnecting…';
      fetch('/api/overview', { headers: { Authorization: 'Bearer ' + token } }).then((r) => {
        if (r.status === 401) { es.close(); askToken(); }
      });
    };
  }

  function setConfigVersion(v) {
    const b = $('#config-badge');
    if (!b) return;
    const revStr = 'rev_' + String(v).padStart(6, '0');
    b.textContent = revStr;
    if (live.configVersion !== null && live.configVersion !== v) {
      b.classList.remove('flash');
      void b.offsetWidth;
      b.classList.add('flash');
    }
    live.configVersion = v;
  }

  // -------------------------------------------------------------------------
  // canvas charts
  // -------------------------------------------------------------------------
  // drawChart plots line series on a canvas. A null value leaves a gap (no
  // data for that bucket) rather than drawing a misleading zero.
  function drawChart(canvas, series, opts = {}) {
    if (!canvas) return;
    const { yFmt = (v) => fmtNum(Math.round(v)), labels = [] } = opts;
    const integer = opts.integer ?? !opts.yFmt; // counts get whole-number ticks
    const dpr = window.devicePixelRatio || 1;
    const w = canvas.clientWidth, h = canvas.clientHeight;
    if (canvas.width !== w * dpr || canvas.height !== h * dpr) {
      canvas.width = w * dpr; canvas.height = h * dpr;
    }
    const ctx = canvas.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);
    const padL = 48, padR = 10, padT = 10, padB = labels.length ? 22 : 8;
    const n = Math.max(...series.map((s) => s.values.length), 2);
    const peak = Math.max(0, ...series.flatMap((s) => s.values.filter((v) => v != null)));
    // Four ticks on a 1/2/2.5/5 x 10^k step, so labels never repeat.
    // Empty charts keep a readable scale (0-8 for counts, 0-2 otherwise).
    const raw = Math.max(peak, integer ? 4 : 2) / 4;
    const mag = Math.pow(10, Math.floor(Math.log10(raw)));
    let step = [1, 2, 2.5, 5, 10].map((m) => m * mag).find((v) => v >= raw * 1.0001) || 10 * mag;
    if (integer) step = Math.max(1, Math.ceil(step));
    const max = step * 4;
    const x = (i) => padL + (i / (n - 1)) * (w - padL - padR);
    const y = (v) => padT + (1 - v / max) * (h - padT - padB);

    ctx.font = '12px Inter, sans-serif';
    ctx.fillStyle = '#657575';
    ctx.strokeStyle = '#dfe6e0';
    ctx.lineWidth = 1;
    for (let i = 0; i <= 4; i++) {
      const v = step * i, yy = Math.round(y(v)) + 0.5;
      ctx.beginPath(); ctx.moveTo(padL, yy); ctx.lineTo(w - padR, yy); ctx.stroke();
      ctx.textAlign = 'right'; ctx.textBaseline = 'middle'; ctx.fillText(yFmt(v), padL - 8, yy);
    }
    if (labels.length) {
      ctx.textAlign = 'center'; ctx.textBaseline = 'top';
      const every = Math.ceil(labels.length / 6);
      labels.forEach((l, i) => { if (i % every === 0) ctx.fillText(l, x(i), h - padB + 6); });
    }
    for (const s of series) {
      const off = n - s.values.length;
      // Split into runs of consecutive values; nulls separate runs.
      const runs = [];
      let run = null;
      s.values.forEach((v, i) => {
        if (v == null) { run = null; return; }
        if (!run) runs.push(run = []);
        run.push([x(i + off), y(v)]);
      });
      for (const pts of runs) {
        if (pts.length === 1) {
          ctx.fillStyle = s.color;
          ctx.beginPath(); ctx.arc(pts[0][0], pts[0][1], 2.5, 0, Math.PI * 2); ctx.fill();
          continue;
        }
        const trace = () => { ctx.beginPath(); pts.forEach(([px, py], i) => (i ? ctx.lineTo(px, py) : ctx.moveTo(px, py))); };
        if (s.fill) {
          trace();
          ctx.lineTo(pts.at(-1)[0], y(0)); ctx.lineTo(pts[0][0], y(0)); ctx.closePath();
          const g = ctx.createLinearGradient(0, padT, 0, h - padB);
          g.addColorStop(0, s.color + '44');
          g.addColorStop(1, s.color + '00');
          ctx.fillStyle = g; ctx.fill();
        }
        trace();
        ctx.strokeStyle = s.color;
        ctx.lineWidth = 2;
        ctx.lineJoin = 'round';
        ctx.stroke();
      }
    }
  }

  function bars(items, total) {
    if (!items.length) return '<div class="empty small">No traffic recorded</div>';
    const max = Math.max(...items.map((i) => i[1]), 1);
    return items.map(([name, n]) => `
      <div class="bar-row">
        <div class="name" title="${esc(name)}">${esc(name)}</div>
        <div class="bar"><span style="width:${(n / max) * 100}%"></span></div>
        <div class="num">${fmtNum(n)}</div>
      </div>`).join('');
  }

  // -------------------------------------------------------------------------
  // router & navigation
  // -------------------------------------------------------------------------
  let currentView = '';
  const titles = {
    live: 'Live traffic',
    analytics: 'Analytics',
    logs: 'Request logs',
    refusals: 'Policy refusals',
    apis: 'APIs',
    approvals: 'Approvals',
    consumers: 'Consumers',
    subscriptions: 'Subscriptions',
    plans: 'Plans',
    fleet: 'Releases & fleet',
    audit: 'Audit log',
    team: 'Users & roles',
    tenants: 'Tenants & Workspaces',
    providers: 'OIDC Identity Providers',
    tests: 'API Test Studio',
    ai: 'AI Management & Governed Inference',
    apiops: 'APIOps & Release Passports'
  };
  const views = {};
  let viewTimer = null;

  async function route(soft = false) {
    const [, name = 'live', param] = location.hash.replace(/^#\/?/, '#/').split('/');
    const v = views[name] ? name : 'live';
    if (!soft) { clearInterval(viewTimer); viewTimer = null; }
    currentView = v;
    $('#page-title').textContent = titles[v] || 'Control Plane';
    $$('.rail-nav a').forEach((a) => a.classList.toggle('active', a.dataset.view === v));
    updateBadges();
    if (!soft) $('#view').innerHTML = '<div class="loading-state" role="status"><span class="loading-spinner" aria-hidden="true"></span>Loading workspace data…</div>';
    try {
      await views[v](param, soft);
    } catch (e) {
      if (e.message !== 'unauthorized') {
        $('#view').innerHTML = `<div class="card"><div class="form-error" role="alert"><strong>Unable to load this view</strong><p>${esc(e.message)}</p><button class="button" id="retry-view">Try again</button></div></div>`;
        $('#retry-view').onclick=()=>route();
      }
    }
  }

  async function updateBadges() {
    try {
      const [pending, apis, users] = await Promise.all([
        api('GET', '/api/subscriptions/pending').catch(() => null),
        api('GET', '/api/apis').catch(() => null),
        api('GET', '/api/admin/users').catch(() => null)
      ]);
      const pCount = (pending?.length || 0) + (users || []).filter(isSignupRequest).length;
      const bPending = $('#nav-pending-count');
      if (bPending) {
        bPending.textContent = pCount;
        bPending.hidden = pCount === 0;
      }
      const bApi = $('#nav-api-count');
      if (bApi) {bApi.hidden=!Array.isArray(apis);bApi.textContent=apis?.length || 0;}
    } catch (_) {}
  }

  window.addEventListener('hashchange', () => route());
  window.addEventListener('resize', () => { if (currentView === 'live' && live.last) renderLiveUpdate(live.last, true); });

  // -------------------------------------------------------------------------
  // VIEW: live traffic
  // -------------------------------------------------------------------------
  views.live = async () => {
    if (currentView !== 'live') return;
    $('#view').innerHTML = `
      <div id="orbit-signals"></div>
      <div class="grid kpis">
        <div class="card kpi"><div class="label">Requests / sec</div><div class="value" id="k-rps">0</div><div class="sub" id="k-rps-sub">60s avg 0</div></div>
        <div class="card kpi"><div class="label">Avg latency</div><div class="value" id="k-lat">–</div><div class="sub" id="k-p95">p95 –</div></div>
        <div class="card kpi"><div class="label">5xx Error Rate</div><div class="value" id="k-err">0%</div><div class="sub" id="k-4xx">4xx 0%</div></div>
        <div class="card kpi"><div class="label">Egress Bandwidth</div><div class="value" id="k-bytes">0 B</div><div class="sub">per second</div></div>
        <div class="card kpi"><div class="label">Total Proxied</div><div class="value" id="k-total">0</div><div class="sub" id="k-uptime">since startup</div></div>
      </div>
      <div class="grid two">
        <div class="card">
          <div class="card-header">
            <h2>Requests / sec <span class="legend"><span><i style="background:#235f4a"></i>total</span><span><i style="background:#d97706"></i>4xx</span><span><i style="background:#dc2626"></i>5xx</span></span></h2>
          </div>
          <canvas class="chart" id="c-rps"></canvas>
        </div>
        <div class="card">
          <div class="card-header">
            <h2>Latency percentiles <span class="legend"><span><i style="background:#0891b2"></i>avg</span><span><i style="background:#d97706"></i>p95</span></span></h2>
          </div>
          <canvas class="chart" id="c-lat"></canvas>
        </div>
      </div>
      <div class="grid three" style="margin-top:20px">
        <div class="card">
          <div class="card-header">
            <h2>Live request tail <span class="muted small" id="tail-count"></span></h2>
            <div class="muted small">Click any request to view decision trail</div>
          </div>
          <div class="table-wrap" style="max-height:480px;overflow:auto">
            <table>
              <thead><tr><th>Time</th><th>Method</th><th>Path</th><th>Status</th><th>Latency</th><th>API</th><th>Decision Reason</th></tr></thead>
              <tbody id="tail"></tbody>
            </table>
          </div>
        </div>
        <div class="grid" style="align-content:start">
          <div class="card"><h3>Traffic by API <span class="muted small">last 60s</span></h3><div id="by-api"></div></div>
          <div class="card"><h3>Status breakdown <span class="muted small">last 60s</span></h3><div id="by-status"></div></div>
          <div class="card"><h3>This node</h3><div id="gw-info" class="small"></div></div>
        </div>
      </div>
    `;
    renderTail(live.tail, true);
    if (live.last) renderLiveUpdate(live.last, true);
    else {
      drawChart($('#c-rps'), [{ values: [], color: '#235f4a' }]);
      drawChart($('#c-lat'), [{ values: [], color: '#0891b2' }]);
    }
    window.RelayOrbit?.renderSignals($('#orbit-signals'));
    const ov = await api('GET', '/api/overview');
    setConfigVersion(ov.gateway.config_version);
    renderGw(ov);
    viewTimer = setInterval(async () => {
      if (currentView === 'live' && !document.hidden) renderGw(await api('GET', '/api/overview').catch(() => ov));
    }, 15000);

    $('#tail').onclick = (e) => {
      const tr = e.target.closest('tr[data-req]');
      if (!tr) return;
      const r = live.tail.find((x) => x.request_id === tr.dataset.req);
      if (r) openDecisionModal(r);
    };
  };

  function renderGw(ov) {
    const el = $('#gw-info');
    if (!el) return;
    const c = ov.counts;
    el.innerHTML = `<table><tbody>
      <tr><td class="muted">Node ID</td><td><b>${esc(ov.node_id)}</b></td></tr>
      <tr><td class="muted">Cluster Revision</td><td><span class="tag green">rev_${String(ov.gateway.config_version).padStart(6, '0')}</span> (${ov.gateway.routes} routes)</td></tr>
      <tr><td class="muted">Enabled APIs</td><td>${c.enabled_apis} / ${c.apis} total</td></tr>
      <tr><td class="muted">Consumers</td><td>${c.consumers} (${c.active_keys} active keys)</td></tr>
      <tr><td class="muted">Active Subscriptions</td><td>${c.subscriptions}</td></tr>
      <tr><td class="muted">Node Uptime</td><td>${fmtUptime(ov.uptime_sec)}</td></tr></tbody></table>`;
  }

  function renderLiveUpdate(t, full = false) {
    const ticks = live.ticks;
    const last60 = ticks.slice(-60);
    const sum = (arr, f) => arr.reduce((a, x) => a + f(x), 0);
    const total60 = sum(last60, (x) => x.rps) || 0;
    $('#k-rps').textContent = fmtNum(t.rps);
    $('#k-rps-sub').textContent = `60s avg ${(total60 / Math.max(last60.length, 1)).toFixed(1)}`;
    $('#k-lat').textContent = t.rps ? fmtMs(t.avg_latency_ms) : '–';
    $('#k-p95').textContent = 'p95 ' + (t.rps ? fmtMs(t.p95_latency_ms) : '–');
    const e5 = sum(last60, (x) => x.status['5xx']), e4 = sum(last60, (x) => x.status['4xx']);
    $('#k-err').textContent = total60 ? ((e5 / total60) * 100).toFixed(1) + '%' : '0%';
    $('#k-err').style.color = total60 && e5 / total60 > 0.05 ? 'var(--red)' : '';
    $('#k-4xx').textContent = `4xx ${total60 ? ((e4 / total60) * 100).toFixed(1) : 0}% · 60s window`;
    $('#k-bytes').textContent = fmtBytes(t.bytes_out);
    $('#k-total').textContent = fmtNum(t.total_requests);
    $('#k-uptime').textContent = `up ${fmtUptime(t.uptime_sec)}${t.dropped ? ` · ${fmtNum(t.dropped)} dropped` : ''}`;

    drawChart($('#c-rps'), [
      { values: ticks.map((x) => x.rps), color: '#235f4a', fill: true },
      { values: ticks.map((x) => x.status['4xx']), color: '#d97706' },
      { values: ticks.map((x) => x.status['5xx']), color: '#dc2626' },
    ]);
    drawChart($('#c-lat'), [
      { values: ticks.map((x) => (x.rps ? x.avg_latency_ms : null)), color: '#0891b2', fill: true },
      { values: ticks.map((x) => (x.rps ? x.p95_latency_ms : null)), color: '#d97706' },
    ], { yFmt: (v) => v.toFixed(v < 10 ? 1 : 0) + 'ms' });

    const byApi = {};
    for (const x of last60) for (const [k, v] of Object.entries(x.by_api)) byApi[k] = (byApi[k] || 0) + v;
    $('#by-api').innerHTML = bars(Object.entries(byApi).sort((a, b) => b[1] - a[1]).slice(0, 8));
    const st = { '2xx': 0, '3xx': 0, '4xx': 0, '5xx': 0 };
    for (const x of last60) for (const k in st) st[k] += x.status[k];
    $('#by-status').innerHTML = bars(Object.entries(st));
    if (!full) renderTail(t.recent, false);
  }

  function tailRow(r, isNew) {
    const reasonTag = r.decision_reason
      ? `<span class="tag ${r.decision_reason === 'proxied_successfully' ? 'success' : 'warn'}">${esc(r.decision_reason)}</span>`
      : '<span class="muted">–</span>';
    return `
      <tr class="${isNew ? 'new-row' : ''}" data-req="${esc(r.request_id)}" style="cursor:pointer">
        <td class="mono muted">${fmtTime(r.ts)}</td>
        <td>${methodBadge(r.method)}</td>
        <td class="mono" title="${esc(r.path)}">${esc(r.path.length > 36 ? r.path.slice(0, 36) + '…' : r.path)}</td>
        <td>${statusPill(r.status)}</td>
        <td class="mono">${fmtMs(r.latency_ms)}</td>
        <td>${esc(r.api_name) || '<span class="muted">–</span>'}</td>
        <td>${reasonTag}</td>
      </tr>`;
  }

  function renderTail(rows, full) {
    const tb = $('#tail');
    if (!tb) return;
    if (full) {
      tb.innerHTML = live.tail.length
        ? live.tail.map((r) => tailRow(r, false)).join('')
        : '<tr><td colspan="7" class="empty">Waiting for traffic… send a request through the gateway.</td></tr>';
    } else if (rows.length) {
      if (tb.querySelector('.empty')) tb.innerHTML = '';
      tb.insertAdjacentHTML('afterbegin', [...rows].reverse().map((r) => tailRow(r, true)).join(''));
      while (tb.rows.length > 150) tb.deleteRow(-1);
    }
    $('#tail-count').textContent = live.tail.length ? `${live.tail.length} most recent` : '';
  }

  // -------------------------------------------------------------------------
  // VIEW: APIs
  // -------------------------------------------------------------------------
  let apiFilter = 'all';
  views.apis = async () => {
    const apis = await api('GET', '/api/apis');
    const draftsCount = apis.filter((a) => a.is_draft).length;
    const aiCount = apis.filter((a) => a.is_ai).length;

    if (currentView !== 'apis') return;
    $('#view').innerHTML = `
      <div class="toolbar">
        <div class="filter-group">
          <button class="filter ${apiFilter === 'all' ? 'active' : ''}" data-filter="all">All (${apis.length})</button>
          <button class="filter ${apiFilter === 'live' ? 'active' : ''}" data-filter="live">Live (${apis.filter((a) => a.enabled && !a.is_draft).length})</button>
          <button class="filter ${apiFilter === 'draft' ? 'active' : ''}" data-filter="draft">Drafts (${draftsCount})</button>
          ${previewAI() ? `<button class="filter ${apiFilter === 'ai' ? 'active' : ''}" data-filter="ai">AI Models (${aiCount})</button>` : ''}
        </div>
        <div class="search-box">
          <span class="muted"></span>
          <input id="api-search" placeholder="Filter APIs by name, path, upstream…">
        </div>
        <div class="spacer"></div>
        <button class="button" id="import-openapi">Import OpenAPI</button>
        <button class="button primary" id="new-api">New API</button>
      </div>

      <div class="card">
        <div class="table-wrap">
          <table id="apis-table">
            <thead>
              <tr>
                <th>API Name</th>
                <th>Gateway Route</th>
                <th>Upstream Backend</th>
                <th>Security & Governance</th>
                <th>Rate Limits & Quota</th>
                <th>State</th>
                <th>Operations</th>
              </tr>
            </thead>
            <tbody id="apis-body"></tbody>
          </table>
        </div>
      </div>
    `;

    function renderList() {
      const q = ($('#api-search')?.value || '').toLowerCase().trim();
      let filtered = apis.filter((a) => {
        if (apiFilter === 'draft' && !a.is_draft) return false;
        if (apiFilter === 'live' && (a.is_draft || !a.enabled)) return false;
        if (apiFilter === 'ai' && !a.is_ai) return false;
        if (q && !a.name.toLowerCase().includes(q) && !a.base_path.toLowerCase().includes(q) && !a.upstream_url.toLowerCase().includes(q)) return false;
        return true;
      });

      const tb = $('#apis-body');
      if (!tb) return;
      tb.innerHTML = filtered.length ? filtered.map((a) => `
        <tr>
          <td class="api-name-cell">
            <b class="api-name">${esc(a.name)}</b>
            ${a.description ? `<div class="muted small api-desc" title="${esc(a.description)}">${esc(a.description)}</div>` : ''}
            <div class="tag-row">
              ${a.is_ai ? '<span class="tag ai">AI model</span>' : ''}
              ${a.protocol && a.protocol !== 'http' ? `<span class="tag proto-${esc(a.protocol)}">${esc({ mcp: 'MCP', grpc: 'gRPC', graphql: 'GraphQL' }[a.protocol] || a.protocol)}</span>` : ''}
              ${(a.traffic_policy?.wasm_plugins || []).length ? '<span class="tag gray">WASM</span>' : ''}
              ${a.is_draft ? '<span class="tag draft">Draft</span>' : ''}
            </div>
          </td>
          <td class="mono nowrap">
            <b>${esc(a.base_path)}</b>
            ${a.strip_path ? '' : '<div><span class="tag gray small" title="The base path is kept when forwarding">no strip</span></div>'}
          </td>
          <td class="mono muted"><span class="truncate" title="${esc(a.upstream_url)}">${esc(a.upstream_url)}</span></td>
          <td>
            <div style="display:flex;flex-wrap:wrap;gap:4px">
              ${authPill(a.auth_type)}
              <span class="tag ${a.visibility === 'public' ? 'success' : 'gray'}">${esc(a.visibility || 'public')}</span>
              ${a.require_approval ? '<span class="tag warn">approval-required</span>' : ''}
              ${a.quota_failure_policy === 'fail_closed' ? '<span class="tag error">fail-closed</span>' : ''}
            </div>
          </td>
          <td>
            ${limitText(a.rate_limit_per_minute)}
            ${a.quota_per_day > 0 ? `<div class="muted small">${fmtNum(a.quota_per_day)} req/day</div>` : ''}
          </td>
          <td>
            ${a.is_draft
              ? '<span class="tag draft">draft staging</span>'
              : (a.enabled ? '<span class="tag success">serving</span>' : '<span class="tag gray">disabled</span>')}
          </td>
          <td class="actions">
            ${a.is_draft ? `<button class="button small primary" data-publish="${a.id}">Publish</button>` : ''}
            ${a.protocol === 'mcp' ? `<button class="button small" data-mcp="${a.id}" title="Approve the MCP tools this API exposes">MCP tools</button>` : ''}
            <button class="button small" data-policies="${a.id}" title="Protocol inspection (GraphQL, gRPC, MCP) and WASM plugins">Policies</button>
            <button class="button small" data-safety="${a.id}" title="Pre-release safety report and go/no-go gates">Safety report</button>
            <button class="button small" data-preview="${a.id}" title="Preview breaking changes against historical traffic">Preview</button>
            <button class="button small" data-edit="${a.id}">Edit</button>
            <button class="button small danger" data-del="${a.id}">Delete</button>
          </td>
        </tr>
      `).join('') : '<tr><td colspan="7" class="empty">No matching APIs found.</td></tr>';
    }

    renderList();

    $('#new-api').onclick = () => apiForm();
    $('#import-openapi').onclick = () => openAPIModal();
    $('#api-search').oninput = () => renderList();

    $$('.filter-group button').forEach((btn) => {
      btn.onclick = () => {
        $$('.filter-group button').forEach((b) => b.classList.remove('active'));
        btn.classList.add('active');
        apiFilter = btn.dataset.filter;
        renderList();
      };
    });

    $('#view').onclick = (e) => {
      const d = actionData(e);
      const id = d.edit || d.del || d.publish || d.preview || d.safety || d.mcp || d.policies;
      if (!id) return;
      const a = apis.find((x) => x.id === id);
      if (d.policies) openPoliciesModal(a);
      if (d.mcp) openMCPToolsModal(a);
      if (d.safety) openReleaseSafetyReportModal(a);
      if (d.edit) apiForm(a);
      if (d.publish) {
        api('POST', `/api/apis/${id}/publish`).then(() => {
          toast(`API <b>${esc(a.name)}</b> published live to cluster!`, 'success');
          route(true);
        }).catch((err) => toast(esc(err.message), 'error'));
      }
      if (d.preview) openImpactPreviewModal(a);
      if (d.del) {
        confirmDialog(
          'Delete API',
          `Permanently delete API <b>${esc(a.name)}</b>? Traffic routing to <code class="inline">${esc(a.base_path)}</code> will stop immediately.`,
          () => api('DELETE', `/api/apis/${id}`)
        );
      }
    };
  };

  // -------------------------------------------------------------------------
  // MCP tool catalog: discover the server's tools, review definitions and
  // approve (pin) them. Saving is an ordinary API update, so it creates a
  // revision and follows approvals and canaries.
  // -------------------------------------------------------------------------
  // -------------------------------------------------------------------------
  // Protocol & plugin policies: GraphQL, gRPC (descriptor upload), MCP link
  // and WASM plugin chain, edited together and saved as one API revision.
  // -------------------------------------------------------------------------
  async function openPoliciesModal(a) {
    const loaded = await api('GET', '/api/wasm/plugins').catch(() => ({ plugins: [] }));
    const gq = a.graphql_policy || {};
    const gp = a.grpc_policy || {};
    const tp = a.traffic_policy || {};
    let descriptor = null; // base64 of a newly uploaded descriptor set
    const lines = (arr) => (arr || []).join('\n');
    const num = (v) => (v === '' || v == null ? 0 : Number(v));
    const json = (s, what) => {
      if (!s.trim()) return undefined;
      try { return JSON.parse(s); } catch (e) { throw new Error(`${what} is not valid JSON: ${e.message}`); }
    };
    openModal({
      title: `Policies · ${a.name}`,
      large: true,
      body: `
        <div class="form-error" hidden></div>
        <div class="field"><label>Protocol</label>
          <select id="pp-protocol">
            ${['http', 'graphql', 'grpc', 'mcp'].map((p) => `<option value="${p}" ${(a.protocol || 'http') === p ? 'selected' : ''}>${{ http: 'HTTP / REST', graphql: 'GraphQL', grpc: 'gRPC', mcp: 'MCP (Model Context Protocol)' }[p]}</option>`).join('')}
          </select>
          <div class="help">The protocol decides which inspector runs on each request. Changing it publishes a new revision.</div></div>

        <fieldset data-proto="graphql" class="card" style="padding:16px;margin:12px 0">
          <legend><b>GraphQL</b> <a class="small" href="https://github.com/apatilgtn/RelayopsAPIM/blob/main/docs/GRAPHQL_GATEWAY.md" target="_blank" rel="noopener">guide</a></legend>
          <div class="field"><label>Schema (SDL)</label><textarea id="gq-schema" rows="6" class="mono">${esc(a.graphql_schema || '')}</textarea></div>
          <label class="check"><input type="checkbox" id="gq-validate" ${gq.validate_against_schema ? 'checked' : ''}> Validate every request against the schema</label>
          <div class="grid-2" style="display:grid;grid-template-columns:repeat(4,1fr);gap:8px;margin-top:8px">
            <div class="field"><label>Max depth</label><input id="gq-depth" type="number" min="0" value="${gq.max_depth || 0}"></div>
            <div class="field"><label>Max cost</label><input id="gq-cost" type="number" min="0" value="${gq.max_cost || 0}"></div>
            <div class="field"><label>Max aliases</label><input id="gq-aliases" type="number" min="0" value="${gq.max_aliases || 0}"></div>
            <div class="field"><label>Max batch</label><input id="gq-batch" type="number" min="0" max="100" value="${gq.max_batch_size || 0}"></div>
          </div>
          <div class="help">0 means unlimited (batching: refused). Cost multiplies list sizes such as <span class="mono">first: 100</span>.</div>
          <label class="check"><input type="checkbox" id="gq-mut" ${gq.allow_mutations ? 'checked' : ''}> Allow mutations</label>
          <label class="check"><input type="checkbox" id="gq-intro" ${gq.allow_introspection ? 'checked' : ''}> Allow introspection</label>
          <div class="field"><label>Operation allowlist (one per line: <span class="mono">sha256:&lt;hex&gt;</span> query hashes, or names)</label>
            <textarea id="gq-allow" rows="3" class="mono">${esc(lines(gq.operation_allowlist))}</textarea></div>
        </fieldset>

        <fieldset data-proto="grpc" class="card" style="padding:16px;margin:12px 0">
          <legend><b>gRPC</b></legend>
          <div class="field"><label>Descriptor set (<span class="mono">protoc --include_imports --descriptor_set_out=api.pb</span>)</label>
            <input id="gp-desc" type="file" accept=".pb,.desc,.protoset,application/octet-stream">
            <div id="gp-desc-info" class="help">${a.grpc_descriptor_set ? 'A descriptor set is loaded: methods outside it are refused with UNIMPLEMENTED.' : 'No descriptor set: any method is forwarded.'}</div>
            <div id="gp-methods"></div></div>
          <div class="grid-2" style="display:grid;grid-template-columns:1fr 1fr;gap:8px">
            <div class="field"><label>Max message size (bytes, both directions)</label><input id="gp-max" type="number" min="0" value="${gp.max_message_size_bytes || 0}"><div class="help">0 = 4 MiB.</div></div>
            <div class="field"><label>Default for methods no rule matches</label><select id="gp-default">
              <option value="allow" ${gp.default_action !== 'deny' ? 'selected' : ''}>Allow</option>
              <option value="deny" ${gp.default_action === 'deny' ? 'selected' : ''}>Deny</option></select></div>
          </div>
          <label class="check"><input type="checkbox" id="gp-refl" ${gp.allow_reflection ? 'checked' : ''}> Allow server reflection</label>
          <label class="check"><input type="checkbox" id="gp-json" ${gp.json_transcoding ? 'checked' : ''}> JSON transcoding (HTTP clients POST JSON to <span class="mono">/package.Service/Method</span>; needs the descriptor set)</label>
          <div class="help">Browsers can call this API with gRPC-Web (binary or text) without any setting; enable CORS on the API for cross-origin pages.</div>
          <div class="field"><label>Method rules (JSON)</label>
            <textarea id="gp-rules" rows="4" class="mono" placeholder='[{"methods":["pkg.Service/Delete*"],"plans":["Enterprise"],"action":"allow"}]'>${esc(gp.rules ? JSON.stringify(gp.rules, null, 2) : '')}</textarea></div>
        </fieldset>

        <fieldset data-proto="mcp" class="card" style="padding:16px;margin:12px 0">
          <legend><b>MCP</b></legend>
          <p class="muted">Tool, prompt and resource governance is managed in the MCP tools dialog: discover, approve, rules and argument validation.</p>
          <button class="button small" id="pp-open-mcp" type="button">Open MCP tools</button>
        </fieldset>

        <fieldset class="card" style="padding:16px;margin:12px 0">
          <legend><b>WASM plugins</b> <a class="small" href="https://github.com/apatilgtn/RelayopsAPIM/blob/main/docs/WASM_PLUGIN_SDK.md" target="_blank" rel="noopener">SDK guide</a></legend>
          <div class="help">Loaded on this node (${esc(loaded.node || '')}): ${(loaded.plugins || []).length ? loaded.plugins.map((p) => `<span class="tag gray mono">${esc(p)}</span>`).join(' ') : '<i>none</i>'}. A gateway without a listed plugin refuses the API's requests (503).</div>
          <div class="field"><label>Plugin chain (in order, comma-separated)</label><input id="wp-list" class="mono" value="${esc((tp.wasm_plugins || []).join(', '))}"></div>
          <div class="grid-2" style="display:grid;grid-template-columns:1fr 1fr;gap:8px">
            <div class="field"><label>Request body for plugins (bytes, 0 = none, max 1048576)</label><input id="wp-body" type="number" min="0" max="1048576" value="${tp.wasm_body_limit_bytes || 0}"></div>
            <div class="field"><label>Response body for plugins (bytes, 0 = none, max 4194304)</label><input id="wp-resp" type="number" min="0" max="4194304" value="${tp.wasm_response_body_limit_bytes || 0}"></div>
          </div>
          <div class="field"><label>Plugin settings (JSON: plugin name → settings object)</label>
            <textarea id="wp-config" rows="5" class="mono" placeholder='{"ip-allowlist": {"allow": ["10.0.0.0/8"]}}'>${esc(tp.wasm_config ? JSON.stringify(tp.wasm_config, null, 2) : '')}</textarea></div>
        </fieldset>`,
      actions: [{
        label: 'Save policies', primary: true,
        onClick: async (close, wrap) => {
          const err = $('.form-error', wrap);
          err.hidden = true;
          try {
            const protocol = $('#pp-protocol', wrap).value;
            const plugins = $('#wp-list', wrap).value.split(',').map((s) => s.trim()).filter(Boolean);
            const body = {
              protocol,
              traffic_policy: { ...tp, wasm_plugins: plugins, wasm_body_limit_bytes: num($('#wp-body', wrap).value),
                wasm_response_body_limit_bytes: num($('#wp-resp', wrap).value),
                wasm_config: json($('#wp-config', wrap).value, 'Plugin settings') }
            };
            if (protocol === 'graphql') {
              body.graphql_schema = $('#gq-schema', wrap).value;
              body.graphql_policy = {
                max_depth: num($('#gq-depth', wrap).value), max_cost: num($('#gq-cost', wrap).value),
                max_aliases: num($('#gq-aliases', wrap).value), max_batch_size: num($('#gq-batch', wrap).value),
                allow_mutations: $('#gq-mut', wrap).checked, allow_introspection: $('#gq-intro', wrap).checked,
                validate_against_schema: $('#gq-validate', wrap).checked,
                operation_allowlist: $('#gq-allow', wrap).value.split('\n').map((s) => s.trim()).filter(Boolean),
                list_size_arguments: gq.list_size_arguments
              };
            }
            if (protocol === 'grpc') {
              body.grpc_policy = {
                max_message_size_bytes: num($('#gp-max', wrap).value), allow_reflection: $('#gp-refl', wrap).checked,
                default_action: $('#gp-default', wrap).value, rules: json($('#gp-rules', wrap).value, 'Method rules') || [],
                json_transcoding: $('#gp-json', wrap).checked
              };
              if (descriptor) body.grpc_descriptor_set = descriptor;
            }
            await api('PUT', `/api/apis/${a.id}`, body);
            toast(`Policies saved for <b>${esc(a.name)}</b>; a new revision is rolling out`, 'success');
            close();
            route(true);
          } catch (e) { err.textContent = e.message || String(e); err.hidden = false; }
        }
      }],
      onOpen: (wrap) => {
        const show = () => {
          const p = $('#pp-protocol', wrap).value;
          $$('[data-proto]', wrap).forEach((f) => { f.hidden = f.dataset.proto !== p; });
        };
        $('#pp-protocol', wrap).onchange = show;
        show();
        $('#pp-open-mcp', wrap).onclick = () => openMCPToolsModal(a);
        $('#gp-desc', wrap).onchange = async (e) => {
          const file = e.target.files[0];
          const info = $('#gp-desc-info', wrap);
          const out = $('#gp-methods', wrap);
          descriptor = null;
          out.innerHTML = '';
          if (!file) return;
          const bytes = new Uint8Array(await file.arrayBuffer());
          let bin = '';
          for (let i = 0; i < bytes.length; i += 0x8000) bin += String.fromCharCode.apply(null, bytes.subarray(i, i + 0x8000));
          const b64 = btoa(bin);
          try {
            const res = await api('POST', '/api/grpc/descriptor/inspect', { descriptor_set: b64 });
            descriptor = b64;
            info.textContent = `${file.name}: ${res.methods.length} method(s). Saved with the policies.`;
            out.innerHTML = `<div class="table-wrap" style="max-height:180px;overflow:auto"><table><tbody>${res.methods.map((m) => `<tr><td class="mono small">${esc(m.name)}</td><td><span class="tag gray">${esc(m.kind)}</span></td></tr>`).join('')}</tbody></table></div>`;
          } catch (er) {
            info.innerHTML = `<span class="form-error">${esc(er.message)}</span>`;
          }
        };
      }
    });
  }

  // Review one held MCP definition: approved version beside the new one.
  async function openMCPReviewModal(entry) {
    let approved = null;
    if (entry.pinned) {
      const history = await api('GET', `/api/mcp/catalog?api_id=${encodeURIComponent(entry.api_id)}&status=approved`).catch(() => []);
      approved = history.find((h) => h.kind === entry.kind && h.name === entry.name && h.fingerprint === entry.pinned) || null;
    }
    const pretty = (d) => esc(JSON.stringify(d, null, 2));
    const decide = (action, label) => async (close) => {
      try {
        const res = await api('POST', `/api/mcp/catalog/${entry.id}/${action}`);
        toast(`${label}: ${esc(entry.kind)} <b>${esc(entry.name)}</b>${res.revision ? ` (revision ${res.revision})` : ''}`, action === 'approve' ? 'success' : 'warn');
        close();
        route(true);
      } catch (e) { toast(esc(e.message), 'error'); }
    };
    openModal({
      title: `Review MCP ${entry.kind} · ${entry.name}`,
      large: true,
      body: `
        <p class="muted">${esc(entry.api_name)} · first seen ${esc(fmtDate(entry.first_seen))} by ${esc(entry.seen_by || 'gateway')} · <span class="mono small">${esc(entry.fingerprint)}</span></p>
        <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px">
          <div><h3 style="font-size:14px">Approved</h3>${approved
            ? `<pre class="mono small" style="white-space:pre-wrap;max-height:360px;overflow:auto">${pretty(approved.definition)}</pre>`
            : `<div class="empty small">${entry.pinned ? 'The approved definition was not recorded (approved before the catalog existed).' : 'Not approved before: this is a new ' + esc(entry.kind) + '.'}</div>`}</div>
          <div><h3 style="font-size:14px">Now offered by the server</h3>
            <pre class="mono small" style="white-space:pre-wrap;max-height:360px;overflow:auto">${pretty(entry.definition)}</pre></div>
        </div>
        <div class="callout warning" style="margin-top:12px">Approving publishes a new API revision that pins this definition. <b>Keep blocked</b> records the decision and keeps refusing it; <b>Resolved</b> clears the hold when the server has gone back to the approved version.</div>`,
      actions: [
        { label: 'Resolved', onClick: decide('resolve', 'Marked resolved') },
        { label: 'Keep blocked', danger: true, onClick: decide('reject', 'Kept blocked') },
        { label: 'Approve', primary: true, onClick: decide('approve', 'Approved') }
      ]
    });
  }

  function openMCPToolsModal(a) {
    const policy = a.mcp_policy || {};
    const pins = { ...(policy.pinned_tools || {}) };
    const promptPins = { ...(policy.pinned_prompts || {}) };
    const rest = { ...policy };
    delete rest.pinned_tools;
    delete rest.pinned_prompts;
    const statusTag = (s) => ({
      pinned: '<span class="tag success">approved</span>',
      changed: '<span class="tag error">changed since approval</span>',
      new: '<span class="tag warn">not approved</span>',
      removed: '<span class="tag gray">no longer offered</span>'
    }[s] || esc(s));
    openModal({
      title: `MCP tools · ${a.name}`,
      large: true,
      body: `
        <p class="muted">The gateway only exposes approved tools. A tool whose definition changes is hidden and refused until it is approved again. Approving saves a new API revision.</p>
        <div style="display:flex;gap:8px;align-items:flex-end;margin-bottom:12px">
          <div class="field" style="flex:1;margin:0"><label>MCP endpoint below the upstream URL (optional)</label>
            <input id="mcp-path" type="text" placeholder="/mcp" class="mono"></div>
          <button class="button" id="mcp-discover">Discover tools</button>
        </div>
        <div id="mcp-catalog"><div class="empty">${Object.keys(pins).length} tool(s) and ${Object.keys(promptPins).length} prompt(s) approved. Discover to compare with what the server offers now.</div></div>
        <details style="margin-top:12px"><summary>Access rules and limits (JSON)</summary>
          <textarea id="mcp-policy" rows="8" class="mono" style="width:100%">${esc(JSON.stringify(rest, null, 2))}</textarea>
          <div class="help">default_action (allow/deny), rules [{tools, consumers, plans, action}], resource_rules / prompt_rules [{match, consumers, plans, action}], validate_arguments, tool_calls_per_minute, max_body_bytes.</div>
        </details>
        <div class="form-error" hidden></div>`,
      actions: [{
        label: 'Save approvals', primary: true,
        onClick: async (close, wrap) => {
          const err = $('.form-error', wrap);
          err.hidden = true;
          try {
            const next = JSON.parse($('#mcp-policy', wrap).value || '{}');
            const boxes = $$('[data-pin]', wrap);
            next.pinned_tools = pins;
            next.pinned_prompts = promptPins;
            if (boxes.length) {
              const picked = { tool: {}, prompt: {} };
              boxes.filter((c) => c.checked).forEach((c) => { picked[c.dataset.kind][c.dataset.pin] = c.dataset.fp; });
              next.pinned_tools = picked.tool;
              // Prompts are only pinned when the server offers some (or some were pinned).
              if (boxes.some((c) => c.dataset.kind === 'prompt')) next.pinned_prompts = picked.prompt;
            }
            await api('PUT', `/api/apis/${a.id}`, { mcp_policy: next });
            toast(`MCP policy saved for <b>${esc(a.name)}</b> (${Object.keys(next.pinned_tools).length} tool(s), ${Object.keys(next.pinned_prompts || {}).length} prompt(s) approved)`, 'success');
            close();
            route(true);
          } catch (e) { err.textContent = e.message || String(e); err.hidden = false; }
        }
      }],
      onOpen: (wrap) => {
        $('#mcp-discover', wrap).onclick = async () => {
          const btn = $('#mcp-discover', wrap);
          const out = $('#mcp-catalog', wrap);
          btn.disabled = true;
          out.innerHTML = '<div class="empty">Connecting to the MCP server…</div>';
          try {
            const path = $('#mcp-path', wrap).value.trim();
            const res = await api('POST', `/api/apis/${a.id}/mcp/discover`, path ? { path } : {});
            const rows = res.tools.concat(res.removed.map((r) => ({ ...r, status: 'removed' })));
            out.innerHTML = `
              <div class="table-wrap"><table>
                <thead><tr><th>Approve</th><th>Tool</th><th>Status</th><th>Definition</th></tr></thead>
                <tbody>${rows.map((t) => `<tr>
                  <td><input type="checkbox" aria-label="Approve ${esc(t.name)}" data-kind="${esc(t.kind || 'tool')}" data-pin="${esc(t.name)}" data-fp="${esc(t.fingerprint)}" ${t.status === 'pinned' || t.status === 'removed' ? 'checked' : ''}></td>
                  <td>${t.kind === 'prompt' ? '<span class="tag gray">prompt</span> ' : ''}<b class="mono">${esc(t.name)}</b>${t.title ? `<div class="muted small">${esc(t.title)}</div>` : ''}</td>
                  <td>${statusTag(t.status)}</td>
                  <td style="max-width:460px">${t.definition
                    ? `<div class="small">${esc((t.description || '').slice(0, 240))}</div>
                       <details><summary class="small muted">Full definition · ${esc(t.fingerprint.slice(0, 19))}…</summary>
                       <pre class="mono small" style="white-space:pre-wrap">${esc(JSON.stringify(t.definition, null, 2))}</pre></details>`
                    : '<span class="muted small">kept until unchecked</span>'}</td>
                </tr>`).join('') || '<tr><td colspan="4" class="empty">The server lists no tools.</td></tr>'}</tbody>
              </table></div>
              <p class="muted small">Endpoint: <span class="mono">${esc(res.endpoint)}</span>. Read a changed tool's full definition before approving it again.</p>
              ${(res.warnings || []).map((w) => `<div class="callout warning small">${esc(w)}</div>`).join('')}`;
          } catch (e) {
            out.innerHTML = `<div class="form-error">${esc(e.message || String(e))}</div>`;
          } finally { btn.disabled = false; }
        };
      }
    });
  }

  // -------------------------------------------------------------------------
  // SIGNATURE FEATURE 1: Change impact preview Modal
  // -------------------------------------------------------------------------
  function openImpactPreviewModal(a) {
    openModal({
      title: `Change impact preview · ${a.name}`,
      large: true,
      body: `
        <div class="callout">
          <b>Safe API Operations:</b> RelayOps analyzes request telemetry over the last 24 hours to simulate how proposed route or authentication changes will affect consumers before you publish.
        </div>
        <div class="form-row">
          <div class="field">
            <label>Proposed Authentication Scheme</label>
            <select id="prev-auth">
              <option value="none" ${a.auth_type === 'none' ? 'selected' : ''}>None (open)</option>
              <option value="api_key" ${a.auth_type === 'api_key' ? 'selected' : ''}>API key + subscription</option>
              <option value="jwt" ${a.auth_type === 'jwt' ? 'selected' : ''}>Strict JWT (exp required)</option>
              <option value="oidc" ${a.auth_type === 'oidc' ? 'selected' : ''}>OIDC / JWKS</option>
            </select>
          </div>
          <div class="field">
            <label>Proposed Base Path</label>
            <input id="prev-path" value="${esc(a.base_path)}">
          </div>
        </div>
        <div class="field">
          <label>Proposed Rate Limit (requests/minute)</label>
          <input id="prev-limit" type="number" value="${a.rate_limit_per_minute || 0}">
        </div>

        <div style="display:flex;gap:8px;margin-top:10px;flex-wrap:wrap">
          <button class="button primary small" id="run-preview-btn">Analyze Traffic Impact</button>
          <button class="button small" id="run-safety-btn">Safety report</button>
          <button class="button small" id="run-impact-btn">Consumer Impact Report</button>
          <button class="button small" id="run-replay-btn">Run Sanitized Traffic Replay</button>
        </div>

        <div id="preview-result" style="margin-top:20px" hidden>
          <div id="prev-severity"></div>
          <div class="impact-summary">
            <div class="impact-box"><div class="lbl">Requests Evaluated</div><div class="num" id="prev-reqs">0</div></div>
            <div class="impact-box"><div class="lbl">Distinct Callers Impacted</div><div class="num" id="prev-consumers">0</div></div>
            <div class="impact-box"><div class="lbl">Breaking Changes</div><div class="num" id="prev-breaking" style="color:var(--red)">0</div></div>
          </div>
          <div id="prev-confidence" class="muted small" style="margin-bottom:12px"></div>
          <div id="prev-warnings"></div>
          <div id="prev-impact-box" style="margin-top:16px" hidden></div>
          <div id="prev-consumers-table" style="margin-top:14px"></div>
          <div id="prev-replay-box" style="margin-top:16px" hidden></div>
        </div>
      `,
      actions: [{
        label: 'Apply change',
        primary: true,
        onClick: async (close, wrap, btn) => {
          const auth = $('#prev-auth', wrap).value;
          const basePath = $('#prev-path', wrap).value.trim();
          const limit = Number($('#prev-limit', wrap).value);
          btn.disabled = true;
          try {
            await api('PUT', `/api/apis/${a.id}`, { auth_type: auth, base_path: basePath, rate_limit_per_minute: limit });
            toast(`Configuration updated and fleet notified!`, 'success');
            close();
            route(true);
          } catch (err) {
            toast(esc(err.message), 'error');
          } finally {
            btn.disabled = false;
          }
        }
      }],
      onOpen: (wrap) => {
        const runBtn = $('#run-preview-btn', wrap);
        const safetyBtn = $('#run-safety-btn', wrap);
        const impactBtn = $('#run-impact-btn', wrap);
        const replayBtn = $('#run-replay-btn', wrap);

        const getProposed = () => ({
          auth_type: $('#prev-auth', wrap).value,
          base_path: $('#prev-path', wrap).value.trim(),
          rate_limit_per_minute: Number($('#prev-limit', wrap).value)
        });

        if (safetyBtn) {
          safetyBtn.onclick = () => {
            openReleaseSafetyReportModal(a, getProposed());
          };
        }

        impactBtn.onclick = async () => {
          impactBtn.disabled = true;
          impactBtn.textContent = 'Generating Report…';
          try {
            const imp = await api('POST', `/api/apis/${a.id}/consumer-impact`, getProposed());
            $('#preview-result', wrap).hidden = false;
            const ibox = $('#prev-impact-box', wrap);
            ibox.hidden = false;
            const apps = imp.applications || [];
            const bChanges = imp.breaking_changes || [];
            ibox.innerHTML = `
              <div class="card" style="padding:14px;border-left:4px solid var(--amber)">
                <div class="card-header">
                  <h3 style="font-size:14px">Consumer Application Impact Report</h3>
                  <span class="tag ${imp.breaking_changes_count > 0 ? 'error' : 'success'}">${imp.breaking_changes_count} breaking changes · ${imp.total_impacted_applications} applications</span>
                </div>
                <div style="margin:8px 0;font-size:13px;line-height:1.4"><b>Actionable Summary:</b> ${esc(imp.actionable_summary)}</div>
                ${bChanges.length ? `
                  <div style="margin-bottom:12px">
                    <div style="font-size:12px;font-weight:600;margin-bottom:4px;color:var(--red)">Detected Breaking Changes:</div>
                    <ul style="margin:0;padding-left:18px;font-size:12px">
                      ${bChanges.map(b => `<li style="color:var(--red)">${esc(b)}</li>`).join('')}
                    </ul>
                  </div>` : ''}
                <div class="table-wrap" style="max-height:280px;overflow:auto;margin-top:8px">
                  <table>
                    <thead>
                      <tr>
                        <th>Application</th>
                        <th>Owner Email</th>
                        <th>Environment</th>
                        <th>Activity Status</th>
                        <th>Failure Mode</th>
                        <th>Required Remediation Action</th>
                      </tr>
                    </thead>
                    <tbody>
                      ${apps.map(ap => `
                        <tr style="${ap.impact_level === 'CRITICAL_BREAKING' ? 'background:#fff1f2' : ''}">
                          <td><b>${esc(ap.app_name)}</b></td>
                          <td class="muted">${esc(ap.consumer_email)}</td>
                          <td><span class="tag gray small">${esc(ap.environment)}</span></td>
                          <td><span class="tag ${ap.activity_status === 'OBSERVED_ACTIVE_CALLER' ? 'green' : 'gray'} small">${esc(ap.activity_status)}</span></td>
                          <td class="small" style="color:var(--red)">${esc(ap.failure_mode)}</td>
                          <td class="small"><b>${esc(ap.required_action)}</b></td>
                        </tr>
                      `).join('') || '<tr><td colspan="6" class="empty">No subscribed client applications on record.</td></tr>'}
                    </tbody>
                  </table>
                </div>
              </div>`;
          } catch (err) {
            toast(esc(err.message), 'error');
          } finally {
            impactBtn.disabled = false;
            impactBtn.textContent = ' Consumer Impact Report';
          }
        };

        runBtn.onclick = async () => {
          runBtn.disabled = true;
          runBtn.textContent = 'Evaluating…';
          try {
            const r = await api('POST', `/api/apis/${a.id}/preview-change`, getProposed());
            $('#preview-result', wrap).hidden = false;
            $('#prev-reqs', wrap).textContent = fmtNum(r.recent_requests || r.requests_evaluated || 0);
            $('#prev-consumers', wrap).textContent = fmtNum(r.active_consumers || (r.impacted_consumers || []).length);
            
            const breakingCount = (r.impacts || []).filter(x => x.severity === 'critical' || x.severity === 'warning').length;
            $('#prev-breaking', wrap).textContent = fmtNum(breakingCount);

            $('#prev-confidence', wrap).textContent = `Sample window: ${r.sample_window || 'Recent 24h'} · ${r.confidence || 'Evaluated against live production traffic'}`;

            const impacts = r.impacts || [];
            const hasCrit = impacts.some(x => x.severity === 'critical');
            $('#prev-severity', wrap).innerHTML = `
              <div class="callout ${hasCrit ? 'danger' : breakingCount ? 'warning' : 'success'}">
                <b>${hasCrit ? 'CRITICAL OPERATIONAL RISK' : breakingCount ? 'WARNING' : 'SAFE FOR PRODUCTION'}</b>:
                ${impacts.map(i => esc(i.message)).join(' ')}
              </div>`;

            const consumers = r.impacted_consumers || [];
            if (consumers.length > 0) {
              $('#prev-consumers-table', wrap).innerHTML = `
                <div class="card" style="padding:10px">
                  <div class="card-header"><h3 style="font-size:13px">Specifically Impacted Consumers</h3></div>
                  <table>
                    <thead><tr><th>Consumer Name</th><th>Email</th><th>Observed Requests</th></tr></thead>
                    <tbody>
                      ${consumers.map(c => `
                        <tr>
                          <td><b>${esc(c.name)}</b></td>
                          <td class="muted">${esc(c.email || '–')}</td>
                          <td class="mono">${fmtNum(c.request_count)} reqs</td>
                        </tr>
                      `).join('')}
                    </tbody>
                  </table>
                </div>`;
            } else {
              $('#prev-consumers-table', wrap).innerHTML = '';
            }
          } catch (err) {
            toast(esc(err.message), 'error');
          } finally {
            runBtn.disabled = false;
            runBtn.textContent = 'Analyze Traffic Impact';
          }
        };

        replayBtn.onclick = async () => {
          replayBtn.disabled = true;
          replayBtn.textContent = 'Simulating Replay…';
          try {
            const rep = await api('POST', `/api/apis/${a.id}/replay-preview`, getProposed());
            $('#preview-result', wrap).hidden = false;
            const rbox = $('#prev-replay-box', wrap);
            rbox.hidden = false;
            const sims = rep.simulations || [];
            rbox.innerHTML = `
              <div class="card" style="padding:12px;background:#f8fafc">
                <div class="card-header">
                  <h3 style="font-size:13px">Sanitized Traffic Replay Simulation (${fmtNum(rep.total_replayed)} requests)</h3>
                  <span class="tag ${rep.status_changed_count > 0 ? 'warn' : 'success'}">${rep.status_changed_count} status diffs detected</span>
                </div>
                <div class="table-wrap" style="max-height:260px;overflow:auto">
                  <table>
                    <thead><tr><th>Path</th><th>Original</th><th>Simulated</th><th>Diff</th><th>Behavioral Explanation</th></tr></thead>
                    <tbody>
                      ${sims.slice(0, 50).map(s => `
                        <tr style="${s.behavioral_diff ? 'background:#fff1f2' : ''}">
                          <td class="mono small">${esc(s.path)}</td>
                          <td>${statusPill(s.original_status)}</td>
                          <td>${statusPill(s.simulated_status)}</td>
                          <td>${s.behavioral_diff ? '<span class="tag error small">DIFF</span>' : '<span class="tag gray small">MATCH</span>'}</td>
                          <td class="small ${s.behavioral_diff ? 'mono' : 'muted'}">${esc(s.reason)}</td>
                        </tr>
                      `).join('') || '<tr><td colspan="5" class="empty">No requests to replay</td></tr>'}
                    </tbody>
                  </table>
                </div>
              </div>`;
          } catch (err) {
            toast(esc(err.message), 'error');
          } finally {
            replayBtn.disabled = false;
            replayBtn.textContent = ' Run Sanitized Traffic Replay';
          }
        };

        runBtn.click();
      }
    });
  }

  // -------------------------------------------------------------------------
  // SIGNATURE FEATURE 2: Unified Release safety report & Go/No-Go Gates
  // -------------------------------------------------------------------------
  async function openReleaseSafetyReportModal(a, proposed = null) {
    let rep = null;
    let fetchErr = null;
    try {
      if (proposed && Object.keys(proposed).length > 0) {
        rep = await api('POST', `/api/apis/${a.id}/release-safety-report`, proposed);
      } else {
        rep = await api('GET', `/api/apis/${a.id}/release-safety-report`);
      }
    } catch (err) {
      fetchErr = err;
    }

    if (fetchErr || !rep) {
      toast(fetchErr ? esc(fetchErr.message) : 'Failed to generate release safety report', 'error');
      return;
    }

    const gates = rep.release_gates || {};
    const gateList = gates.gates || [];
    const limitations = rep.simulation_limitations || {};
    const sampling = rep.sampling_coverage || {};
    const impact = rep.consumer_impact || {};
    const apps = impact.applications || [];

    const verdict = gates.verdict || 'UNKNOWN';
    const verdictClass = verdict === 'PASSED' ? 'success' : verdict === 'WARNING_GATED' ? 'warn' : 'error';
    const verdictIcon = verdict === 'PASSED' ? '✓' : verdict === 'WARNING_GATED' ? '⚠' : '⛔';

    openModal({
      title: `Release safety report · ${esc(a.name)}`,
      large: true,
      body: `
        <div style="display:flex;align-items:center;justify-content:space-between;padding:14px;background:#f8fafc;border-radius:8px;border:1px solid #e2e8f0;margin-bottom:16px">
          <div>
            <div style="font-size:12px;text-transform:uppercase;color:var(--text-muted);font-weight:600;letter-spacing:0.5px">Release Gate Verdict</div>
            <div style="font-size:18px;font-weight:700;display:flex;align-items:center;gap:8px;margin-top:2px">
              <span class="tag ${verdictClass}" style="font-size:13px;padding:3px 8px">${verdictIcon} ${esc(verdict)}</span>
              <span style="font-size:14px;font-weight:500;color:var(--text-main)">${esc(gates.summary || rep.summary)}</span>
            </div>
          </div>
          <div class="muted small" style="text-align:right">
            <div>Target Revision: <b>v${rep.target_revision || 'proposed'}</b></div>
            <div>Generated: ${fmtDate(rep.generated_at)}</div>
          </div>
        </div>

        <!-- 1. Explicit Release Gates -->
        <div class="card" style="padding:14px;margin-bottom:16px">
          <div class="card-header">
            <h3 style="font-size:14px;margin:0">1. Explicit Release Gates (Go/No-Go)</h3>
            <span class="muted small">${gateList.filter(g => g.status === 'PASSED').length}/${gateList.length} Gates Passed</span>
          </div>
          <div class="table-wrap" style="margin-top:8px">
            <table>
              <thead>
                <tr>
                  <th>Gate Name</th>
                  <th>Category</th>
                  <th>Status</th>
                  <th>Enforcement Rationale & Thresholds</th>
                </tr>
              </thead>
              <tbody>
                ${gateList.map(g => `
                  <tr>
                    <td><b>${esc(g.gate_name)}</b></td>
                    <td><span class="tag gray small">${esc(g.category)}</span></td>
                    <td><span class="tag ${g.status === 'PASSED' ? 'success' : g.status === 'WARNING' ? 'warn' : 'error'} small">${esc(g.status)}</span></td>
                    <td class="small">${esc(g.reason)}</td>
                  </tr>
                `).join('')}
              </tbody>
            </table>
          </div>
        </div>

        <!-- 2. Simulation Limitations & Sample Coverage -->
        <div class="grid two" style="gap:16px;margin-bottom:16px">
          <div class="card" style="padding:14px">
            <div class="card-header"><h3 style="font-size:14px;margin:0">2. Statistical Sample Coverage</h3></div>
            <div class="impact-summary" style="margin-top:8px">
              <div class="impact-box"><div class="lbl">Total Logs</div><div class="num">${fmtNum(sampling.total_historical_logs || 0)}</div></div>
              <div class="impact-box"><div class="lbl">Evaluated</div><div class="num">${fmtNum(sampling.analyzed_requests || 0)}</div></div>
              <div class="impact-box"><div class="lbl">App Coverage</div><div class="num" style="color:var(--green)">${sampling.consumer_coverage_ratio ? (sampling.consumer_coverage_ratio * 100).toFixed(0) + '%' : '100%'}</div></div>
            </div>
            <div style="font-size:12px;color:var(--text-muted);margin-top:8px;line-height:1.4">
              <div><b>Confidence Interval:</b> ${esc(sampling.confidence_interval || '95%')} (Margin of error: ±${(sampling.margin_of_error_pct || 0).toFixed(1)}%)</div>
              <div><b>Observation Window:</b> ${sampling.observation_window_hours || 24} hours historical telemetry</div>
            </div>
          </div>

          <div class="card" style="padding:14px;background:#fefce8;border:1px solid #fef08a">
            <div class="card-header"><h3 style="font-size:14px;margin:0;color:#854d0e">3. Simulation Limitations Disclaimer</h3></div>
            <p style="font-size:12px;color:#713f12;margin:6px 0 8px 0;line-height:1.4">${esc(limitations.limitations_disclaimer || '')}</p>
            <ul style="margin:0;padding-left:16px;font-size:12px;color:#854d0e">
              ${(limitations.items || []).map(item => `<li>${esc(item)}</li>`).join('')}
            </ul>
          </div>
        </div>

        <!-- 4. Consumer Impact Matrix -->
        <div class="card" style="padding:14px">
          <div class="card-header">
            <h3 style="font-size:14px;margin:0">4. Consumer Impact & Remediation Matrix</h3>
            <span class="tag ${impact.breaking_changes_count > 0 ? 'error' : 'success'}">${impact.breaking_changes_count || 0} breaking changes · ${impact.total_impacted_applications || 0} impacted apps</span>
          </div>
          <div style="margin:8px 0;font-size:12px"><b>Actionable Summary:</b> ${esc(impact.actionable_summary || 'No breaking changes detected.')}</div>
          <div class="table-wrap" style="max-height:220px;overflow:auto;margin-top:8px">
            <table>
              <thead>
                <tr>
                  <th>Application</th>
                  <th>Owner</th>
                  <th>Environment</th>
                  <th>Status</th>
                  <th>Failure Mode</th>
                  <th>Required Remediation</th>
                </tr>
              </thead>
              <tbody>
                ${apps.map(ap => `
                  <tr style="${ap.impact_level === 'CRITICAL_BREAKING' ? 'background:#fff1f2' : ''}">
                    <td><b>${esc(ap.app_name)}</b></td>
                    <td class="muted">${esc(ap.consumer_email)}</td>
                    <td><span class="tag gray small">${esc(ap.environment)}</span></td>
                    <td><span class="tag ${ap.activity_status === 'OBSERVED_ACTIVE_CALLER' ? 'green' : 'gray'} small">${esc(ap.activity_status)}</span></td>
                    <td class="small" style="color:var(--red)">${esc(ap.failure_mode)}</td>
                    <td class="small"><b>${esc(ap.required_action)}</b></td>
                  </tr>
                `).join('') || '<tr><td colspan="6" class="empty">No subscribed client applications affected.</td></tr>'}
              </tbody>
            </table>
          </div>
        </div>
      `,
      actions: [{
        label: 'Close',
        primary: false,
        onClick: (close) => close()
      }]
    });
  }

  // -------------------------------------------------------------------------
  // SIGNATURE FEATURE 3: Request Decision Explorer Modal
  // -------------------------------------------------------------------------
  // Plain wording for common decision reasons; anything else is humanised.
  const reasonText = {
    proxied_successfully: 'Allowed and forwarded to the upstream service.',
    missing_api_key: 'Refused: the request carried no API key.',
    invalid_api_key: 'Refused: the API key is not valid for this API.',
    expired_api_key: 'Refused: the API key has expired.',
    invalid_jwt: 'Refused: the bearer token could not be verified.',
    not_subscribed: 'Refused: the consumer is not subscribed to this API.',
    subscription_pending: 'Refused: the subscription is waiting for approval.',
    rate_limit_exceeded: 'Refused: the per-minute rate limit was exceeded.',
    daily_quota_exceeded: 'Refused: the daily quota is used up.',
    monthly_quota_exceeded: 'Refused: the monthly quota is used up.',
    client_closed_request: 'The client closed the connection before the response was sent.',
    upstream_error: 'The upstream service failed or could not be reached.',
    route_not_found: 'No API is published at this path.',
  };
  const humanReason = (code, ok) => reasonText[code] || (code ? (ok ? 'Allowed: ' : 'Refused: ') + code.replaceAll('_', ' ') + '.' : (ok ? reasonText.proxied_successfully : 'Refused.'));

  async function openDecisionModal(r) {
    const isOk = r.status < 400;
    let diag = null;
    try {
      if (r.request_id) {
        diag = await api('GET', `/api/requests/${r.request_id}/diagnose`);
      } else {
        diag = await api('POST', '/api/requests/diagnose', r);
      }
    } catch (_) {}

    // The gateway evaluates in order; once a check refuses the request,
    // later checks never run and are shown as not evaluated.
    const pub = r.policy_evaluations?.auth?.type === 'none';
    const sub = r.subscription_status || 'none';
    const rate = r.rate_limit_status || 'ok';
    const steps = [
      { name: 'Routing', ok: !!r.api_name, detail: r.api_name ? `Matched API <code>${esc(r.api_name)}</code> on <code>${esc(r.matched_route || r.path)}</code>` : 'No published API matches this path' },
      { name: 'Authentication', ok: r.auth_status === 'ok', detail: pub ? 'Not required: public API' : r.auth_status === 'ok' ? 'Credential accepted' : `Refused: <code>${esc(r.auth_status || 'unknown')}</code>` },
      { name: 'Subscription', ok: ['active', 'not_required', 'none'].includes(sub), detail: sub === 'active' ? 'Active subscription' : sub === 'not_required' || sub === 'none' ? 'Not required for this API' : `Refused: <code>${esc(sub)}</code>` },
      { name: 'Rate limit & quota', ok: rate === 'ok', detail: rate === 'ok' ? 'Within limits' : `Refused: <code>${esc(rate)}</code>` },
      { name: 'Protocol & plugin policies', ok: isOk || r.status >= 500 || r.status === 499 || !r.decision_reason || r.decision_reason === 'proxied_successfully', detail: isOk || !r.decision_reason || r.decision_reason === 'proxied_successfully' || r.status >= 500 || r.status === 499 ? 'No policy refused the request' : `Refused: <code>${esc(r.decision_reason)}</code>` },
      { name: 'Upstream', ok: r.status < 500 && r.status !== 499, detail: r.status === 499 ? 'Client disconnected before the response' : r.status >= 500 ? `Failed: ${esc(r.error || 'HTTP ' + r.status)}` : `Responded ${esc(r.status)} in ${fmtMs(r.upstream_duration_ms || 0)}` },
    ];
    let failed = false;
    const trail = steps.map((st) => {
      const state = failed ? 'skip' : st.ok ? 'ok' : 'fail';
      if (state === 'fail') failed = true;
      return `<div class="decision-step ${state}"><div class="step-icon ${state}" aria-hidden="true"></div><div><b>${st.name}</b><div class="muted small">${state === 'skip' ? 'Not evaluated' : st.detail}</div></div></div>`;
    }).join('');
    const remedies = (list, none) => list?.length ? `<ol class="remedy-list">${list.map((a) => `<li>${esc(String(a).replace(/^\s*\d+[.)]\s*/, ''))}</li>`).join('')}</ol>` : `<p class="muted small" style="margin:0">${none}</p>`;

    openModal({
      title: `Request ${r.request_id || 'detail'}`,
      large: true,
      body: `
        <div class="trail-head">
          <div>${statusPill(r.status)} ${methodBadge(r.method)} <code class="mono" style="font-size:14px;font-weight:600">${esc(r.path)}</code></div>
          <div class="muted small">${fmtDate(r.ts)}</div>
        </div>
        <div class="callout ${isOk ? 'success' : 'danger'}"><b>Decision:</b> ${esc(humanReason(r.decision_reason, isOk))}</div>
        ${diag ? `
          <div class="diagnosis">
            <div class="diagnosis-head"><h3>Diagnosis</h3><span class="tag ${isOk ? 'success' : 'warn'}">${esc(diag.root_cause_category)}</span></div>
            <p>${esc(diag.plain_language_explanation)}</p>
            <div class="grid two">
              <div class="remedy"><h4>For the developer</h4>${remedies(diag.developer_actions, 'No client changes needed.')}</div>
              <div class="remedy"><h4>For the operator</h4>${remedies(diag.operator_actions, 'No gateway changes needed.')}</div>
            </div>
          </div>` : ''}
        <h3 class="trail-title">Decision trail</h3>
        <div class="decision-trail">${trail}</div>
        <details class="trail-details">
          <summary>Request details</summary>
          <table><tbody>
            <tr><td class="muted">Configuration revision</td><td><span class="tag green mono">rev_${String(r.config_revision || 1).padStart(6, '0')}</span></td></tr>
            <tr><td class="muted">Request ID</td><td class="mono">${esc(r.request_id)}</td></tr>
            <tr><td class="muted">Client IP</td><td class="mono">${esc(r.client_ip)}</td></tr>
            <tr><td class="muted">Gateway latency</td><td>${fmtMs(r.latency_ms)}</td></tr>
            <tr><td class="muted">Upstream time</td><td>${fmtMs(r.upstream_duration_ms || 0)}</td></tr>
            ${r.error ? `<tr><td class="muted">Error</td><td class="mono" style="color:var(--red)">${esc(r.error)}</td></tr>` : ''}
            ${r.policy_evaluations && Object.keys(r.policy_evaluations).length ? `<tr><td class="muted">Policy evaluations</td><td><pre class="trail-json">${esc(JSON.stringify(r.policy_evaluations, null, 2))}</pre></td></tr>` : ''}
          </tbody></table>
        </details>
      `,
      actions: [
        { label: 'Close', primary: !window.RelayOrbit, onClick: (close) => close() },
        ...(window.RelayOrbit && r.request_id ? [{ label: 'Ask Orbit about this request', primary: true, onClick: (close) => {
          close();
          window.RelayOrbit.open({ send: true, context: { request_id: r.request_id },
            prompt: `Why did request ${r.request_id} (${r.method} ${r.path}) return ${r.status}, and what should we do about it?` });
        } }] : [])
      ]
    });
  }

  // -------------------------------------------------------------------------
  // SIGNATURE FEATURE 2: Fleet Convergence Proof & Safe Rollback View
  // -------------------------------------------------------------------------
  function renderCanaryOverview(cs, revision, autoRollback, actions = true) {
    const pct = Math.max(0, Math.min(100, Number(cs?.traffic_percent) || 0));
    const split = pct > 0 || !!cs?.header;
    const dedicated = Number(cs?.canary_nodes_count) || 0;
    const ack = Number(cs?.canary_acknowledged_count) || 0;
    const standard = Number(cs?.standard_nodes_count) || 0;
    const splitting = Number(cs?.standard_nodes_serving_split) || 0;
    const routingReady = !!cs && (split ? standard > 0 && splitting === standard && ack === dedicated : dedicated > 0 && ack === dedicated);
    const comparison = cs?.comparison;
    const samples = Number(comparison?.canary?.requests) || 0;
    const minimum = Number(autoRollback?.min_requests) || 5;
    const rate = value => Number.isFinite(Number(value)) ? Number(value).toFixed(2) + '%' : 'Unavailable';
    const state = !cs ? 'Status unavailable' : !routingReady ? 'Waiting for gateways' : samples < minimum ? 'Collecting evidence' : 'Ready for review';
    const metric = (label, value, note) => `<div class="canary-metric"><span>${label}</span><strong>${value}</strong><small>${note}</small></div>`;
    return `<section class="canary-overview" aria-label="Canary deployment overview">
      <div class="canary-heading"><div><span class="canary-eyebrow">Deployment in progress</span><h2>Canary revision ${esc(revision)}</h2><p>A canary exposes a candidate release to a limited audience before wider deployment.</p></div><span class="tag ${routingReady ? 'success' : 'warn'}">${state}</span></div>
      <div class="canary-layout" style="display:grid; grid-template-columns: repeat(auto-fit, minmax(280px, 1fr)); gap:18px">
        <div>
          <h3>1. What is serving traffic</h3>
          <p class="muted small">Target group: <strong>${esc(cs?.target_group || 'Unavailable')}</strong></p>
          ${!cs ? '<p>Status could not be loaded. Refresh before making a release decision.</p>' : split ? `<div class="canary-allocation" role="img" aria-label="Configured caller allocation: ${pct}% candidate, ${100-pct}% baseline"><span style="width:${pct}%"></span></div><div class="canary-allocation-labels"><strong>${pct}% candidate</strong><span>${100-pct}% baseline</span></div><p class="muted small">Configured share of callers on standard gateways; observed request share may differ. Dedicated canary gateways always serve the candidate.${cs.header ? ` Requests matching <code>${esc(cs.header)}</code> also select the candidate.` : ''}</p>` : '<p><strong>Dedicated canary gateways only</strong></p><p class="muted">Standard gateways keep serving the baseline. No percentage split is configured.</p>'}
          <div class="canary-facts"><div><span>Dedicated gateways ready</span><strong>${cs ? `${ack} / ${dedicated}` : 'Unavailable'}</strong></div><div><span>Standard gateways ${split ? 'splitting traffic' : 'registered'}</span><strong>${cs ? split ? `${splitting} / ${standard}` : standard : 'Unavailable'}</strong></div></div>
          ${cs && !split && !dedicated ? '<p class="canary-notice">No dedicated canary gateways are registered. Configure a traffic split or register a canary gateway before expecting candidate traffic.</p>' : ''}
        </div>
        <div>
          <h3>2. What is tested &amp; observed</h3>
          <div class="canary-metrics">
            ${metric('Candidate requests', comparison ? fmtNum(samples) : 'Unavailable', comparison ? `Last ${Number(comparison.window_seconds) || 60} seconds` : 'Traffic comparison could not be loaded')}
            ${metric('Candidate 5xx errors', comparison ? rate(comparison.canary?.error_rate_pct) : 'Unavailable', 'Server errors, not all failed requests')}
            ${metric(comparison?.baseline?.revision ? `Baseline rev_${esc(comparison.baseline.revision)} 5xx errors` : 'Baseline 5xx errors', comparison ? rate(comparison.baseline?.error_rate_pct) : 'Unavailable', comparison ? `${fmtNum(comparison.baseline?.requests)} baseline requests` : 'No comparison available')}
            ${metric('Promotion gate', !cs?.gate_status?.enforced ? 'Not enforced' : cs?.gate_status?.eligible ? 'Eligible' : 'Blocked', !cs?.gate_status?.enforced ? 'No active gate policy' : cs?.gate_status?.eligible ? 'Passing test evidence recorded' : 'Missing required test suite run')}
          </div>
        </div>
        <div>
          <h3>3. What blocks promotion</h3>
          <ul style="font-size:12px; line-height:1.6; padding-left:18px; margin:0 0 10px; color:var(--text)">
            <li style="color:${routingReady ? 'var(--green)' : 'var(--amber)'}"><strong>Gateway sync:</strong> ${routingReady ? 'All nodes acknowledged' : 'Gateways still synchronizing'}</li>
            <li style="color:${samples >= minimum ? 'var(--green)' : 'var(--amber)'}"><strong>Traffic evidence:</strong> ${samples >= minimum ? `${samples} requests observed (>= ${minimum} min)` : `Insufficient sample (${samples}/${minimum} reqs)`}</li>
            <li style="color:${!cs?.gate_status?.enforced || cs?.gate_status?.eligible ? 'var(--green)' : 'var(--red)'}"><strong>Test Studio gate:</strong> ${!cs?.gate_status?.enforced ? 'Policy not enforced' : cs?.gate_status?.eligible ? 'Eligible (fresh passing evidence)' : 'Blocked (failing or missing test run)'}</li>
          </ul>
          <p class="canary-notice" style="margin-top:6px">${!comparison ? 'Do not infer release health without traffic evidence.' : samples < minimum ? `Too little candidate traffic to assess health (${samples} of ${minimum} minimum requests).` : cs?.gate_status?.enforced && !cs?.gate_status?.eligible ? 'Automated Test Studio promotion gate is active: a passing test suite run is required before fleet promotion.' : 'Review errors, consumer impact and contract validation before promoting. Gateway readiness alone does not prove release health.'}</p>
        </div>
      </div>
      <div class="canary-next"><div><strong>Next decision</strong><p>Promote sends the candidate to the full fleet. Abort withdraws it and restores baseline routing. Gateways acknowledge changes asynchronously.</p></div>${actions ? `<div class="canary-actions"><button class="button ghost" data-canary-status="${revision}">Gateway details</button><button class="button" data-canary="${revision}">Adjust exposure</button><button class="button danger" data-abort="${revision}">Abort canary</button><button class="button primary" data-promote="${revision}">Review promotion</button></div>` : ''}</div>
    </section>`;
  }

  views.fleet = async () => {
    const [fleet, revs, autoRollback, runtime, nodeCreds] = await Promise.all([
      api('GET', '/api/fleet/status'),
      api('GET', '/api/revisions'),
      api('GET', '/api/revisions/auto-rollback/config').catch(() => null),
      api('GET', '/api/system/runtime').catch(() => null),
      api('GET', '/api/admin/dataplane/credentials').catch(() => null) // superadmins only
    ]);
    // Nodes re-acknowledge every 30s; no report for 90s means the node is gone or cut off.
    const heartbeatAge = (n) => Math.max(0, Math.round((Date.now() - new Date(n.applied_at).getTime()) / 1000));
    const fmtAge = (s) => s < 90 ? `${s}s ago` : s < 5400 ? `${Math.round(s / 60)} min ago` : `${Math.round(s / 3600)} h ago`;
    const canaryRevision = fleet.canary_revision || revs.find(r => r.status === 'canary')?.revision;
    const canaryStatus = canaryRevision ? await api('GET', `/api/revisions/${canaryRevision}/canary-status`).catch(() => null) : null;
    const nodes = fleet.nodes || [];
    const canaryNodes = nodes.filter(n => n.is_canary);
    const standardNodes = nodes.filter(n => !n.is_canary);
    const arCooldown = !!(autoRollback?.cooldown_until && new Date(autoRollback.cooldown_until) > new Date());
    const nodeOnTarget = (n) => n.revision === fleet.target_revision || (n.is_canary && fleet.canary_revision && n.revision === fleet.canary_revision);

    if (currentView !== 'fleet') return;
    $('#view').innerHTML = `
      <div class="toolbar">
        <div class="muted">Real-time convergence tracking, canary releases, side-by-side release comparisons, automatic rollback, and declarative configuration across data-plane gateway fleet.</div>
        <div class="spacer"></div>
        <button class="button small" id="btn-compare-revs">Compare revisions</button>
        <button class="button small ghost" id="btn-auto-rollback">Auto-Rollback</button>
        <button class="button small ghost" id="btn-export-config">Export config</button>
        <button class="button primary" id="btn-apply-config">Apply config</button>
        <button class="button small" id="refresh-fleet">Refresh</button>
      </div>

      ${canaryRevision ? renderCanaryOverview(canaryStatus, canaryRevision, autoRollback) : `<section class="canary-overview"><span class="canary-eyebrow">Progressive delivery</span><h2>No canary deployment in progress</h2><p>Validate your configuration and review consumer impact, then apply it as a canary. Observe a limited audience before promoting to the full fleet.</p><div class="canary-next"><span>Start with <strong>Apply config</strong> above and select <strong>Publish as canary</strong>.</span></div></section>`}
      <ol class="release-steps" aria-label="Release workflow">
        <li><b>1</b><a href="#/apis">Impact</a></li>
        <li><b>2</b><a href="#/tests">Test Studio</a></li>
        <li><b>3</b>Canary</li>
        <li><b>4</b><a href="#/live">Observe</a></li>
        <li><b>5</b>Promote / Roll back</li>
      </ol>
      <section class="card" id="release-journey" style="margin-bottom:20px">
        <div class="card-header"><h2>Release journey</h2></div>
        <p class="muted">Impact, a reviewed plan, Test Studio on the live revision, canary, then promote. Promotion without fresh eligible evidence returns HTTP 412.</p>
        <p>${canaryRevision
          ? `Canary <a href="#/tests">rev_${canaryRevision}</a> is in flight. Run the required suite against that revision, then review promotion. A blocked promote lists the missing or stale evidence.`
          : `No canary. Preview consumer impact on <a href="#/apis">APIs</a>, apply a reviewed plan as a canary, then validate it in <a href="#/tests">Test Studio</a>.`}</p>
      </section>
      <div class="grid two" style="grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: 16px;">
        <div class="card kpi">
          <div class="label">Stable Revision</div>
          <div class="value" style="color:var(--green)">rev_${String(fleet.target_revision || 1).padStart(6, '0')}</div>
          <div class="sub">${fleet.canary_revision ? ` Canary in flight: <b>rev_${fleet.canary_revision}</b>` : 'No canary in flight'}</div>
        </div>
        <div class="card kpi">
          <div class="label">Fleet Convergence State</div>
          <div class="value" style="color:${fleet.converged ? 'var(--green)' : 'var(--amber)'}">
            ${fleet.converged ? '100% Converged' : 'Synchronizing…'}
          </div>
          <div class="sub">${nodes.length} node${nodes.length === 1 ? '' : 's'} (${canaryNodes.length} canary, ${standardNodes.length} standard)</div>
        </div>
        <div class="card kpi" id="kpi-auto-rollback" style="cursor:pointer" title="Click to configure auto-rollback">
          <div class="label">Auto-Rollback Supervisor</div>
          <div class="value" style="color:${autoRollback?.enabled ? (arCooldown ? 'var(--amber)' : 'var(--green)') : 'var(--muted)'}">
            ${!autoRollback ? 'Unavailable' : autoRollback.enabled ? (arCooldown ? 'In cooldown' : 'Monitoring') : 'Disabled'}
          </div>
          <div class="sub">${autoRollback?.enabled ? `Threshold: ${Number(autoRollback.error_rate_threshold_percent ?? 5).toFixed(1)}% over ${autoRollback.evaluation_window_seconds ?? 60}s · Min: ${autoRollback.min_requests ?? 5} reqs · Fleet-wide` : 'Background watcher disabled'}</div>
        </div>
        <div class="card kpi" id="kpi-gitops" style="cursor:pointer" title="Declarative GitOps Configuration">
          <div class="label">Declarative GitOps Config</div>
          <div class="value" style="color:var(--blue)">format_version 1.0</div>
          <div class="sub">Validated plan  atomic apply (fleet-wide or canary)</div>
        </div>
      </div>

      <div class="card" style="margin-top:20px">
        <div class="card-header">
          <h2>Gateway fleet</h2>
        </div>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Node ID</th>
                <th>Node Role & Group</th>
                <th>Active Revision</th>
                <th>Convergence Status</th>
                <th>Active Routes</th>
                <th>Active Keys</th>
                <th>Application Latency</th>
                <th>Last heartbeat</th>
              </tr>
            </thead>
            <tbody>
              ${nodes.map((n) => `
                <tr>
                  <td><b>${esc(n.node_id)}</b></td>
                  <td>
                    <span class="tag ${n.is_canary ? 'blue' : 'gray'}">${n.is_canary ? ' Canary node' : 'Standard'}</span>
                    <span class="muted small" style="margin-left:4px">${esc(n.node_group || 'default')}</span>
                  </td>
                  <td class="mono">
                    <span class="tag green">rev_${String(n.revision).padStart(6, '0')}</span>
                    ${n.canary_revision ? `<span class="tag blue small" title="Serving a traffic-split canary">+ split rev_${n.canary_revision}</span>` : ''}
                  </td>
                  <td>
                    ${nodeOnTarget(n)
                      ? '<span class="tag success"> Up-to-date</span>'
                      : '<span class="tag warn">Pending Sync</span>'}
                  </td>
                  <td>${fmtNum(n.routes_count)} routes</td>
                  <td>${fmtNum(n.keys_count)} keys</td>
                  <td class="mono">${n.took_ms.toFixed(2)} ms</td>
                  <td class="muted" title="${esc(fmtDate(n.applied_at))}">${heartbeatAge(n) > 90 ? `<span class="tag warn">Stale</span> ` : ''}${fmtAge(heartbeatAge(n))}</td>
                </tr>
              `).join('') || '<tr><td colspan="8" class="empty">No gateway nodes registered yet.</td></tr>'}
            </tbody>
          </table>
        </div>
      </div>

      ${runtime ? `
      <div class="grid two" style="grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: 16px; margin-top:20px">
        <div class="card kpi">
          <div class="label">This node's role</div>
          <div class="value">${esc(runtime.role === 'all' ? 'Gateway + control plane' : runtime.role === 'control-plane' ? 'Control plane' : runtime.role)}</div>
          <div class="sub">${esc(runtime.node_id)}</div>
        </div>
        <div class="card kpi">
          <div class="label">Request-log sampling</div>
          <div class="value" style="color:${runtime.log_sample_rate < 1 ? 'var(--amber)' : 'var(--green)'}">${runtime.log_sample_rate < 1 ? `${+(runtime.log_sample_rate * 100).toFixed(2)}% of successes` : 'All requests'}</div>
          <div class="sub">${runtime.log_sample_rate < 1 ? 'Errors always kept; totals are weighted estimates' : 'Every request is written to request logs'}</div>
        </div>
        <div class="card kpi">
          <div class="label">Rollback evaluation</div>
          <div class="value">Every ${esc(runtime.auto_rollback_interval_seconds)}s</div>
          <div class="sub" style="overflow-wrap:anywhere">Set with RELAYOPS_AUTO_ROLLBACK_INTERVAL_SECONDS</div>
        </div>
        <div class="card kpi">
          <div class="label">Gateway node API</div>
          <div class="value" style="color:${runtime.node_api?.enabled ? 'var(--green)' : 'var(--muted)'}">${runtime.node_api?.enabled ? 'Enabled' : 'Off'}</div>
          <div class="sub">${runtime.node_api?.enabled ? [runtime.node_api.signing_key_id ? `Signed config · key ${esc(runtime.node_api.signing_key_id)}` : 'Unsigned config', runtime.node_api.shared_token ? 'shared token on' : 'per-node credentials only', runtime.node_api.require_client_cert ? 'mTLS required' : ''].filter(Boolean).join(' · ') : 'Gateway-only nodes cannot connect'}</div>
        </div>
      </div>` : ''}

      ${nodeCreds ? `
      <div class="card" style="margin-top:20px" id="node-credentials">
        <div class="card-header">
          <h2>Gateway node credentials</h2>
          <span class="muted small">Per-node tokens for gateway-only nodes. Each works only for its node ID and can be revoked on its own.</span>
          <div class="spacer"></div>
          <button class="button small primary" id="btn-issue-cred">Issue credential</button>
        </div>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Node ID</th><th>Description</th><th>Status</th><th>Last seen</th><th>Created</th><th></th></tr></thead>
            <tbody>
              ${nodeCreds.map((c) => {
                const expired = c.expires_at && new Date(c.expires_at) < new Date();
                const status = c.revoked_at ? '<span class="tag gray">Revoked</span>' : expired ? '<span class="tag warn">Expired</span>' : '<span class="tag success">Active</span>';
                return `<tr>
                  <td><b>${esc(c.node_id)}</b></td>
                  <td>${esc(c.description || '')}</td>
                  <td>${status}${c.expires_at && !c.revoked_at ? ` <span class="muted small">until ${esc(fmtDate(c.expires_at))}</span>` : ''}</td>
                  <td class="muted">${c.last_seen_at ? `${esc(fmtDate(c.last_seen_at))} <span class="small">${esc(c.last_seen_ip || '')}</span>` : 'Never'}</td>
                  <td class="muted">${esc(fmtDate(c.created_at))}<div class="small">${esc(c.created_by || '')}</div></td>
                  <td>${c.revoked_at ? '' : `<button class="button small danger" data-revoke-cred="${esc(c.id)}" data-node="${esc(c.node_id)}">Revoke</button>`}</td>
                </tr>`;
              }).join('') || '<tr><td colspan="6" class="empty">No node credentials issued. Gateways use the shared node token, if one is configured.</td></tr>'}
            </tbody>
          </table>
        </div>
      </div>` : ''}

      <div class="card" style="margin-top:20px">
        <div class="card-header">
          <h2>Release history</h2>
          <span class="muted small">Compare proposed revisions, execute canary deployments, inspect canary convergence, or perform instant transactional rollbacks</span>
        </div>
        ${(() => {
          // A newly published revision goes live on every gateway. Canarying it
          // returns the fleet to the previous active release (the baseline) and
          // exposes the new one to a share of callers.
          const prevActive = (rev) => revs.find(x => x.status === 'active' && (x.target_group || 'all') === 'all' && Number(x.revision) < Number(rev))?.revision;
          const card = (r) => {
            const active = Number(r.revision) === Number(fleet.target_revision);
            const canary = r.status === 'canary';
            const label = canary ? 'Canary' : r.status === 'rolled_back' ? 'Rolled back' : active ? 'Live on all gateways' : 'Previous release';
            const fallback = active && !canaryRevision ? prevActive(r.revision) : undefined;
            return `<article class="release-history-card"><div class="canary-heading"><h3>Revision ${esc(r.revision)}</h3><span class="tag ${canary ? 'blue' : active ? 'success' : 'gray'}">${label}</span></div><p>${esc(r.description || 'Configuration snapshot')}</p><dl class="canary-facts"><div><dt>Scope</dt><dd>${esc(r.target_group || 'all')}</dd></div><div><dt>Created by</dt><dd>${esc(r.created_by || 'system')}</dd></div><div><dt>Created</dt><dd>${fmtDate(r.created_at)}</dd></div></dl>${r.rollback_of ? `<p class="muted small">Restores configuration from revision ${esc(r.rollback_of)}</p>` : ''}${fallback ? `<p class="muted small">Moving it to canary sends most callers back to revision ${esc(fallback)} while this release is verified.</p>` : ''}<div class="canary-actions"><button class="button small ghost" data-compare="${r.revision}">Compare with baseline</button>${canary ? `<button class="button small" data-canary-status="${r.revision}">Deployment details</button>` : fallback ? `<button class="button small" data-canary="${r.revision}" data-baseline="${fallback}">Move to canary</button>` : ''}${!active && !canary ? `<button class="button small" data-rollback="${r.revision}">Restore this release</button>` : ''}</div></article>`;
          };
          if (!revs.length) return '<p class="empty">No releases yet. Apply a reviewed configuration to create your first release.</p>';
          const shown = 6;
          return `<div class="release-history-cards">${revs.slice(0, shown).map(card).join('')}</div>${revs.length > shown ? `<details class="release-history-more"><summary>Show ${revs.length - shown} older release${revs.length - shown === 1 ? '' : 's'}</summary><div class="release-history-cards">${revs.slice(shown).map(card).join('')}</div></details>` : ''}`;
        })()}
      </div>
    `;

    $('#refresh-fleet').onclick = () => views.fleet();
    const btnIssueCred = $('#btn-issue-cred');
    if (btnIssueCred) {
      btnIssueCred.onclick = () => openForm({
        title: 'Issue gateway node credential',
        fields: [
          { name: 'node_id', label: 'Node ID', required: true, placeholder: 'edge-sydney-1', help: "The gateway's RELAYOPS_NODE_ID. The token works only for this node." },
          { name: 'description', label: 'Description', placeholder: 'Sydney edge, rack 2' },
          { name: 'expires_in_days', label: 'Expires after (days)', type: 'number', value: 90, help: '0 for no expiry.' }
        ],
        submitLabel: 'Issue credential',
        onSubmit: async (v) => {
          const out = await api('POST', '/api/admin/dataplane/credentials', { node_id: v.node_id, description: v.description, expires_in_days: Number(v.expires_in_days) || 0 });
          openModal({
            title: `Credential issued for ${out.credential.node_id}`,
            body: `
              <div class="callout warning">Copy this token now. Only its SHA-256 hash is stored; it cannot be shown again.</div>
              <div class="secret-box">${esc(out.token)}</div>
              <div class="muted small">On the gateway: <code class="mono">RELAYOPS_NODE_ID=${esc(out.credential.node_id)}</code> and <code class="mono">RELAYOPS_DATAPLANE_TOKEN=&lt;this token&gt;</code></div>`,
            actions: [{ label: 'Copy token', primary: true, onClick: (close) => { navigator.clipboard?.writeText(out.token); toast('Token copied', 'success'); close(); } }]
          });
        }
      });
    }
    document.querySelectorAll('[data-revoke-cred]').forEach((btn) => {
      btn.onclick = () => openModal({
        title: 'Revoke gateway node credential',
        body: `<p>Revoke the credential for <b>${esc(btn.dataset.node)}</b>? The node is refused on its next call and keeps serving its last configuration until it gets a new credential.</p>`,
        actions: [{ label: 'Revoke', danger: true, onClick: async (close) => {
          try { await api('DELETE', `/api/admin/dataplane/credentials/${btn.dataset.revokeCred}`); toast('Credential revoked', 'success'); close(); views.fleet(); }
          catch (e) { toast(e.message || 'Revoke failed', 'error'); }
        } }]
      });
    });
    const btnCompareRevs = $('#btn-compare-revs');
    if (btnCompareRevs) {
      btnCompareRevs.onclick = () => {
        if (!revs.length) { toast('No revisions are available to compare.', 'error'); return; }
        const options = revs.map(r => ({value: r.revision, label: `Revision ${r.revision} · ${r.status.replaceAll('_', ' ')}`}));
        openModal({title: 'Compare revisions', body: `${fieldHTML({name:'base',label:'Baseline revision',type:'select',options,value:revs.find(r => r.revision < fleet.target_revision)?.revision || revs.at(-1).revision})}${fieldHTML({name:'target',label:'Target revision',type:'select',options,value:fleet.target_revision || revs[0].revision})}`, actions:[{label:'Compare',primary:true,onClick:(close,wrap)=>{const base=Number(wrap.querySelector('[name=base]').value),target=Number(wrap.querySelector('[name=target]').value);close();openRevisionCompareModal(base,target);}}]});
      };
    }

    const openAutoRollbackDialog = () => {
      openModal({
        title: ' Automatic rollback',
        large: false,
        body: `
          <p class="muted small" style="margin-bottom:14px">
            The supervisor evaluates the candidate revision against the previous baseline. If error rates exceed the configured threshold after minimum traffic is observed, RelayOps automatically rolls back to the safe baseline revision.
          </p>
          <div class="form-grid" style="display:flex;flex-direction:column;gap:12px">
            <label style="display:flex;align-items:center;gap:8px;cursor:pointer">
              <input type="checkbox" id="ar-enabled" ${autoRollback?.enabled ? 'checked' : ''}>
              <b>Enable Automatic Rollback Supervisor</b>
            </label>
            <div>
              <label class="label">Error Rate Threshold (% of 5xx responses, e.g. 5)</label>
              <input type="number" step="0.1" min="0.1" max="100" id="ar-threshold" class="input" value="${autoRollback?.error_rate_threshold_percent ?? 5}">
            </div>
            <div>
              <label class="label">Evaluation Window (seconds of fleet-wide traffic)</label>
              <input type="number" min="10" max="86400" id="ar-window" class="input" value="${autoRollback?.evaluation_window_seconds ?? 60}">
            </div>
            <div>
              <label class="label">Minimum Requests Sample (before triggering)</label>
              <input type="number" min="1" id="ar-min-requests" class="input" value="${autoRollback?.min_requests ?? 5}">
            </div>
            <div>
              <label class="label">Cooldown Window (seconds to prevent flapping)</label>
              <input type="number" min="0" id="ar-cooldown" class="input" value="${autoRollback?.cooldown_seconds ?? 60}">
            </div>
            <div>
              <label class="label">Minimum Baseline Requests (needed to compare against the previous revision)</label>
              <input type="number" min="0" id="ar-min-baseline" class="input" value="${autoRollback?.min_baseline_requests ?? 20}">
            </div>
            <div>
              <label class="label">When the baseline has too little traffic</label>
              <select id="ar-baseline-action" class="input">
                <option value="absolute" ${(autoRollback?.insufficient_baseline_action ?? 'absolute') === 'absolute' ? 'selected' : ''}>Act on the absolute threshold alone</option>
                <option value="hold" ${autoRollback?.insufficient_baseline_action === 'hold' ? 'selected' : ''}>Hold: never roll back without a comparison</option>
              </select>
            </div>
            <div class="card" style="padding:10px;background:#f8fafc;font-size:12px">
              <div><b>Active Target Revision:</b> rev_${autoRollback?.current_active_revision ?? fleet.target_revision}</div>
              <div><b>Safe Baseline Revision:</b> rev_${autoRollback?.current_baseline_revision ?? Math.max(1, fleet.target_revision - 1)}</div>
              <div><b>Cooldown Status:</b> ${arCooldown ? '<span class="tag warn small">In Cooldown until ' + fmtDate(autoRollback.cooldown_until) + '</span>' : '<span class="tag success small">Ready / Active</span>'}</div>
              <div><b>Last Evaluated:</b> ${autoRollback?.last_evaluated_at ? fmtDate(autoRollback.last_evaluated_at) + ' by ' + esc(autoRollback.last_evaluated_by || '') : 'never'}</div>
              <div class="muted" style="margin-top:4px">Settings are stored in Postgres and shared by every control-plane node; one node evaluates at a time.</div>
              ${autoRollback?.last_triggered_reason ? `<div style="margin-top:4px"><b>Last Triggered:</b> <span class="tag error small">${esc(autoRollback.last_triggered_reason)}</span></div>` : ''}
            </div>
          </div>
        `,
        actions: [
          {
            label: 'Evaluate now',
            onClick: async (close) => {
              try {
                const evalRes = await api('POST', '/api/revisions/auto-rollback/evaluate');
                if (evalRes.triggered) {
                  toast(` Rollback triggered! ${evalRes.reason}`, 'warning');
                } else {
                  toast(` Evaluated: ${evalRes.reason || 'Thresholds within safe baseline limits.'}`, 'success');
                }
                close();
                views.fleet();
              } catch (err) {
                toast(esc(err.message), 'error');
              }
            }
          },
          {
            label: 'Save',
            primary: true,
            onClick: async (close, wrap) => {
              try {
                const enabled = wrap.querySelector('#ar-enabled').checked;
                const error_rate_threshold_percent = parseFloat(wrap.querySelector('#ar-threshold').value);
                const evaluation_window_seconds = parseInt(wrap.querySelector('#ar-window').value, 10);
                const min_requests = parseInt(wrap.querySelector('#ar-min-requests').value, 10);
                const cooldown_seconds = parseInt(wrap.querySelector('#ar-cooldown').value, 10);
                const min_baseline_requests = parseInt(wrap.querySelector('#ar-min-baseline').value, 10);
                const insufficient_baseline_action = wrap.querySelector('#ar-baseline-action').value;
                await api('POST', '/api/revisions/auto-rollback/config', {
                  enabled,
                  error_rate_threshold_percent,
                  evaluation_window_seconds,
                  min_requests,
                  cooldown_seconds,
                  min_baseline_requests,
                  insufficient_baseline_action
                });
                toast(' Auto-rollback supervisor configuration updated!', 'success');
                close();
                views.fleet();
              } catch (err) {
                toast(esc(err.message), 'error');
              }
            }
          }
        ]
      });
    };

    const btnAutoRollback = $('#btn-auto-rollback');
    if (btnAutoRollback) btnAutoRollback.onclick = openAutoRollbackDialog;
    const kpiAutoRollback = $('#kpi-auto-rollback');
    if (kpiAutoRollback && autoRollback) kpiAutoRollback.onclick = openAutoRollbackDialog;

    // Declarative Export
    const btnExportConfig = $('#btn-export-config');
    if (btnExportConfig) {
      btnExportConfig.onclick = async () => {
        try {
          const exportData = await api('GET', '/api/system/export');
          const jsonStr = JSON.stringify(exportData, null, 2);
          openModal({
            title: ` Declarative Config Export · rev_${String(fleet.target_revision || 1).padStart(6, '0')}`,
            large: true,
            body: `
              <p class="muted small" style="margin-bottom:10px">Declarative document (<code>"format_version": "1.0"</code>) with every API and plan. Secrets are never exported; reference them as <code>\${secret:NAME}</code>. Apply it unchanged and the plan shows no changes. CLI: <code>relayopsctl export -o relayops.yaml</code>.</p>
              <textarea readonly style="width:100%;height:320px;font-family:monospace;font-size:12px;padding:8px;box-sizing:border-box;background:#f8fafc;border:1px solid var(--border);border-radius:4px">${esc(jsonStr)}</textarea>
            `,
            actions: [
              {
                label: 'Copy JSON',
                onClick: (_, wrap) => {
                  navigator.clipboard.writeText(jsonStr);
                  toast('Configuration JSON copied to clipboard!', 'success');
                }
              },
              {
                label: 'Download file',
                primary: true,
                onClick: () => {
                  const blob = new Blob([jsonStr], { type: 'application/json' });
                  const url = URL.createObjectURL(blob);
                  const a = document.createElement('a');
                  a.href = url;
                  a.download = `relayops-config-rev${fleet.target_revision}.json`;
                  a.click();
                  URL.revokeObjectURL(url);
                  toast('Downloaded relayops-config JSON file', 'success');
                }
              }
            ]
          });
        } catch (err) {
          toast(esc(err.message), 'error');
        }
      };
    }

    // Declarative Apply
    const btnApplyConfig = $('#btn-apply-config');
    const renderPlan = (plan) => {
      if (!plan) return '';
      if (!plan.valid) {
        return `<div class="card" style="padding:10px;border-color:var(--red)"><b>Invalid document</b><ul>${(plan.errors || []).map(e => `<li>${esc(e)}</li>`).join('')}</ul></div>`;
      }
      const sym = { create: '+', update: '~', delete: '−', unchanged: '' };
      const rows = (plan.changes || []).filter(c => c.action !== 'unchanged').map(c => `
        <tr>
          <td><span class="tag ${c.action === 'delete' ? 'error' : c.action === 'create' ? 'success' : 'warn'}">${sym[c.action]} ${esc(c.action)}</span></td>
          <td>${esc(c.type)} <b>${esc(c.name)}</b></td>
          <td class="small">
            ${(c.changes || []).map(f => `<dl class="field-diff"><dt>${esc(f.field)}</dt><dd><span>Before</span><code>${esc(JSON.stringify(f.before))}</code></dd><dd><span>After</span><code>${esc(JSON.stringify(f.after))}</code></dd></dl>`).join('')}
            ${(c.consumer_impact?.breaking_changes || []).map(b => `<div style="color:var(--red)"> ${esc(b)}</div>`).join('')}
            ${c.replay ? `<div class="muted">Replay: ${c.replay.status_changed} of ${c.replay.replayed} recent requests change status</div>` : ''}
            ${(c.warnings || []).filter(w => !w.includes('recent requests')).map(w => `<div style="color:var(--amber)"> ${esc(w)}</div>`).join('')}
          </td>
        </tr>`).join('');
      const s = plan.summary || {};
      return `
        <div class="card" style="padding:10px">
          <b>Plan:</b> ${s.create || 0} to create, ${s.update || 0} to update, ${s.delete || 0} to delete, ${s.unchanged || 0} unchanged
          ${(plan.warnings || []).map(w => `<div style="color:var(--amber);margin-top:4px"> ${esc(w)}</div>`).join('')}
          ${rows ? `<div class="table-wrap" style="margin-top:8px"><table><thead><tr><th>Action</th><th>Resource</th><th>Details</th></tr></thead><tbody>${rows}</tbody></table></div>` : '<div class="muted" style="margin-top:6px">No changes. The live configuration already matches.</div>'}
          <div class="muted small" style="margin-top:6px">Plan hash <code>${esc(plan.plan_hash || '')}</code> — apply is refused if anything changes before you confirm.</div>
        </div>`;
    };

    const openApplyDialog = () => {
      let reviewed = null; // { text, prune, plan }
      openModal({
        title: 'Review configuration',
        large: true,
        body: `
          <p class="muted small" style="margin-bottom:10px">
            Paste a declarative document (<code>"format_version": "1.0"</code>). <b>Preview plan</b> validates it and shows field-level changes, consumer impact and a replay of recent traffic. <b>Apply</b> publishes exactly the reviewed plan as one atomic revision. CLI: <code>relayopsctl plan -f relayops.yaml</code>.
          </p>
          <label for="apply-cfg-input">Configuration document</label><textarea id="apply-cfg-input" style="width:100%;height:260px;font-family:monospace;font-size:12px;padding:8px;box-sizing:border-box;border:1px solid var(--border);border-radius:4px" placeholder='{
  "format_version": "1.0",
  "plans": [{ "name": "gold", "rate_limit_per_minute": 1200 }],
  "apis": [
    {
      "name": "payments-api",
      "base_path": "/pay",
      "upstream_url": "https://httpbin.org/anything",
      "auth_type": "api_key",
      "enabled": true,
      "traffic_policy": { "retries": { "attempts": 2 }, "circuit_breaker": { "failure_threshold": 5 } }
    }
  ]
}'></textarea>
          <div style="display:flex;gap:16px;flex-wrap:wrap;margin:8px 0">
            <label style="display:flex;gap:6px;align-items:center"><input type="checkbox" id="apply-prune"> Prune (delete APIs/plans not in the document)</label>
            <label style="display:flex;gap:6px;align-items:center"><input type="checkbox" id="apply-canary"> Publish as canary</label>
            <label style="display:flex;gap:6px;align-items:center">Traffic % <input type="number" id="apply-pct" class="input" min="0" max="100" value="10" style="width:80px"></label>
          </div>
          <div id="apply-plan-out"></div>
        `,
        actions: [
          {
            label: 'Preview plan',
            onClick: async (_, wrap) => {
              const text = wrap.querySelector('#apply-cfg-input').value.trim();
              if (!text) { toast('Paste a configuration document first', 'warning'); return; }
              const prune = wrap.querySelector('#apply-prune').checked;
              try {
                const { data } = await apiRaw('POST', '/api/system/plan' + (prune ? '?prune=true' : ''), text);
                if (!data || data.valid === undefined) throw new Error(data?.message || 'plan failed');
                reviewed = { text, prune, plan: data };
                wrap.querySelector('#apply-plan-out').innerHTML = renderPlan(data);
              } catch (err) {
                toast(esc(err.message), 'error');
              }
            }
          },
          {
            label: 'Apply reviewed plan',
            primary: true,
            onClick: async (close, wrap) => {
              const text = wrap.querySelector('#apply-cfg-input').value.trim();
              const prune = wrap.querySelector('#apply-prune').checked;
              if (!reviewed || reviewed.text !== text || reviewed.prune !== prune) {
                toast('Preview the plan for this exact document first', 'warning');
                return;
              }
              if (!reviewed.plan.valid) { toast('The document is invalid', 'error'); return; }
              const q = new URLSearchParams({ plan_hash: reviewed.plan.plan_hash });
              if (prune) q.set('prune', 'true');
              if (wrap.querySelector('#apply-canary').checked) {
                q.set('rollout', 'canary');
                q.set('traffic_percent', String(parseInt(wrap.querySelector('#apply-pct').value, 10) || 0));
              }
              openModal({title:'Confirm configuration release', body:`<p>Publish the reviewed configuration ${q.get('rollout') === 'canary' ? 'as a canary' : 'to the fleet'}?</p><p class="muted">${reviewed.plan.summary?.delete || 0} resources will be deleted. Review hash: <code>${esc(reviewed.plan.plan_hash)}</code></p>`, actions:[{label:'Apply reviewed plan',primary:true,onClick:async(confirmClose)=>{
              try {
                const res = await api('POST', '/api/system/apply?' + q.toString(), text);
                toast(res.applied ? ` Applied as rev_${res.revision}${res.rollout === 'canary' ? ' (canary)' : ''}` : 'No changes to apply', 'success');
                confirmClose();
                close();
                views.fleet();
              } catch (err) {
                toast(esc(err.message), 'error');
              }
              }}]});
            }
          }
        ]
      });
    };
    if (btnApplyConfig) btnApplyConfig.onclick = openApplyDialog;
    const kpiGitops = $('#kpi-gitops');
    if (kpiGitops) kpiGitops.onclick = openApplyDialog;

    $('#view').onclick = async (e) => {
      const d = actionData(e);
      const compareRev = d.compare;
      if (compareRev) {
        openRevisionCompareModal(Number(compareRev), fleet.target_revision);
        return;
      }

      const canaryStatusRev = d.canaryStatus;
      if (canaryStatusRev) {
        try {
          const cs = await api('GET', `/api/revisions/${canaryStatusRev}/canary-status`);
          openModal({
            title: `Canary deployment · revision ${canaryStatusRev}`,
            large: true,
            body: `
              ${renderCanaryOverview(cs, canaryStatusRev, autoRollback, false)}
              <div class="card" style="padding:12px">
                <div class="card-header"><h3 style="font-size:13px">Fleet Node Acknowledgement Breakdown</h3></div>
                <div class="table-wrap">
                  <table>
                    <thead>
                      <tr>
                        <th>Node ID</th>
                        <th>Type</th>
                        <th>Active Revision</th>
                        <th>Status</th>
                      </tr>
                    </thead>
                    <tbody>
                      ${[...(cs.canary_nodes || []), ...(cs.standard_nodes || [])].map(n => `
                        <tr>
                          <td><b>${esc(n.node_id)}</b></td>
                          <td><span class="tag ${n.is_canary ? 'blue' : 'gray'}">${n.is_canary ? ' Canary' : 'Standard'}</span></td>
                          <td class="mono">rev_${String(n.revision).padStart(6, '0')}</td>
                          <td>${n.revision === Number(canaryStatusRev) ? '<span class="tag success"> Serving canary</span>' : n.canary_revision === Number(canaryStatusRev) ? '<span class="tag blue">Split: baseline + canary</span>' : '<span class="tag warn">Baseline</span>'}</td>
                        </tr>
                      `).join('') || '<tr><td colspan="4" class="empty">No nodes registered</td></tr>'}
                    </tbody>
                  </table>
                </div>
              </div>
            `,
            actions: [{ label: 'Close', primary: true, onClick: (close) => close() }]
          });
        } catch (err) {
          toast(esc(err.message), 'error');
        }
        return;
      }

      const canaryRev = d.canary;
      if (canaryRev) {
        openModal({
          title: `${Number(canaryRev) === Number(canaryRevision) ? 'Adjust canary exposure' : 'Move to canary'} · revision ${canaryRev}`,
          body: `
            ${d.baseline ? `<p>Revision ${esc(d.baseline)} becomes the baseline again on standard gateways. Revision ${esc(canaryRev)} serves only the share you choose until you promote or abort it.</p>` : ''}
            <p class="muted small">Choose how many callers should receive the candidate. Dedicated canary gateways always serve it. A percentage split assigns callers consistently on standard gateways. Set the share to 0 and leave the header empty to use dedicated gateways only. A matching routing header overrides the percentage; review its exposure before saving.</p>
            <div style="display:flex;flex-direction:column;gap:10px;margin-top:8px">
              <label>Traffic share (%)<input type="number" id="cn-pct" class="input" min="0" max="100" value="${Number(canaryRev) === Number(canaryRevision) ? Number(canaryStatus?.traffic_percent) || 0 : 10}"></label>
              <label>Routing header (optional)<input type="text" id="cn-header" class="input" placeholder="X-Canary" value="${esc(Number(canaryRev) === Number(canaryRevision) ? canaryStatus?.header || '' : '')}"></label>
              <label>Header value (optional, any value when empty)<input type="text" id="cn-value" class="input" value="${esc(Number(canaryRev) === Number(canaryRevision) ? canaryStatus?.header_value || '' : '')}"></label>
            </div>`,
          actions: [{
            label: Number(canaryRev) === Number(canaryRevision) ? 'Update exposure' : 'Start canary',
            primary: true,
            onClick: async (close, wrap) => {
              try {
                const share = Number(wrap.querySelector('#cn-pct').value);
                if (!Number.isInteger(share) || share < 0 || share > 100) { toast('Enter a whole percentage between 0 and 100.', 'error'); return; }
                const res = await api('POST', `/api/revisions/${canaryRev}/canary`, {
                  traffic_percent: share,
                  header: wrap.querySelector('#cn-header').value.trim(),
                  header_value: wrap.querySelector('#cn-value').value,
                });
                toast(` ${res.message}`, 'success');
                close();
                views.fleet();
              } catch (err) {
                toast(esc(err.message), 'error');
              }
            }
          }]
        });
        return;
      }

      const abortRev = d.abort;
      if (abortRev) {
        confirmDialog(
          'Abort Canary',
          `Withdraw <b>rev_${abortRev}</b> from every node and restore the stable revision?`,
          async () => {
            const res = await api('POST', `/api/revisions/${abortRev}/abort`);
            toast(` ${res.message}`, 'success');
            views.fleet();
          },
          'Abort Canary'
        );
        return;
      }

      const promoteRev = d.promote;
      if (promoteRev) {
        const cs = await api('GET', `/api/revisions/${promoteRev}/canary-status`).catch(() => null);
        const gate = cs?.gate_status;
        if (gate?.enforced && !gate?.eligible) {
          openModal({
            title: 'Test Studio Promotion Gate Blocked',
            body: `
              <div class="form-error" style="margin-bottom:14px">
                <strong>Promotion Gate Not Satisfied</strong>
                <p>Automatic promotion is blocked because required passing test suite evidence is missing or ineligible for revision <b>rev_${promoteRev}</b>.</p>
                <ul style="margin:8px 0 0 16px; font-size:12px">
                  ${(gate.reasons || ['Passing test run evidence required']).map(r => `<li>${esc(r)}</li>`).join('')}
                </ul>
              </div>
              <p class="small muted">Superadmins may override this gate with an explicit business justification.</p>
              <div class="field" style="margin-top:12px">
                <label>Superadmin Override Reason</label>
                <input type="text" id="gate-override-reason" placeholder="e.g. Critical security hotfix verified in staging" />
              </div>
            `,
            actions: [
              {
                label: 'Promote with Override',
                danger: true,
                onClick: async (close, wrap) => {
                  const reason = wrap.querySelector('#gate-override-reason')?.value?.trim();
                  if (!reason) {
                    toast('Superadmin override reason is required to bypass test gate', 'warn');
                    return;
                  }
                  try {
                    await api('POST', `/api/revisions/${promoteRev}/promote?override_reason=${encodeURIComponent(reason)}`);
                    toast(` Revision rev_${promoteRev} promoted with gate override!`, 'success');
                    close();
                    views.fleet();
                  } catch (err) {
                    toast(err.message, 'error');
                  }
                }
              }
            ]
          });
          return;
        }

        confirmDialog(
          'Promote release',
          `Promote <b>rev_${promoteRev}</b> from canary to 100% of all gateway nodes in the cluster?`,
          async () => {
            await api('POST', `/api/revisions/${promoteRev}/promote`);
            toast(` Revision rev_${promoteRev} promoted to all fleet gateways!`, 'success');
            views.fleet();
          },
          'Promote'
        );
        return;
      }

      const revID = d.rollback;
      if (revID) {
        confirmDialog(
          'Restore release',
          `Rollback entire cluster configuration to <b>rev_${revID}</b>? This restores the saved gateway configuration as a new revision. Gateway acknowledgements arrive asynchronously; application data is not restored.`,
          async () => {
            const res = await api('POST', `/api/revisions/${revID}/rollback`);
            toast(` Rollback completed! New active revision is <b>rev_${res.new_revision}</b>`, 'success');
            views.fleet();
          },
          'Restore release'
        );
      }
    };
  };

  async function openRevisionCompareModal(baseRev, targetRev) {
    try {
      const raw = await api('GET', `/api/revisions/compare?base=${baseRev}&target=${targetRev}`);
      const comp = {risk_level:raw.risk_score ?? raw.risk_level ?? 'Unavailable',total_added:raw.apis_added_count ?? raw.total_added ?? '—',total_removed:raw.apis_removed_count ?? raw.total_removed ?? '—',total_modified:raw.apis_modified_count ?? raw.total_modified ?? '—',total_breaking_changes:raw.breaking_count ?? raw.total_breaking_changes ?? '—',summary:raw.summary_statement ?? raw.summary ?? '',apis:raw.api_diffs ?? raw.apis ?? []};
      const riskClass = ['CRITICAL','HIGH','CRITICAL_BREAKING','HIGH_BREAKING'].includes(comp.risk_level) ? 'error' : ['MEDIUM','MODERATE'].includes(comp.risk_level) ? 'warn' : 'success';
      const apis = comp.apis || [];

      openModal({
        title: `Release comparison · rev_${baseRev}  rev_${targetRev}`,
        large: true,
        body: `
          <div class="callout ${riskClass === 'error' ? 'danger' : riskClass === 'warn' ? 'warning' : 'success'}" style="margin-bottom:16px">
            <div style="display:flex;align-items:center;justify-content:space-between">
              <b>Release Risk Rating: <span class="tag ${riskClass}">${esc(comp.risk_level)}</span></b>
              <span class="muted small">${comp.total_breaking_changes} Breaking Changes Detected</span>
            </div>
            <p style="margin:6px 0 0 0;font-size:13px">${esc(comp.summary)}</p>
          </div>

          <div class="impact-summary" style="margin-bottom:16px">
            <div class="impact-box"><div class="lbl">Added APIs</div><div class="num" style="color:var(--green)">${comp.total_added}</div></div>
            <div class="impact-box"><div class="lbl">Modified APIs</div><div class="num" style="color:var(--amber)">${comp.total_modified}</div></div>
            <div class="impact-box"><div class="lbl">Removed APIs</div><div class="num" style="color:var(--red)">${comp.total_removed}</div></div>
            <div class="impact-box"><div class="lbl">Breaking Changes</div><div class="num" style="color:var(--red)">${comp.total_breaking_changes}</div></div>
          </div>

          <div class="card" style="padding:12px">
            <div class="card-header"><h3 style="font-size:14px">Changed APIs & Field Differences</h3></div>
            <div class="table-wrap" style="max-height:360px;overflow:auto">
              <table>
                <thead>
                  <tr>
                    <th>API Name</th>
                    <th>Change Type</th>
                    <th>Field</th>
                    <th>Category</th>
                    <th>Before (rev_${baseRev})</th>
                    <th>After (rev_${targetRev})</th>
                    <th>Breaking</th>
                  </tr>
                </thead>
                <tbody>
                  ${apis.flatMap(a => {
                    const diffs = a.field_diffs || [];
                    if (!diffs.length) {
                      return [`<tr>
                        <td><b>${esc(a.api_name)}</b></td>
                        <td><span class="tag ${a.change_type === 'ADDED' ? 'green' : a.change_type === 'REMOVED' ? 'error' : 'gray'} small">${esc(a.change_type)}</span></td>
                        <td colspan="5" class="muted small">No field-level configuration changes</td>
                      </tr>`];
                    }
                    return diffs.map((d, i) => `
                      <tr style="${d.is_breaking ? 'background:#fff1f2' : ''}">
                        ${i === 0 ? `<td rowspan="${diffs.length}"><b>${esc(a.api_name)}</b></td><td rowspan="${diffs.length}"><span class="tag ${a.change_type === 'ADDED' ? 'green' : a.change_type === 'REMOVED' ? 'error' : 'warn'} small">${esc(a.change_type)}</span></td>` : ''}
                        <td>${esc(d.field_name)}</td>
                        <td><span class="tag gray small">${esc(d.category)}</span></td>
                        <td class="mono small">${esc(JSON.stringify(d.before))}</td>
                        <td class="mono small"><b>${esc(JSON.stringify(d.after))}</b></td>
                        <td>${d.is_breaking ? '<span class="tag error small">BREAKING</span>' : '<span class="tag gray small">Safe</span>'}</td>
                      </tr>
                    `);
                  }).join('') || '<tr><td colspan="7" class="empty">No differences between revisions.</td></tr>'}
                </tbody>
              </table>
            </div>
          </div>
        `,
        actions: [{ label: 'Close', primary: true, onClick: (close) => close() }]
      });
    } catch (err) {
      toast(esc(err.message), 'error');
    }
  }

  // -------------------------------------------------------------------------
  // VIEW: Users & roles Management
  // -------------------------------------------------------------------------
  views.team = async () => {
    const users = await api('GET', '/api/admin/users');
    if (currentView !== 'team') return;
    $('#view').innerHTML = `
      <div class="toolbar">
        <div class="muted">Role-Based Access Control (RBAC): Named administrators, SSO integration, granular roles (superadmin, admin, operator, auditor, developer).</div>
        <div class="spacer"></div>
        <button class="button primary small" id="new-user">Add user</button>
      </div>

      <div class="card">
        <div class="card-header">
          <h2>Administrators (${users.length})</h2>
        </div>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Administrator</th>
                <th>Role</th>
                <th>Department / Team</th>
                <th>Identity Provider</th>
                <th>Last Login</th>
                <th>Status</th>
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              ${users.map((u) => {
                const roleBadge = u.role === 'superadmin' ? '<span class="tag role-superadmin">superadmin</span>'
                  : u.role === 'admin' ? '<span class="tag blue">admin</span>'
                  : u.role === 'operator' ? '<span class="tag green">operator</span>'
                  : u.role === 'auditor' ? '<span class="tag warn">auditor (read-only)</span>'
                  : '<span class="tag gray">' + esc(u.role) + '</span>';
                return `
                  <tr>
                    <td>
                      <b>${esc(u.name)}</b>
                      <div class="muted small">${esc(u.email)}</div>
                    </td>
                    <td>${roleBadge}</td>
                    <td>${esc(u.team || 'Engineering')}</td>
                    <td><span class="tag gray">${esc(u.sso_provider || 'local')}</span></td>
                    <td class="muted">${u.last_login_at ? fmtDate(u.last_login_at) : 'Never'}</td>
                    <td>${u.active ? '<span class="tag success">Active</span>' : '<span class="tag error">inactive</span>'}</td>
                    <td class="actions">
                      <button class="button small" data-edituser="${esc(u.id)}">Edit</button>
                      <button class="button small danger" data-deluser="${esc(u.id)}">Delete</button>
                    </td>
                  </tr>
                `;
              }).join('') || '<tr><td colspan="7" class="empty">No administrator users found.</td></tr>'}
            </tbody>
          </table>
        </div>
      </div>
    `;

    const userForm = (u) => openForm({
      title: u ? `Edit administrator · ${u.name}` : 'Add administrator',
      submitLabel: u ? 'Save changes' : 'Add administrator',
      fields: [
        [
          { name: 'name', label: 'Full Name', value: u?.name, required: true },
          { name: 'email', label: 'Email Address', value: u?.email, required: true }
        ],
        [
          {
            name: 'role', label: 'Role', type: 'select', value: u?.role || 'operator', help: 'Operator: APIs, routes, approvals, rollback. Admin: all policy and API management. Auditor: read-only. Superadmin: everything, including users. Developer: portal only.', options: [
              { value: 'operator', label: 'Operator' },
              { value: 'admin', label: 'Admin' },
              { value: 'auditor', label: 'Auditor (read-only)' },
              { value: 'superadmin', label: 'Superadmin' },
              { value: 'developer', label: 'Developer (portal only)' }
            ]
          },
          { name: 'team', label: 'Department / Team', value: u?.team || 'Engineering' }
        ],
        { name: 'active', label: 'Account active', type: 'checkbox', value: u ? u.active : true }
      ],
      onSubmit: async (v) => {
        if (u) {
          await api('PUT', `/api/admin/users/${u.id}`, v);
          toast(`Administrator <b>${esc(v.name)}</b> updated!`, 'success');
        } else {
          await api('POST', '/api/admin/users', v);
          toast(`Administrator <b>${esc(v.name)}</b> created!`, 'success');
        }
        views.team();
      }
    });

    $('#new-user').onclick = () => userForm();
    $('#view').onclick = (e) => {
      const d = actionData(e);
      const editID = d.edituser;
      const delID = d.deluser;
      if (editID) {
        const u = users.find((x) => x.id === editID);
        if (u) userForm(u);
      }
      if (delID) {
        confirmDialog('Delete Administrator', 'Permanently remove this administrator account?', async () => {
          await api('DELETE', `/api/admin/users/${delID}`);
          toast('Administrator removed', 'warn');
          views.team();
        });
      }
    };
  };

  // -------------------------------------------------------------------------
  // VIEW: Tenants & Workspaces
  // -------------------------------------------------------------------------
  views.tenants = async () => {
    const tenants = await api('GET', '/api/tenants').catch(() => []);
    if (currentView !== 'tenants') return;
    $('#view').innerHTML = `
      <div class="toolbar">
        <div class="muted">Multi-Tenant Isolation: Scoped environments, APIs, plans, and team memberships.</div>
        <div class="spacer"></div>
        <button class="button primary small" id="new-tenant">Create tenant</button>
      </div>

      <div class="card">
        <div class="card-header">
          <h2>Tenants (${tenants.length})</h2>
        </div>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Tenant</th>
                <th>Created</th>
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              ${tenants.map(t => `
                <tr>
                  <td>
                    <b>${esc(t.name)}</b>
                    <div class="mono small muted">${esc(t.slug)}</div>
                  </td>
                  <td class="muted">${fmtDate(t.created_at)}</td>
                  <td class="actions">
                    <button class="button small" data-members="${esc(t.slug)}" data-name="${esc(t.name)}">Members</button>
                    <button class="button small" data-edittenant="${esc(t.slug)}">Edit</button>
                    ${t.slug !== 'default' ? `<button class="button small danger" data-deltenant="${esc(t.slug)}">Delete</button>` : ''}
                  </td>
                </tr>
              `).join('') || '<tr><td colspan="3" class="empty">No tenants yet.</td></tr>'}
            </tbody>
          </table>
        </div>
      </div>
    `;

    $('#new-tenant').onclick = () => tenantForm();

    $('#view').onclick = async (e) => {
      const d = actionData(e);
      const memSlug = d.members;
      if (memSlug) {
        openTenantMembersModal(memSlug, d.name);
        return;
      }
      const editSlug = d.edittenant;
      if (editSlug) {
        const t = tenants.find(x => x.slug === editSlug);
        if (t) tenantForm(t);
        return;
      }
      const delSlug = d.deltenant;
      if (delSlug) {
        confirmDialog('Delete Tenant', `Permanently delete tenant <b>${esc(delSlug)}</b> and unassign its memberships?`, async () => {
          await api('DELETE', `/api/tenants/${encodeURIComponent(delSlug)}`);
          toast('Tenant deleted successfully', 'warn');
          views.tenants();
        });
      }
    };
  };

  function tenantForm(t = {}) {
    const isEdit = !!t.slug;
    openForm({
      title: isEdit ? `Edit tenant · ${t.name}` : 'New tenant',
      submitLabel: isEdit ? 'Save changes' : 'Create tenant',
      fields: [
        { name: 'name', label: 'Name', value: t.name || '', required: true, placeholder: 'Acme Corp' },
        { name: 'slug', label: 'Slug', value: t.slug || '', required: true, disabled: isEdit, placeholder: 'acme-corp', help: isEdit ? 'The slug identifies the tenant and cannot change.' : 'Lowercase letters, digits and hyphens.' },
        ...(isEdit ? [] : [{ name: 'admin_email', label: 'First administrator email (optional)', type: 'email', placeholder: 'admin@acme.com', help: 'An existing user who becomes the tenant admin.' }])
      ],
      onSubmit: async (data, close) => {
        if (isEdit) {
          await api('PUT', `/api/tenants/${encodeURIComponent(t.slug)}`, { name: data.name });
          toast('Tenant updated', 'success');
        } else {
          await api('POST', '/api/tenants', { name: data.name, slug: data.slug, ...(data.admin_email ? { admin_email: data.admin_email } : {}) });
          toast('Tenant created', 'success');
        }
        close();
        views.tenants();
      }
    });
  }

  async function openTenantMembersModal(slug, name) {
    try {
      const [members, allUsers] = await Promise.all([
        api('GET', `/api/tenants/${encodeURIComponent(slug)}/members`),
        api('GET', '/api/admin/users').catch(() => [])
      ]);
      openModal({
        title: `Members · ${esc(name || slug)}`,
        large: true,
        body: `
          <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:12px;gap:8px;flex-wrap:wrap">
            <span class="muted small">Users assigned access to this tenant and their scoped roles.</span>
            <div style="display:flex;gap:8px">
              <select id="add-member-user" class="input small" style="min-width:180px">
                <option value="">Select User to Add...</option>
                ${allUsers.map(u => `<option value="${esc(u.id)}">${esc(u.name)} (${esc(u.email)})</option>`).join('')}
              </select>
              <select id="add-member-role" class="input small">
                <option value="developer">developer</option>
                <option value="operator">operator</option>
                <option value="admin">admin</option>
                <option value="auditor">auditor</option>
              </select>
              <button class="button small primary" id="btn-add-member">Add</button>
            </div>
          </div>
          <div class="table-wrap" style="max-height:300px;overflow:auto">
            <table>
              <thead><tr><th>User</th><th>Email</th><th>Tenant Role</th><th>Assigned At</th><th>Action</th></tr></thead>
              <tbody>
                ${members.map(m => `
                  <tr>
                    <td><b>${esc(m.name || m.user_name || m.user_id)}</b></td>
                    <td>${esc(m.email || m.user_email || '—')}</td>
                    <td><span class="tag green">${esc(m.role)}</span></td>
                    <td class="muted">${fmtDate(m.assigned_at || m.created_at)}</td>
                    <td>
                      <button class="button small danger ghost" data-delmember="${esc(m.user_id || m.id)}">Remove</button>
                    </td>
                  </tr>
                `).join('') || '<tr><td colspan="5" class="empty">No members assigned to this tenant yet.</td></tr>'}
              </tbody>
            </table>
          </div>
        `,
        actions: [{ label: 'Close', primary: true, onClick: (close) => close() }]
      });

      const modalWrap = document.querySelector('.modal-backdrop');
      if (modalWrap) {
        modalWrap.querySelector('#btn-add-member').onclick = async () => {
          const userID = modalWrap.querySelector('#add-member-user').value;
          const role = modalWrap.querySelector('#add-member-role').value;
          if (!userID) { toast('Please select a user to add', 'warning'); return; }
          await api('POST', `/api/tenants/${encodeURIComponent(slug)}/members`, { user_id: userID, role });
          toast('Member added to tenant', 'success');
          modalWrap.remove();
          openTenantMembersModal(slug, name);
        };
        modalWrap.onclick = async (e) => {
          const delUserID = actionData(e).delmember;
          if (delUserID) {
            await api('DELETE', `/api/tenants/${encodeURIComponent(slug)}/members/${encodeURIComponent(delUserID)}`);
            toast('Member removed from tenant', 'warn');
            modalWrap.remove();
            openTenantMembersModal(slug, name);
          }
        };
      }
    } catch (err) {
      toast(esc(err.message), 'error');
    }
  }

  // -------------------------------------------------------------------------
  // VIEW: OIDC Identity Providers
  // -------------------------------------------------------------------------
  views.providers = async () => {
    const providers = await api('GET', '/api/admin/oidc-providers').catch(() => []);
    if (currentView !== 'providers') return;
    $('#view').innerHTML = `
      <div class="toolbar">
        <div class="muted">Federated Single Sign-On (SSO): Configure OpenID Connect Identity Providers (Okta, Entra ID, Auth0, Keycloak).</div>
        <div class="spacer"></div>
        <button class="button primary small" id="new-provider">Add OIDC provider</button>
      </div>

      <div class="card">
        <div class="card-header">
          <h2>Identity providers (${providers.length})</h2>
        </div>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Provider Name</th>
                <th>Issuer URL</th>
                <th>Client ID</th>
                <th>Allowed Domains</th>
                <th>Status</th>
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              ${providers.map(p => `
                <tr>
                  <td><b>${esc(p.name)}</b></td>
                  <td class="mono small">${esc(p.issuer)}</td>
                  <td class="mono small">${esc(p.client_id)}</td>
                  <td>${(p.allowed_domains || []).map(d => `<span class="tag gray small">${esc(d)}</span>`).join(' ') || '<span class="muted small">All domains</span>'}</td>
                  <td>${p.active ? '<span class="tag success">Active</span>' : '<span class="tag gray">Inactive</span>'}</td>
                  <td class="actions">
                    <button class="button small ghost" data-editprov="${esc(p.name)}">Edit</button>
                    <button class="button small danger ghost" data-delprov="${esc(p.name)}">Delete</button>
                  </td>
                </tr>
              `).join('') || '<tr><td colspan="6" class="empty">No OIDC identity providers configured yet.</td></tr>'}
            </tbody>
          </table>
        </div>
      </div>
    `;

    $('#new-provider').onclick = () => providerForm();

    $('#view').onclick = (e) => {
      const d = actionData(e);
      const editName = d.editprov;
      if (editName) {
        const p = providers.find(x => x.name === editName);
        if (p) providerForm(p);
        return;
      }
      const delName = d.delprov;
      if (delName) {
        confirmDialog('Delete OIDC Provider', `Permanently remove OIDC identity provider <b>${esc(delName)}</b>?`, async () => {
          await api('DELETE', `/api/admin/oidc-providers/${encodeURIComponent(delName)}`);
          toast('OIDC provider deleted', 'warn');
          views.providers();
        });
      }
    };
  };

  function providerForm(p = {}) {
    const isEdit = !!p.name;
    openForm({
      title: isEdit ? `Edit identity provider · ${p.name}` : 'Add identity provider',
      submitLabel: isEdit ? 'Save changes' : 'Add provider',
      fields: [
        { name: 'name', label: 'Identifier', value: p.name || '', required: true, disabled: isEdit, help: 'Unique slug, e.g. "okta-corp" or "azure-ad"' },
        { name: 'issuer', label: 'Issuer URL', value: p.issuer || '', required: true, help: 'Base URL hosting /.well-known/openid-configuration' },
        { name: 'client_id', label: 'Client ID', value: p.client_id || '', required: true },
        { name: 'client_secret', label: 'Client secret', type: 'password', value: p.client_secret || '', help: isEdit ? 'Leave blank to preserve existing secret' : 'IdP application secret' },
        { name: 'scopes', label: 'Scopes', value: p.scopes || 'openid profile email', help: 'Space-delimited OAuth2 scopes' },
        { name: 'default_role', label: 'Default role for new users', type: 'select', value: p.default_role || 'operator', options: ['developer', 'operator', 'admin', 'auditor'] },
        { name: 'allowed_domains', label: 'Allowed email domains', value: (p.allowed_domains || []).join(', '), help: 'Comma-separated domains, e.g. "acme.com, corp.internal"' },
        { name: 'active', label: 'Enabled for sign-in', type: 'checkbox', value: p.active ?? true }
      ],
      onSubmit: async (data, close) => {
        const payload = {
          name: data.name,
          issuer: data.issuer,
          client_id: data.client_id,
          scopes: data.scopes || 'openid profile email',
          default_role: data.default_role || 'operator',
          active: data.active === true || data.active === 'true' || data.active === 'on',
          allowed_domains: data.allowed_domains ? data.allowed_domains.split(',').map(s => s.trim()).filter(Boolean) : []
        };
        if (data.client_secret) {
          payload.client_secret = data.client_secret;
        }
        await api('PUT', `/api/admin/oidc-providers/${encodeURIComponent(data.name)}`, payload);
        toast(isEdit ? 'OIDC provider updated' : 'OIDC provider configured', 'success');
        close();
        views.providers();
      }
    });
  }

  // -------------------------------------------------------------------------
  // VIEW: Approvals
  // -------------------------------------------------------------------------
  function isSignupRequest(user) {
    return !user.active && user.sso_provider === 'local' && !user.last_login_at;
  }

  views.approvals = async () => {
    const [pending, identity, mcpPending] = await Promise.all([
      api('GET', '/api/subscriptions/pending'),
      api('GET', '/api/auth/me'),
      api('GET', '/api/mcp/catalog?status=pending').catch(() => [])
    ]);
    const canApproveAccounts = identity.user?.role === 'superadmin';
    const accountRequests = canApproveAccounts ? (await api('GET', '/api/admin/users')).filter(isSignupRequest) : [];
    if (currentView !== 'approvals') return;
    $('#view').innerHTML = `
      <div class="toolbar">
        <div class="muted">Review account signup requests and API subscription requests before granting access.</div>
        <div class="spacer"></div>
        <button class="button small" id="refresh-approvals">Refresh</button>
      </div>

      <div class="card">
        <div class="card-header"><h2>Account access requests (${accountRequests.length})</h2></div>
        <p class="muted small" style="padding:0 20px">Approve a registered account to allow email and password sign-in. Its assigned role is preserved; manage roles in <a href="#/team">Users &amp; roles</a>.</p>
        ${canApproveAccounts ? `<div class="table-wrap"><table>
          <thead><tr><th>Name / Email</th><th>Team</th><th>Assigned role</th><th>Requested at</th><th>Action</th></tr></thead>
          <tbody>${accountRequests.map(user => `<tr>
            <td><b>${esc(user.name)}</b><div class="muted small">${esc(user.email)}</div></td>
            <td>${esc(user.team)}</td><td><span class="tag">${esc(user.role)}</span></td>
            <td class="muted">${fmtDate(user.created_at)}</td>
            <td><button class="button small primary" data-account-approve="${esc(user.id)}">Approve account</button></td>
          </tr>`).join('') || '<tr><td colspan="5" class="empty">No account signup requests awaiting approval.</td></tr>'}</tbody>
        </table></div>` : '<p class="muted" style="padding:0 20px 20px">A platform administrator (superadmin) must review account requests.</p>'}
      </div>

      <div class="card">
        <div class="card-header"><h2>MCP tools &amp; prompts held for review (${mcpPending.length})</h2></div>
        <p class="muted small" style="padding:0 20px">A changed or new definition on an MCP API with an approved catalog. Calls to it are refused on every gateway until you approve it. Read the definition first: a changed description can carry instructions to the model.</p>
        <div class="table-wrap"><table>
          <thead><tr><th>API</th><th>Definition</th><th>Change</th><th>Seen</th><th>Actions</th></tr></thead>
          <tbody>${mcpPending.map((m) => `<tr>
            <td><span class="tag blue">${esc(m.api_name)}</span></td>
            <td><span class="tag gray">${esc(m.kind)}</span> <b class="mono">${esc(m.name)}</b>
              <div class="muted small" style="max-width:420px">${esc(String((m.definition || {}).description || '').slice(0, 160))}</div></td>
            <td>${m.pinned ? '<span class="tag error">changed since approval</span>' : '<span class="tag warn">new</span>'}</td>
            <td class="muted small">${fmtDate(m.first_seen)}<div>by ${esc(m.seen_by || 'gateway')}</div></td>
            <td class="actions"><button class="button small primary" data-mcp-review="${esc(m.id)}">Review</button></td>
          </tr>`).join('') || '<tr><td colspan="5" class="empty">No MCP definitions awaiting review.</td></tr>'}</tbody>
        </table></div>
      </div>

      <div class="card">
        <div class="card-header">
          <h2>Pending subscriptions (${pending.length})</h2>
        </div>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Consumer / Application</th>
                <th>Target API</th>
                <th>Requested Plan</th>
                <th>Requested At</th>
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              ${pending.map((s) => `
                <tr>
                  <td><b>${esc(s.consumer_name)}</b></td>
                  <td><span class="tag blue">${esc(s.api_name)}</span></td>
                  <td>${esc(s.plan_name || 'API Default')}</td>
                  <td class="muted">${fmtDate(s.created_at)}</td>
                  <td class="actions">
                    <button class="button small primary" data-approve="${s.id}">Approve</button>
                    <button class="button small danger" data-reject="${s.id}">Reject</button>
                  </td>
                </tr>
              `).join('') || '<tr><td colspan="5" class="empty">No API subscription requests awaiting approval.</td></tr>'}
            </tbody>
          </table>
        </div>
      </div>
    `;

    $('#refresh-approvals').onclick = () => views.approvals();
    $('#view').onclick = (e) => {
      const reviewID = e.target.closest('[data-mcp-review]')?.dataset.mcpReview;
      if (reviewID) {
        const entry = mcpPending.find((m) => m.id === reviewID);
        if (entry) openMCPReviewModal(entry);
        return;
      }
      const accountID = e.target.closest('[data-account-approve]')?.dataset.accountApprove;
      if (accountID) {
        const user = accountRequests.find(user => user.id === accountID);
        if (!user) return;
        confirmDialog('Approve account access', `Allow <b>${esc(user.email)}</b> to sign in with the <b>${esc(user.role)}</b> role?`, async () => {
          await api('PUT', `/api/admin/users/${encodeURIComponent(user.id)}`, {
            name: user.name, email: user.email, team: user.team, role: user.role, active: true
          });
          toast('Account approved — email and password sign-in is now available.', 'success');
          await views.approvals();
          await updateBadges();
        }, 'Approve account');
        return;
      }
      const d = actionData(e);
      const app = d.approve;
      const rej = d.reject;
      if (app) {
        api('POST', `/api/subscriptions/${app}/approve`).then(() => {
          toast('Subscription approved — developer access granted immediately', 'success');
          views.approvals();
        }).catch((err) => toast(esc(err.message), 'error'));
      }
      if (rej) {
        api('POST', `/api/subscriptions/${rej}/reject`).then(() => {
          toast('Subscription rejected', 'warn');
          views.approvals();
        }).catch((err) => toast(esc(err.message), 'error'));
      }
    };
  };

  // -------------------------------------------------------------------------
  // VIEW: Request logs & Logs
  // -------------------------------------------------------------------------
  // -------------------------------------------------------------------------
  // VIEW: Refusals: what the gateway refused by policy, and why, per protocol
  // -------------------------------------------------------------------------
  const refusalFilter = { window: '24h', protocol: '' };
  const windowLabels = { '5m': 'Last 5 minutes', '15m': 'Last 15 minutes', '1h': 'Last hour', '6h': 'Last 6 hours', '24h': 'Last 24 hours', '7d': 'Last 7 days', '30d': 'Last 30 days' };
  const windowOptions = (values, current) => values.map((w) => `<option value="${w}" ${current === w ? 'selected' : ''}>${windowLabels[w] || w}</option>`).join('');
  // Gateway error messages repeat the reason code ("missing_api_key: provide ...").
  const withoutReasonPrefix = (msg, reason) => (msg && reason && msg.startsWith(reason + ': ') ? msg.slice(reason.length + 2) : msg || '');
  views.refusals = async () => {
    const q = new URLSearchParams({ window: refusalFilter.window, protocol: refusalFilter.protocol });
    const data = await api('GET', '/api/analytics/refusals?' + q.toString());
    if (currentView !== 'refusals') return;
    const protoTag = (p) => `<span class="tag ${{ graphql: 'warn', grpc: 'blue', mcp: 'ai', http: 'gray' }[p] || 'gray'}">${esc(p === 'http' ? 'HTTP' : p === 'mcp' ? 'MCP' : p === 'grpc' ? 'gRPC' : 'GraphQL')}</span>`;
    const total = data.by_reason.reduce((n, r) => n + r.count, 0);
    $('#view').innerHTML = `
      <div class="toolbar">
        <div class="filter-group" id="ref-proto">
          ${[['', 'All'], ['http', 'HTTP'], ['graphql', 'GraphQL'], ['grpc', 'gRPC'], ['mcp', 'MCP']].map(([v, l]) => `<button class="filter ${refusalFilter.protocol === v ? 'active' : ''}" data-proto="${v}">${l}</button>`).join('')}
        </div>
        <div class="spacer"></div>
        <select id="ref-window" aria-label="Time window">${windowOptions(['1h', '6h', '24h', '7d'], refusalFilter.window)}</select>
      </div>
      <p class="muted">Requests the gateway refused by policy (authentication, limits, protocol inspection, MCP governance, WASM plugins), not upstream failures. ${Math.round(total)} in the last ${esc(data.window)}${refusalFilter.protocol ? ' for ' + esc(refusalFilter.protocol) : ''}.</p>
      <div class="card">
        <div class="card-header"><h2>By reason</h2></div>
        <div class="table-wrap"><table>
          <thead><tr><th>Protocol</th><th>Reason</th><th>API</th><th>Requests</th><th>Last seen</th></tr></thead>
          <tbody>${data.by_reason.map((r) => `<tr>
            <td>${protoTag(r.protocol)}${r.wasm ? ' <span class="tag gray">WASM</span>' : ''}</td>
            <td class="mono">${esc(r.reason)}</td><td>${esc(r.api_name || '—')}</td>
            <td>${fmtNum(Math.round(r.count))}</td><td class="muted">${fmtDate(r.last_seen)}</td></tr>`).join('') || '<tr><td colspan="5" class="empty">Nothing refused in this window.</td></tr>'}</tbody>
        </table></div>
      </div>
      <div class="card">
        <div class="card-header"><h2>Recent refusals</h2></div>
        <div class="table-wrap"><table>
          <thead><tr><th>Time</th><th>Protocol</th><th>API / route</th><th>Status</th><th>Why</th><th></th></tr></thead>
          <tbody>${data.recent.map((r, i) => `<tr>
            <td class="muted">${fmtTime(r.ts)}</td><td>${protoTag(r.protocol)}</td>
            <td>${esc(r.api_name || '—')}<div class="mono small muted">${esc(r.matched_route || (r.method + ' ' + r.path))}</div></td>
            <td>${statusPill(r.status)}</td>
            <td><span class="mono">${esc(r.reason)}</span><div class="small muted" style="max-width:420px">${esc(withoutReasonPrefix(r.error, r.reason))}</div></td>
            <td class="nowrap"><button class="button small" data-trail="${i}">Decision trail</button></td></tr>`).join('') || '<tr><td colspan="6" class="empty">No recent refusals.</td></tr>'}</tbody>
        </table></div>
      </div>`;
    $('#ref-window').onchange = (e) => { refusalFilter.window = e.target.value; views.refusals(); };
    $$('#ref-proto [data-proto]').forEach((b) => { b.onclick = () => { refusalFilter.protocol = b.dataset.proto; views.refusals(); }; });
    $('#view').onclick = (e) => {
      const i = e.target.closest('[data-trail]')?.dataset.trail;
      if (i == null) return;
      const r = data.recent[Number(i)];
      openModal({
        title: `Why ${r.method} ${r.path} was refused`,
        large: true,
        body: `<p class="muted">${esc(r.api_name)} · request <span class="mono">${esc(r.request_id)}</span> · ${fmtDate(r.ts)}</p>
          <pre class="mono small" style="white-space:pre-wrap;max-height:480px;overflow:auto">${esc(JSON.stringify(r.policy_evaluations || {}, null, 2))}</pre>`,
        actions: []
      });
    };
  };

  const logFilter = { api_id: '', status: '', q: '', auto: true };
  views.logs = async (_, soft) => {
    const apis = await api('GET', '/api/apis');
    if (!soft || !$('#log-table')) {
      if (currentView !== 'logs') return;
      $('#view').innerHTML = `
        <div class="toolbar">
          <select id="lf-api">
            <option value="">All APIs</option>
            ${apis.map((a) => `<option value="${a.id}">${esc(a.name)}</option>`).join('')}
          </select>
          <select id="lf-status">
            <option value="">All Statuses</option>
            <option value="2xx">2xx Success</option>
            <option value="3xx">3xx Redirect</option>
            <option value="4xx">4xx Client Errors</option>
            <option value="5xx">5xx Gateway Errors</option>
            <option value="errors">All Errors (≥400)</option>
          </select>
          <div class="search-box">
            <span class="muted"></span>
            <input id="lf-q" placeholder="Search path, request ID, error, or decision…">
          </div>
          <div class="field check" style="margin:0">
            <input type="checkbox" id="lf-auto" checked>
            <label for="lf-auto" class="muted small">Auto-refresh</label>
          </div>
          <div class="spacer"></div>
          <button class="button small" id="lf-refresh">Refresh</button>
        </div>

        <div class="card">
          <div class="table-wrap">
            <table id="log-table">
              <thead>
                <tr>
                  <th>Timestamp</th>
                  <th>Method</th>
                  <th>Path</th>
                  <th>Status</th>
                  <th>Latency</th>
                  <th>API</th>
                  <th>Consumer</th>
                  <th>Client IP</th>
                  <th>Decision Reason</th>
                </tr>
              </thead>
              <tbody id="log-body"></tbody>
            </table>
          </div>
        </div>
      `;

      $('#lf-api').value = logFilter.api_id;
      $('#lf-status').value = logFilter.status;
      $('#lf-q').value = logFilter.q;
      $('#lf-auto').checked = logFilter.auto;

      const reload = () => {
        logFilter.api_id = $('#lf-api').value;
        logFilter.status = $('#lf-status').value;
        logFilter.q = $('#lf-q').value.trim();
        loadLogs();
      };
      $('#lf-api').onchange = reload;
      $('#lf-status').onchange = reload;
      $('#lf-refresh').onclick = reload;
      let deb;
      $('#lf-q').oninput = () => { clearTimeout(deb); deb = setTimeout(reload, 300); };
      $('#lf-auto').onchange = (e) => { logFilter.auto = e.target.checked; };

      viewTimer = setInterval(() => {
        if (currentView === 'logs' && logFilter.auto && !document.hidden) loadLogs();
      }, 10000);

      $('#log-body').onclick = (e) => {
        const tr = e.target.closest('tr[data-req-id]');
        if (!tr) return;
        const reqId = tr.dataset.reqId;
        const item = currentLogs.find((x) => x.request_id === reqId);
        if (item) openDecisionModal(item);
      };
    }
    await loadLogs();
  };

  let currentLogs = [];
  async function loadLogs() {
    const p = new URLSearchParams({ limit: 50 });
    for (const k of ['api_id', 'status', 'q']) if (logFilter[k]) p.set(k, logFilter[k]);
    const logs = await api('GET', '/api/logs?' + p);
    currentLogs = logs || [];
    const tb = $('#log-body');
    if (!tb) return;
    tb.innerHTML = currentLogs.length ? currentLogs.map((l) => `
      <tr data-req-id="${esc(l.request_id)}" style="cursor:pointer">
        <td class="mono muted">${fmtTime(l.ts)}</td>
        <td>${methodBadge(l.method)}</td>
        <td class="mono">${esc(l.path.length > 36 ? l.path.slice(0, 36) + '…' : l.path)}</td>
        <td>${statusPill(l.status)}</td>
        <td class="mono">${fmtMs(l.latency_ms)}</td>
        <td>${esc(l.api_name) || '<span class="muted">–</span>'}</td>
        <td>${esc(l.consumer_name) || '<span class="muted">–</span>'}</td>
        <td class="mono muted hide-mobile">${esc(l.client_ip)}</td>
        <td>
          <span class="tag ${l.decision_reason === 'proxied_successfully' ? 'success' : 'warn'}">
            ${esc(l.decision_reason || 'proxied')}
          </span>
        </td>
      </tr>
    `).join('') : '<tr><td colspan="9" class="empty">No requests match query filters.</td></tr>';
  }

  // -------------------------------------------------------------------------
  // VIEW: Consumers & Key Management
  // -------------------------------------------------------------------------
  views.consumers = async (selectedId) => {
    const consumers = await api('GET', '/api/consumers');
    if (currentView !== 'consumers') return;
    $('#view').innerHTML = `
      <div class="toolbar">
        <div class="muted">Applications, partner systems, and API clients governed by RelayOps.</div>
        <div class="spacer"></div>
        <button class="button primary" id="new-consumer">New consumer</button>
      </div>

      <div class="card">
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Consumer Name</th>
                <th>Contact Email</th>
                <th>Status</th>
                <th>Active Keys</th>
                <th>Subscriptions</th>
                <th>Created</th>
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              ${consumers.map((c) => `
                <tr ${c.id === selectedId ? 'style="background:#f0f7ef"' : ''}>
                  <td><a href="#/consumers/${c.id}"><b>${esc(c.name)}</b></a></td>
                  <td class="muted">${esc(c.email)}</td>
                  <td><span class="tag ${c.status === 'active' ? 'success' : 'warn'}">${esc(c.status || 'active')}</span></td>
                  <td>${c.key_count}</td>
                  <td>${c.subscription_count}</td>
                  <td class="muted">${fmtDate(c.created_at)}</td>
                  <td class="actions">
                    <a class="button small" href="#/consumers/${c.id}">Manage Keys & Subs</a>
                    <button class="button small danger" data-del="${c.id}">Delete</button>
                  </td>
                </tr>
              `).join('') || '<tr><td colspan="7" class="empty">No consumers registered yet.</td></tr>'}
            </tbody>
          </table>
        </div>
      </div>
      <div id="consumer-detail" style="margin-top:24px"></div>
    `;

    $('#new-consumer').onclick = () => openForm({
      title: 'New consumer',
      submitLabel: 'Create consumer',
      fields: [
        { name: 'name', label: 'Application / Consumer Name', placeholder: 'acme-logistics-app', required: true },
        { name: 'email', label: 'Contact Email', type: 'email', placeholder: 'developer@acme.com', required: true }
      ],
      onSubmit: async (v) => {
        const c = await api('POST', '/api/consumers', v);
        location.hash = `#/consumers/${c.id}`;
      }
    });

    $('#view').onclick = (e) => {
      const delId = actionData(e).del;
      if (delId) {
        const c = consumers.find((x) => x.id === delId);
        confirmDialog(
          'Delete Consumer',
          `Permanently delete consumer <b>${esc(c.name)}</b>? All associated API keys will immediately stop working.`,
          async () => {
            await api('DELETE', `/api/consumers/${c.id}`);
            if (selectedId === c.id) location.hash = '#/consumers';
          }
        );
      }
    };

    if (selectedId && consumers.some((c) => c.id === selectedId)) {
      await renderConsumerDetail(consumers.find((c) => c.id === selectedId));
    }
  };

  async function renderConsumerDetail(c) {
    const [keys, subs, apis, plans] = await Promise.all([
      api('GET', `/api/consumers/${c.id}/keys`),
      api('GET', '/api/subscriptions'),
      api('GET', '/api/apis'),
      api('GET', '/api/plans')
    ]);
    const mySubs = subs.filter((s) => s.consumer_id === c.id);
    const el = $('#consumer-detail');
    if (!el) return;

    el.innerHTML = `
      <div class="grid consumer-detail-grid">
        <div class="card">
          <div class="card-header">
            <h2>API keys · ${esc(c.name)}</h2>
            <button class="button small primary" id="new-key">Issue key</button>
          </div>
          <div class="table-wrap">
            <table>
              <thead><tr><th>Label</th><th>Prefix</th><th>Status</th><th>Created</th><th>Actions</th></tr></thead>
              <tbody>
                ${keys.map((k) => `
                  <tr>
                    <td><b>${esc(k.name)}</b></td>
                    <td class="mono">${esc(k.key_prefix)}…</td>
                    <td>${k.active ? '<span class="tag success">Active</span>' : '<span class="tag error">Revoked</span>'}</td>
                    <td class="muted">${fmtDate(k.created_at)}</td>
                    <td class="actions">
                      ${k.active
                        ? `<button class="button small" data-revoke="${k.id}">Revoke</button>`
                        : `<button class="button small" data-activate="${k.id}">Activate</button>`}
                      <button class="button small danger" data-delkey="${k.id}">Delete</button>
                    </td>
                  </tr>
                `).join('') || '<tr><td colspan="5" class="empty">No active keys issued for this consumer.</td></tr>'}
              </tbody>
            </table>
          </div>
        </div>

        <div class="card">
          <div class="card-header">
            <h2>Subscriptions · ${esc(c.name)}</h2>
            <button class="button small primary" id="new-sub">Subscribe to API</button>
          </div>
          <div class="table-wrap">
            <table>
              <thead><tr><th>API</th><th>Plan</th><th>State</th><th>Actions</th></tr></thead>
              <tbody>
                ${mySubs.map((s) => `
                  <tr>
                    <td><b>${esc(s.api_name)}</b></td>
                    <td>${esc(s.plan_name) || '<span class="muted">API Default</span>'}</td>
                    <td>${s.active ? '<span class="tag success">Active</span>' : '<span class="tag gray">suspended</span>'}</td>
                    <td class="actions">
                      <button class="button small danger" data-delsub="${s.id}">Remove</button>
                    </td>
                  </tr>
                `).join('') || '<tr><td colspan="4" class="empty">Consumer is not subscribed to any API.</td></tr>'}
              </tbody>
            </table>
          </div>
        </div>
      </div>
    `;

    $('#new-key').onclick = () => openForm({
      title: 'Issue New API Key',
      submitLabel: 'Generate Key',
      fields: [{ name: 'name', label: 'Key Label / Environment', value: 'production', required: true }],
      onSubmit: async (v) => {
        const k = await api('POST', `/api/consumers/${c.id}/keys`, v);
        openModal({
          title: 'API Key Issued Successfully',
          body: `
            <div class="callout warning">
              Copy this API key now. It is stored securely as a SHA-256 hash and cannot be recovered again.
            </div>
            <div class="secret-box" id="raw-key">${esc(k.key)}</div>
            <div class="muted small">Usage header: <code class="mono">X-API-Key: ${esc(k.key)}</code></div>
          `,
          actions: [{
            label: 'Copy Key to Clipboard',
            primary: true,
            onClick: (close) => {
              navigator.clipboard?.writeText(k.key);
              toast('API key copied to clipboard!', 'success');
              close();
              renderConsumerDetail(c);
            }
          }]
        });
      }
    });

    $('#new-sub').onclick = () => {
      const avail = apis.filter((a) => !mySubs.some((s) => s.api_id === a.id));
      if (!avail.length) { toast('Already subscribed to all available APIs', 'warn'); return; }
      openForm({
        title: `Subscribe ${c.name} to API`,
        submitLabel: 'Create Subscription',
        fields: [
          { name: 'api_id', label: 'Select API', type: 'select', options: avail.map((a) => ({ value: a.id, label: `${a.name} (${a.base_path})` })) },
          { name: 'plan_id', label: 'Plan & Quota', type: 'select', options: [{ value: '', label: 'No plan (use API default limits)' }, ...plans.map((p) => ({ value: p.id, label: `${p.name} (${limitText(p.rate_limit_per_minute)})` }))] }
        ],
        onSubmit: async (v) => {
          await api('POST', '/api/subscriptions', { consumer_id: c.id, ...v });
          toast('Subscription created and applied live!', 'success');
          renderConsumerDetail(c);
        }
      });
    };

    el.onclick = (e) => {
      const d = actionData(e);
      if (d.revoke) api('POST', `/api/keys/${d.revoke}/revoke`).then(() => { toast('Key revoked immediately', 'warn'); renderConsumerDetail(c); });
      if (d.activate) api('POST', `/api/keys/${d.activate}/activate`).then(() => { toast('Key reactivated', 'success'); renderConsumerDetail(c); });
      if (d.delkey) confirmDialog('Delete Key', 'Permanently delete this API key?', async () => { await api('DELETE', `/api/keys/${d.delkey}`); renderConsumerDetail(c); });
      if (d.delsub) confirmDialog('Remove Subscription', 'Remove this API subscription?', async () => { await api('DELETE', `/api/subscriptions/${d.delsub}`); renderConsumerDetail(c); });
    };
  }

  // -------------------------------------------------------------------------
  // VIEW: Subscriptions
  // -------------------------------------------------------------------------
  views.subscriptions = async () => {
    const [subs, plans, apis, consumers] = await Promise.all([
      api('GET', '/api/subscriptions'),
      api('GET', '/api/plans'),
      api('GET', '/api/apis'),
      api('GET', '/api/consumers')
    ]);
    const planOpts = (sel) => `<option value="">API default</option>` + plans.map((p) => `<option value="${p.id}" ${p.id === sel ? 'selected' : ''}>${esc(p.name)} (${limitText(p.rate_limit_per_minute)})</option>`).join('');

    if (currentView !== 'subscriptions') return;
    $('#view').innerHTML = `
      <div class="toolbar">
        <div class="muted">Active subscriptions linking consumer applications to APIs under specific plans.</div>
        <div class="spacer"></div>
        <button class="button primary" id="new-sub">New subscription</button>
      </div>

      <div class="card">
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Consumer</th>
                <th>API</th>
                <th>Assigned Plan</th>
                <th>Status</th>
                <th>Subscribed Since</th>
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              ${subs.map((s) => `
                <tr>
                  <td><a href="#/consumers/${s.consumer_id}"><b>${esc(s.consumer_name)}</b></a></td>
                  <td><b>${esc(s.api_name)}</b></td>
                  <td><select data-plan="${s.id}">${planOpts(s.plan_id)}</select></td>
                  <td>${s.active ? '<span class="tag success">Active</span>' : '<span class="tag gray">suspended</span>'}</td>
                  <td class="muted">${fmtDate(s.created_at)}</td>
                  <td class="actions">
                    <button class="button small" data-toggle="${s.id}">${s.active ? 'Suspend' : 'Resume'}</button>
                    <button class="button small danger" data-del="${s.id}">Remove</button>
                  </td>
                </tr>
              `).join('') || '<tr><td colspan="6" class="empty">No subscriptions established yet.</td></tr>'}
            </tbody>
          </table>
        </div>
      </div>
    `;

    $('#new-sub').onclick = () => {
      if (!consumers.length || !apis.length) { toast('Create at least one consumer and one API first', 'warn'); return; }
      openForm({
        title: 'New subscription',
        submitLabel: 'Subscribe',
        fields: [
          { name: 'consumer_id', label: 'Consumer', type: 'select', options: consumers.map((c) => ({ value: c.id, label: c.name })) },
          { name: 'api_id', label: 'Target API', type: 'select', options: apis.map((a) => ({ value: a.id, label: `${a.name} (${a.base_path})` })) },
          { name: 'plan_id', label: 'Plan', type: 'select', options: [{ value: '', label: 'API Default Limit' }, ...plans.map((p) => ({ value: p.id, label: `${p.name} (${limitText(p.rate_limit_per_minute)})` }))] }
        ],
        onSubmit: (v) => api('POST', '/api/subscriptions', v)
      });
    };

    $('#view').onchange = (e) => {
      const id = e.target.closest('[data-plan]')?.dataset.plan;
      if (!id) return;
      const s = subs.find((x) => x.id === id);
      api('PUT', `/api/subscriptions/${id}`, { plan_id: e.target.value, active: s.active })
        .then(() => toast('Plan changed — new quota enforced in memory and Redis immediately', 'success'))
        .catch((err) => toast(esc(err.message), 'error'));
    };

    $('#view').onclick = (e) => {
      const d = actionData(e);
      const s = subs.find((x) => x.id === (d.toggle || d.del));
      if (!s) return;
      if (d.toggle) {
        api('PUT', `/api/subscriptions/${s.id}`, { plan_id: s.plan_id || '', active: !s.active })
          .then(() => route(true));
      }
      if (d.del) {
        confirmDialog('Remove Subscription', `Revoke <b>${esc(s.consumer_name)}</b>'s access to <b>${esc(s.api_name)}</b>?`, () => api('DELETE', `/api/subscriptions/${s.id}`));
      }
    };
  };

  // -------------------------------------------------------------------------
  // VIEW: Plans
  // -------------------------------------------------------------------------
  views.plans = async () => {
    const plans = await api('GET', '/api/plans');
    if (currentView !== 'plans') return;
    $('#view').innerHTML = `
      <div class="toolbar">
        <div class="muted">Plans define rate limits and daily/monthly quotas enforced distributedly via Redis.</div>
        <div class="spacer"></div>
        <button class="button primary" id="new-plan">New plan</button>
      </div>

      <div class="card">
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Plan Name</th>
                <th>Description</th>
                <th>Rate Limit (min)</th>
                <th>Daily Quota</th>
                <th>Monthly Quota</th>
                <th>Created</th>
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              ${plans.map((p) => `
                <tr>
                  <td><b>${esc(p.name)}</b></td>
                  <td class="muted">${esc(p.description || '–')}</td>
                  <td>${limitText(p.rate_limit_per_minute)}</td>
                  <td>${p.quota_per_day > 0 ? fmtNum(p.quota_per_day) + '/day' : 'unlimited'}</td>
                  <td>${p.quota_per_month > 0 ? fmtNum(p.quota_per_month) + '/month' : 'unlimited'}</td>
                  <td class="muted">${fmtDate(p.created_at)}</td>
                  <td class="actions">
                    <button class="button small" data-edit="${p.id}">Edit</button>
                    <button class="button small danger" data-del="${p.id}">Delete</button>
                  </td>
                </tr>
              `).join('') || '<tr><td colspan="7" class="empty">No plans defined yet.</td></tr>'}
            </tbody>
          </table>
        </div>
      </div>
    `;

    const form = (p) => openForm({
      title: p ? `Edit plan · ${p.name}` : 'New plan',
      fields: [
        { name: 'name', label: 'Plan Name', value: p?.name, required: true },
        { name: 'description', label: 'Description', value: p?.description },
        [
          { name: 'rate_limit_per_minute', label: 'Rate Limit (req/min)', type: 'number', value: p?.rate_limit_per_minute ?? 60, help: '0 = unlimited' },
          { name: 'quota_per_day', label: 'Daily Quota', type: 'number', value: p?.quota_per_day ?? 0, help: '0 = unlimited' }
        ],
        { name: 'quota_per_month', label: 'Monthly Quota', type: 'number', value: p?.quota_per_month ?? 0, help: '0 = unlimited' }
      ],
      onSubmit: (v) => (p ? api('PUT', `/api/plans/${p.id}`, v) : api('POST', '/api/plans', v))
    });

    $('#new-plan').onclick = () => form();
    $('#view').onclick = (e) => {
      const d = actionData(e);
      const p = plans.find((x) => x.id === (d.edit || d.del));
      if (!p) return;
      if (d.edit) form(p);
      else confirmDialog('Delete Plan', `Delete plan <b>${esc(p.name)}</b>? Subscriptions using it will revert to API defaults.`, () => api('DELETE', `/api/plans/${p.id}`));
    };
  };

  // -------------------------------------------------------------------------
  // VIEW: Analytics
  // -------------------------------------------------------------------------
  let anWindow = '1h';
  views.analytics = async () => {
    if (currentView !== 'analytics') return;
    $('#view').innerHTML = `
      <div class="toolbar">
        <select id="an-window">
          ${windowOptions(['5m', '15m', '1h', '6h', '24h', '7d'], anWindow)}
        </select>
        <span class="muted small">Aggregated from gateway request logs stored in PostgreSQL</span>
        <div class="spacer"></div>
        <button class="button small" id="an-refresh">Refresh</button>
      </div>
      <div id="an-body"><div class="muted">Loading telemetry data…</div></div>
    `;

    $('#an-window').onchange = (e) => { anWindow = e.target.value; loadAnalytics(); };
    $('#an-refresh').onclick = loadAnalytics;
    await loadAnalytics();
    viewTimer = setInterval(() => { if (currentView === 'analytics' && !document.hidden) loadAnalytics(); }, 30000);
  };

  async function loadAnalytics() {
    const s = await api('GET', '/api/analytics/summary?window=' + anWindow);
    if(currentView!=='analytics') return;
    const errRate = s.total ? ((s.errors_5xx / s.total) * 100).toFixed(2) : '0.00';
    const showTokens = typeof previewAI === 'function' && previewAI();
    const topTable = (items, withTokens = false) => `
      <table>
        <thead><tr><th>Name</th><th>Requests</th><th>Errors</th><th>Avg Latency</th>${withTokens ? '<th>Tokens</th>' : ''}</tr></thead>
        <tbody>
          ${(Array.isArray(items) ? items : []).map((t) => `
            <tr>
              <td><b>${esc(t.name)}</b></td>
              <td>${fmtNum(t.count)}</td>
              <td>${t.errors ? `<span style="color:var(--red)">${fmtNum(t.errors)}</span>` : 0}</td>
              <td class="mono">${fmtMs(t.avg_latency_ms)}</td>
              ${withTokens ? `<td class="mono" style="color:var(--green);font-weight:600">${fmtNum(t.tokens)}</td>` : ''}
            </tr>
          `).join('') || `<tr><td colspan="${withTokens ? 5 : 4}" class="empty">No activity recorded in this time window.</td></tr>`}
        </tbody>
      </table>`;

    const el = $('#an-body');
    if (!el) return;
    el.innerHTML = `
      <div class="grid kpis">
        <div class="card kpi"><div class="label">Total Requests</div><div class="value">${fmtNum(s.total)}</div><div class="sub">in last ${esc(s.window)}</div></div>
        <div class="card kpi"><div class="label">5xx Error Rate</div><div class="value">${errRate}%</div><div class="sub">${fmtNum(s.errors_5xx)} 5xx · ${fmtNum(s.errors_4xx)} 4xx</div></div>
        <div class="card kpi"><div class="label">p50 / p95 Latency</div><div class="value" style="font-size:22px">${fmtMs(s.p50_latency_ms)} / ${fmtMs(s.p95_latency_ms)}</div><div class="sub">p99 ${fmtMs(s.p99_latency_ms)}</div></div>
        <div class="card kpi"><div class="label">Average Latency</div><div class="value">${fmtMs(s.avg_latency_ms)}</div><div class="sub">round-trip duration</div></div>
        ${showTokens ? `<div class="card kpi"><div class="label">LLM Tokens Metered</div><div class="value" style="color:var(--green)">${fmtNum(s.total_tokens || 0)}</div><div class="sub">AI inference ledger</div></div>` : ''}
      </div>
      <div class="grid two">
        <div class="card">
          <div class="card-header"><h2>Traffic over time <span class="legend"><span><i style="background:#235f4a"></i>requests</span><span><i style="background:#dc2626"></i>5xx</span></span></h2></div>
          <canvas class="chart" id="an-c1"></canvas>
        </div>
        <div class="card">
          <div class="card-header"><h2>Average latency</h2></div>
          <canvas class="chart" id="an-c2"></canvas>
        </div>
      </div>
      <div class="grid two" style="margin-top:20px">
        <div class="card"><div class="card-header"><h2>Top APIs</h2></div><div class="table-wrap">${topTable(s.top_apis, showTokens)}</div></div>
        <div class="card"><div class="card-header"><h2>Top consumers</h2></div><div class="table-wrap">${topTable(s.top_consumers, showTokens)}</div></div>
      </div>
      ${showTokens ? `<div class="card" style="margin-top:20px"><div class="card-header"><h2>Top AI models</h2></div><div class="table-wrap">${topTable(s.top_models, true)}</div></div>` : ''}
    `;

    const labels = (s.series || []).map((p) => new Date(p.bucket).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false }));
    drawChart($('#an-c1'), [
      { values: (s.series || []).map((p) => p.count), color: '#235f4a', fill: true },
      { values: (s.series || []).map((p) => p.errors), color: '#dc2626' }
    ], { labels });
    drawChart($('#an-c2'), [
      { values: (s.series || []).map((p) => (p.count ? p.avg_latency_ms : null)), color: '#0891b2', fill: true }
    ], { labels, yFmt: (v) => v.toFixed(v < 10 ? 1 : 0) + 'ms' });
  }

  // -------------------------------------------------------------------------
  // VIEW: Audit log
  // -------------------------------------------------------------------------
 // auditSummary shows the first fields of an audit record as readable
  // pairs, with the full record one click away.
  // clientLabel turns a User-Agent into "Chrome on Windows", "relayopsctl",
  // "curl" and so on; the raw string stays in the cell's tooltip.
  function clientLabel(ua) {
    if (!ua) return 'Internal';
    const browser = /Edg\//.test(ua) ? 'Edge' : /OPR\//.test(ua) ? 'Opera' : /Firefox\//.test(ua) ? 'Firefox'
      : /Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : '';
    const os = /Windows/.test(ua) ? 'Windows' : /iPhone|iPad/.test(ua) ? 'iOS' : /Mac OS X/.test(ua) ? 'macOS'
      : /Android/.test(ua) ? 'Android' : /Linux/.test(ua) ? 'Linux' : '';
    const known = ua.match(/\b(WindowsPowerShell|PowerShell|relayopsctl|curl|Wget|Go-http-client|python-requests|Postman|insomnia|k6|node-fetch|axios|okhttp)\b/i);
    if (known) return known[1] === 'WindowsPowerShell' ? 'PowerShell' : known[1];
    if (browser) return os ? `${browser} on ${os}` : browser;
    if (ua.startsWith('Mozilla/')) { const products = ua.match(/([A-Za-z][\w.-]*)\/[\d.]+/g) || []; const last = products.at(-1)?.split('/')[0]; if (last && last !== 'Mozilla') return last; }
    const tool = ua.split(/[\/\s]/)[0];
    return tool.length > 24 ? tool.slice(0, 24) + '…' : tool;
  }

  function auditSummary(obj) {
    const entries = Object.entries(obj || {});
    if (!entries.length) return '<span class="muted">—</span>';
    const show = (v) => {
      const t = typeof v === 'string' ? v : JSON.stringify(v);
      return t.length > 90 ? t.slice(0, 90) + '…' : t;
    };
    const rows = entries.slice(0, 4).map(([k, v]) => `<div class="kv"><span class="k">${esc(k.replace(/_/g, ' '))}</span><span class="v mono">${esc(show(v))}</span></div>`).join('');
    return rows + `<details class="small"><summary>Full record${entries.length > 4 ? ` (${entries.length} fields)` : ''}</summary><pre class="mono">${esc(JSON.stringify(obj, null, 2))}</pre></details>`;
  }

  views.audit = async () => {
    const logs = await api('GET', '/api/audit-logs');
    if (currentView !== 'audit') return;
    $('#view').innerHTML = `
      <div class="toolbar">
        <div class="muted">Immutable enterprise audit trail capturing named administrative modifications, publishes, key operations, role attributions, and before/after diffs.</div>
        <div class="spacer"></div>
        <button class="button small" id="refresh-audit">Refresh</button>
      </div>

      <div class="card">
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Timestamp</th>
                <th>Actor Attribution</th>
                <th>Action</th>
                <th>Resource</th>
                <th>Network & Provenance</th>
                <th>State Diff / Changes</th>
              </tr>
            </thead>
            <tbody>
              ${logs.map((a) => `
                <tr>
                  <td class="mono muted">${fmtDate(a.ts)}</td>
                  <td>
                    <b>${esc(a.actor_email || a.actor || 'system')}</b>
                    ${a.actor_role ? `<div><span class="tag gray small">${esc(a.actor_role)}</span></div>` : ''}
                  </td>
                  <td><span class="tag ${a.action === 'ROLLBACK' ? 'warn' : a.action === 'DELETE' ? 'error' : 'green'}"><b>${esc(a.action)}</b></span></td>
                  <td>
                    <b>${esc(a.resource_type)}</b>
                    <div class="mono small muted">${esc(a.resource_name || a.resource_id || '–')}</div>
                  </td>
                  <td>
                    <div class="mono small">${esc(a.client_ip || '127.0.0.1')}</div>
                    <div class="muted small" title="${esc(a.user_agent || '')}">${esc(clientLabel(a.user_agent))}</div>
                  </td>
                  <td class="audit-detail">${auditSummary(a.state_diff && Object.keys(a.state_diff).length ? a.state_diff : (a.details || {}))}</td>
                </tr>
              `).join('') || '<tr><td colspan="6" class="empty">No administrative actions logged yet.</td></tr>'}
            </tbody>
          </table>
        </div>
      </div>
    `;

    $('#refresh-audit').onclick = () => views.audit();
  };

  // -------------------------------------------------------------------------
  // OpenAPI Importer Dialog
  // -------------------------------------------------------------------------
  function openAPIModal() {
    openModal({
      title: 'Import OpenAPI or Swagger',
      large: true,
      body: `
        <div class="callout">
          <b>Safe Publishing:</b> OpenAPI specs can be validated and staged as inactive <b>Drafts</b> for pre-publication review.
        </div>
        <div class="field">
          <label>Paste OpenAPI / Swagger Document (JSON or YAML)</label>
          <textarea id="openapi-spec-input" rows="12" placeholder='openapi: 3.0.0\ninfo:\n  title: Petstore Logistics API\n  version: 2.1.0\nservers:\n  - url: http://localhost:7070\npaths:\n  /shipments:\n    get:\n      summary: List all active shipments'></textarea>
        </div>
        <div class="field check">
          <input type="checkbox" id="openapi-as-draft" checked>
          <label for="openapi-as-draft">Stage as Draft for Review (Recommended: isolated from gateway traffic until published)</label>
        </div>
      `,
      actions: [{
        label: 'Validate and import',
        primary: true,
        onClick: async (close, wrap, btn) => {
          const spec = $('#openapi-spec-input', wrap).value.trim();
          if (!spec) { toast('Please paste an OpenAPI specification', 'error'); return; }
          const asDraft = $('#openapi-as-draft', wrap).checked;
          btn.disabled = true;
          try {
            const res = await fetch(`/api/apis/import-openapi?draft=${asDraft}`, {
              method: 'POST',
              headers: { 'Authorization': 'Bearer ' + token, 'Content-Type': 'text/plain' },
              body: spec
            });
            const data = await res.json();
            if (!res.ok) throw new Error(data.message || data.error || 'Import failed');
            toast(`API <b>${esc(data.name)}</b> imported successfully! ${asDraft ? 'Staged as draft for review.' : 'Live on gateway.'}`, 'success');
            close();
            route(true);
          } catch (e) {
            toast(esc(e.message), 'error');
          } finally {
            btn.disabled = false;
          }
        }
      }]
    });
  }

  // -------------------------------------------------------------------------
  // API Form (Create & Edit)
  // -------------------------------------------------------------------------
  function apiForm(a) {
    const isNew = !a;
    a = a || {
      strip_path: true,
      auth_type: 'none',
      visibility: 'public',
      require_approval: false,
      quota_failure_policy: 'fail_open',
      timeout_ms: 30000,
      rate_limit_per_minute: 0,
      quota_per_day: 0,
      quota_per_month: 0,
      enabled: true,
      is_ai: false,
      is_draft: false,
      request_headers: {}
    };
    const hdrs = Object.entries(a.request_headers || {}).map(([k, v]) => `${k}: ${v}`).join('\n');

    openForm({
      title: isNew ? 'New API' : `Edit API · ${a.name}`,
      submitLabel: isNew ? 'Create API' : 'Save changes',
      large: true,
      fields: [
        [
          { name: 'name', label: 'API Name', value: a.name, placeholder: 'orders-api', required: true },
          { name: 'base_path', label: 'Gateway Base Path', value: a.base_path, placeholder: '/orders', required: true }
        ],
        { name: 'upstream_url', label: 'Upstream Backend URL', value: a.upstream_url, placeholder: 'http://backend.internal:7070', required: true },
        { name: 'description', label: 'Description', value: a.description },
        [
          {
            name: 'auth_type', label: 'Authentication Scheme', type: 'select', value: a.auth_type, options: [
              { value: 'none', label: 'None (open public route)' },
              { value: 'api_key', label: 'API Key + Subscription Plan' },
              { value: 'jwt', label: 'Strict JWT (HS256, mandatory exp claim)' },
              { value: 'oidc', label: 'OIDC / JWKS (RS256, ES256)' },
              { value: 'mtls', label: 'Mutual TLS (client certificate)' }
            ]
          },
          { name: 'jwt_secret', label: 'JWT Signing Secret (HS256)', value: a.jwt_secret, type: 'password', help: 'Min 16 characters. Secret never exported.' }
        ],
        [
          { name: 'jwks_url', label: 'JWKS Endpoint URL (OIDC)', value: a.jwks_url, placeholder: 'https://auth.domain.com/.well-known/jwks.json', help: 'Public keys endpoint for asymmetric signature verification.' },
          { name: 'oidc_issuer', label: 'Expected Issuer (iss)', value: a.oidc_issuer, placeholder: 'https://auth.domain.com/' }
        ],
        [
          {
            name: 'visibility', label: 'Catalog Visibility', type: 'select', value: a.visibility || 'public', options: [
              { value: 'public', label: 'Public (listed in developer catalog)' },
              { value: 'private', label: 'Private (hidden from developer catalog)' },
              { value: 'internal', label: 'Internal Only' }
            ]
          },
          {
            name: 'quota_failure_policy', label: 'If the quota store is down', type: 'select', value: a.quota_failure_policy || 'fail_open', help: 'Allowing traffic marks responses with X-RelayOps-Degraded; blocking returns 503.', options: [
              { value: 'fail_open', label: 'Allow traffic (fail open)' },
              { value: 'fail_closed', label: 'Block traffic (fail closed)' }
            ]
          }
        ],
        [
          { name: 'rate_limit_per_minute', label: 'Rate Limit (req/min per client)', type: 'number', value: a.rate_limit_per_minute, help: '0 = unlimited. Distributed Redis enforcement.' },
          { name: 'quota_per_day', label: 'Daily Quota (req/day)', type: 'number', value: a.quota_per_day, help: '0 = unlimited.' }
        ],
        [
          { name: 'timeout_ms', label: 'Upstream Timeout (ms)', type: 'number', value: a.timeout_ms, help: 'Connection & read timeout.' },
          { name: 'quota_per_month', label: 'Monthly Quota (req/month)', type: 'number', value: a.quota_per_month, help: '0 = unlimited.' }
        ],
        { name: 'request_headers', label: 'Upstream Header Injection', type: 'textarea', value: hdrs, placeholder: 'Authorization: Bearer \${secret:NVIDIA_API_KEY}\nX-Tenant: prod', help: 'Supports dynamic secrets: ${secret:KEY_NAME}. One "Header: value" per line.' },
        [
          { name: 'strip_path', label: 'Strip base path before forwarding', type: 'checkbox', value: a.strip_path },
          { name: 'cors_enabled', label: 'Enable CORS headers', type: 'checkbox', value: a.cors_enabled }
        ],
        [
          { name: 'require_approval', label: 'Require Admin Approval for Subscriptions', type: 'checkbox', value: a.require_approval },
          { name: 'is_ai', label: 'AI Model Gateway (token metering & SSE delta parsing)', type: 'checkbox', value: a.is_ai }
        ],
        [
          { name: 'is_draft', label: 'Saved as Draft (inactive)', type: 'checkbox', value: a.is_draft },
          { name: 'enabled', label: 'Enabled (serving gateway traffic)', type: 'checkbox', value: a.enabled }
        ]
      ],
      onChange: (v, wrap) => {
        $('[data-field="jwt_secret"]', wrap).style.display = v.auth_type === 'jwt' ? '' : 'none';
        $('[data-field="jwks_url"]', wrap).style.display = v.auth_type === 'oidc' ? '' : 'none';
        $('[data-field="oidc_issuer"]', wrap).style.display = v.auth_type === 'oidc' ? '' : 'none';
      },
      onSubmit: async (v) => {
        const headers = {};
        for (const line of v.request_headers.split('\n')) {
          const i = line.indexOf(':');
          if (i > 0) headers[line.slice(0, i).trim()] = line.slice(i + 1).trim();
          else if (line.trim()) throw new Error(`Invalid header line: "${line}"`);
        }
        v.request_headers = headers;
        if (isNew) {
          await api('POST', '/api/apis', v);
          toast(`API <b>${esc(v.name)}</b> created — live at <code class="mono">:8080${esc(v.base_path)}</code>`, 'success');
        } else {
          await api('PUT', `/api/apis/${a.id}`, v);
          toast(`API <b>${esc(v.name)}</b> updated successfully`, 'success');
        }
      }
    });
  }

  // -------------------------------------------------------------------------
  // boot & initial load
  // -------------------------------------------------------------------------
  $('#token-btn').onclick = () => askToken();
  api('GET', '/api/auth/me').then(me => RelayUI.setAccess(me)).catch(() => RelayUI.setAccess({permissions:[]}));
  if (window.initTestStudio) {
    window.initTestStudio({
      views,
      api,
      toast,
      esc,
      openModal,
      statusPill,
      methodBadge,
      fmtMs,
      fmtDate,
      fmtNum,
      $
    });
  }
  if (window.initAIManagement) {
    window.initAIManagement({
      views,
      api,
      toast,
      esc,
      openModal,
      statusPill,
      methodBadge,
      fmtMs,
      fmtDate,
      fmtNum,
      $
    });
  }
  if (window.initAPIOps) {
    window.initAPIOps({
      views,
      api,
      toast,
      esc,
      openModal,
      statusPill,
      methodBadge,
      fmtMs,
      fmtDate,
      fmtNum,
      $
    });
  }
  if (window.initOrbit) window.initOrbit({ api, esc, toast });
  connectStream();
  route();
})();
