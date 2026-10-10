/* Developer hub. All catalog content is untrusted; escape HTML and keep credentials in memory. */
(() => {
  'use strict';
  const $ = (id) => document.getElementById(id);
  const methods = ['get', 'post', 'put', 'patch', 'delete', 'head', 'options'];
  const state = { apis: [], plans: [], filter: 'all', loaded: false, error: '', response: '', toastTimer: null };
  const esc = (value) => String(value ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  const pretty = (value) => JSON.stringify(value, null, 2);
  const number = (value) => Number(value || 0).toLocaleString();
  const authName = (api) => ({ none: 'No authentication', api_key: 'API key', jwt: 'JWT token', oidc: 'OIDC token' }[api.auth_type] || 'Check with provider');
  const findAPI = (id) => state.apis.find((api) => api.id === id);
  const link = (view, id) => `#${view}/${encodeURIComponent(id)}`;
  const shellQuote = (value) => "'" + String(value).replace(/'/g, "'\\''") + "'";

  function resolve(spec, value) {
    const visited = new Set();
    while (value && typeof value === 'object' && typeof value.$ref === 'string') {
      if (!value.$ref.startsWith('#/') || visited.has(value.$ref)) return {};
      visited.add(value.$ref);
      const parts = value.$ref.slice(2).split('/').map((part) => part.replace(/~1/g, '/').replace(/~0/g, '~'));
      value = parts.reduce((node, key) => node && Object.hasOwn(node, key) ? node[key] : undefined, spec);
    }
    return value && typeof value === 'object' ? value : {};
  }

  // Show a protocol filter only when the catalog has such APIs.
  function syncProtocolFilters(apis) {
    for (const p of ['graphql', 'grpc', 'mcp']) {
      const b = document.querySelector(`[data-filter="${p}"]`);
      if (b) b.hidden = !apis.some((a) => !a.is_ai && a.protocol === p);
    }
  }

  // apiKind labels an API by protocol.
  function apiKind(api) {
    if (api.is_ai) return { label: 'AI & MODELS', icon: 'layers', cls: 'ai' };
    return ({
      graphql: { label: 'GRAPHQL', icon: 'braces', cls: 'graphql' },
      grpc: { label: 'gRPC', icon: 'zap', cls: 'grpc' },
      mcp: { label: 'MCP SERVER', icon: 'bot', cls: 'mcp' }
    })[api.protocol] || { label: 'REST API', icon: 'code-xml', cls: '' };
  }

  // A reference the gateway generated (the owner published none) is not
  // presented as a list of endpoints, except gRPC methods from a descriptor.
  function isSynthesized(api) { return !!api.openapi_spec?.info?.['x-relayops-synthesized']; }
  function specDescription(api) { return isSynthesized(api) ? '' : (api.openapi_spec?.info?.description || ''); }
  function referenceSummary(api) {
    const n = operations(api).length;
    if (isSynthesized(api)) {
      if (api.protocol === 'grpc') return n ? `${n} method${n === 1 ? '' : 's'}` : 'Methods not published';
      if (api.protocol === 'graphql') return 'GraphQL endpoint';
      if (api.protocol === 'mcp') return 'MCP endpoint';
      return 'No published reference';
    }
    return n ? `${n} endpoint${n === 1 ? '' : 's'}` : 'No published reference';
  }

  function operations(api) {
    const spec = api.openapi_spec || {};
    return Object.entries(spec.paths || {}).flatMap(([path, raw]) => {
      const item = resolve(spec, raw);
      return methods.filter((method) => item[method]).map((method) => ({
        path, method: method.toUpperCase(), operation: resolve(spec, item[method]), item,
      }));
    });
  }

  // OpenAPI operation paths are relative to the server; RelayOps mounts that API at base_path.
  function gatewayPath(api, operationPath) {
    const path = operationPath || '';
    const base = String(api.base_path || '/').replace(/\/$/, '');
    return (base + '/' + path.replace(/^\//, '')).replace(/\/$/, '') || '/';
  }

  function sampleValue(spec, raw, depth = 0) {
    if (depth > 5) return null;
    const schema = resolve(spec, raw);
    if (schema.example !== undefined) return schema.example;
    if (schema.default !== undefined) return schema.default;
    if (schema.enum?.length) return schema.enum[0];
    if (schema.oneOf || schema.anyOf) return sampleValue(spec, (schema.oneOf || schema.anyOf)[0], depth + 1);
    if (schema.allOf) return Object.assign({}, ...schema.allOf.map((part) => sampleValue(spec, part, depth + 1) || {}));
    if (schema.type === 'object' || schema.properties) return Object.fromEntries(Object.entries(schema.properties || {}).slice(0, 30).map(([key, value]) => [key, sampleValue(spec, value, depth + 1)]));
    if (schema.type === 'array') return [sampleValue(spec, schema.items, depth + 1)];
    if (schema.type === 'integer' || schema.type === 'number') return schema.minimum ?? 0;
    if (schema.type === 'boolean') return false;
    if (schema.format === 'date') return '2026-01-01';
    if (schema.format === 'date-time') return '2026-01-01T00:00:00Z';
    return 'string';
  }

  function parameters(api, op) {
    const spec = api.openapi_spec || {};
    const merged = new Map();
    [...(op.item.parameters || []), ...(op.operation.parameters || [])].forEach((raw) => {
      const p = resolve(spec, raw);
      merged.set(p.in + ':' + p.name, p);
    });
    return [...merged.values()];
  }

  function requestBody(api, op) {
    const spec = api.openapi_spec || {};
    const body = resolve(spec, op.operation.requestBody);
    const media = body.content?.['application/json'];
    if (media) {
      if (media.example !== undefined) return pretty(media.example);
      if (media.examples) {
        const example = resolve(spec, Object.values(media.examples)[0]);
        if (example.value !== undefined) return pretty(example.value);
      }
      return pretty(sampleValue(spec, media.schema));
    }
    const swaggerBody = parameters(api, op).find((p) => p.in === 'body');
    return swaggerBody ? pretty(sampleValue(spec, swaggerBody.schema)) : '';
  }

  async function fetchJSON(url, options = {}) {
    const headers = { ...(options.headers || {}) };
    const savedKey = localStorage.getItem('relayops_portal_key') || localStorage.getItem('relayops_last_key');
    if (savedKey && !headers['X-Developer-Key'] && !headers['Authorization']) {
      headers['X-Developer-Key'] = savedKey;
    }
    const response = await fetch(url, { ...options, headers, signal: AbortSignal.timeout(25000) });
    let data;
    try { data = await response.json(); } catch (_) { throw new Error('The server returned an unreadable response. Please try again.'); }
    if (!response.ok) throw new Error(data.message || `Request failed (${response.status}).`);
    return data;
  }

  function toast(message) {
    clearTimeout(state.toastTimer);
    $('toast').textContent = message;
    $('toast').hidden = false;
    state.toastTimer = setTimeout(() => { $('toast').hidden = true; }, 3500);
  }

  async function copy(text) {
    try { await navigator.clipboard.writeText(text); toast('Copied to clipboard'); }
    catch (_) { toast('Clipboard unavailable. Select and copy the text instead.'); }
  }

  function catalogMessage(title, detail, retry = false) {
    $('catalog-grid').innerHTML = `<div class="empty-state"><h3>${esc(title)}</h3><p>${esc(detail)}</p>${retry ? '<button class="button" type="button" data-retry>Try again</button>' : ''}</div>`;
  }

  function renderCatalog() {
    if (state.error) { catalogMessage('Could not load the catalog', state.error, true); return; }
    const query = $('catalog-search').value.trim().toLowerCase();
    const apis = state.apis.filter((api) => {
      const category = state.filter === 'all' || (state.filter === 'ai' ? api.is_ai : state.filter === 'rest' ? !api.is_ai && (api.protocol || 'http') === 'http' : !api.is_ai && api.protocol === state.filter);
      const searchable = [api.name, api.description, api.base_path, ...operations(api).flatMap((op) => [op.path, op.operation.summary, ...(op.operation.tags || [])])].join(' ').toLowerCase();
      return category && searchable.includes(query);
    }).sort((a, b) => $('catalog-sort').value === 'operations' ? operations(b).length - operations(a).length || a.name.localeCompare(b.name) : a.name.localeCompare(b.name));
    $('catalog-summary').textContent = `Showing ${apis.length} of ${state.apis.length} APIs`;
    if (!apis.length) {
      catalogMessage(state.apis.length ? 'No matching APIs' : 'Your catalog is ready for its first API', state.apis.length ? 'Try another search or category.' : 'Public, published APIs will appear here when your API provider makes them available.');
      return;
    }
    $('catalog-grid').innerHTML = apis.map((api) => {
      const kind = apiKind(api);
      return `<article class="api-card"><div class="card-top"><span class="api-icon ${kind.cls}" aria-hidden="true">${RelayUI.icon(kind.icon)}</span><span class="tag ${kind.cls}">${kind.label}</span></div><h3>${esc(api.openapi_spec?.info?.title || api.name)}</h3><p class="${api.description || specDescription(api) ? '' : 'muted'}">${esc(api.description || specDescription(api) || 'No description yet.')}</p><div class="api-path" title="${esc(api.base_path)}">${esc(api.base_path)}</div><div class="card-meta"><span>${esc(authName(api))}</span><span>·</span><span>${referenceSummary(api)}</span>${api.require_approval ? '<span>Approval required</span>' : ''}</div><div class="card-actions"><a href="${link('reference', api.id)}">View reference </a><a href="${link('console', api.id)}">Try a request </a></div></article>`;
    }).join('');
  }

  async function loadCatalog() {
    $('refresh-catalog').disabled = true;
    $('catalog-grid').setAttribute('aria-busy', 'true');
    $('catalog-grid').innerHTML = '<div class="empty-state">Loading your API catalog…</div>';
    try {
      const data = await fetchJSON('/portal/api/catalog');
      state.apis = Array.isArray(data.apis) ? data.apis : [];
      syncProtocolFilters(state.apis);
      state.plans = Array.isArray(data.plans) ? data.plans : [];
      state.loaded = true; state.error = '';
      $('nav-count').textContent = state.apis.length;
      $('catalog-count').textContent = state.apis.length;
      populateSelectors(); renderCatalog(); route(false);
    } catch (error) {
      state.error = error.message;
      $('catalog-summary').textContent = 'Catalog unavailable'; renderCatalog();
      $('reg-api-list').textContent = 'Catalog unavailable. Return to the catalog and refresh to select APIs.';
      $('register-submit').disabled = true;
      if (location.hash.startsWith('#reference')) route(false);
    } finally {
      $('refresh-catalog').disabled = false;
      $('catalog-grid').setAttribute('aria-busy', 'false');
    }
  }

  function populateSelectors() {
    const selected = $('console-api').value;
    $('console-api').innerHTML = '<option value="">Choose an API</option>' + state.apis.map((api) => `<option value="${esc(api.id)}">${esc(api.openapi_spec?.info?.title || api.name)}</option>`).join('');
    if (findAPI(selected)) $('console-api').value = selected;
    const checked = new Set([...document.querySelectorAll('[name="reg-api"]:checked')].map((el) => el.value));
    $('reg-api-list').innerHTML = state.apis.map((api) => `<label class="subscription-option"><input type="checkbox" name="reg-api" value="${esc(api.id)}" ${checked.has(api.id) ? 'checked' : ''}><span>${esc(api.openapi_spec?.info?.title || api.name)}<small>${esc(authName(api))} · ${api.require_approval ? 'Approval required' : 'No subscription approval required'}</small></span></label>`).join('') || '<p class="field-help">No public APIs are available yet.</p>';
    $('register-submit').disabled = state.apis.length === 0;

    // Render the API owner's published plans. Do not invent RelayOps SKUs.
    const plans = state.plans || [];
    if (plans.length > 0) {
      $('registration-plan').innerHTML = `
        <hr>
        <span class="eyebrow">COMMERCIAL TIERS</span>
        <div class="plans-tier-list" style="display:flex;flex-direction:column;gap:8px;margin-top:8px">
          ${plans.map((p) => {
            const price = p.price_monthly_usd != null && Number(p.price_monthly_usd) > 0
              ? `$${Number(p.price_monthly_usd).toFixed(0)}/mo`
              : 'Included';
            return `
              <div style="border:1px solid #cbd5e1;border-radius:6px;padding:8px 10px;background:#f8fafc">
                <div style="display:flex;justify-content:space-between;align-items:center">
                  <b>${esc(p.name)}</b>
                  <span class="tag ${price.includes('Free') ? 'gray' : 'success'}" style="font-weight:700">${price}</span>
                </div>
                <div class="small muted" style="margin-top:2px">${esc(p.description || '')}</div>
                <div class="small mono" style="margin-top:4px;color:var(--text-muted)">
                  ${p.rate_limit_per_minute > 0 ? number(p.rate_limit_per_minute) + ' req/min' : 'Unlimited'} ·
                  ${p.quota_per_day > 0 ? number(p.quota_per_day) + ' req/day' : 'Unlimited'}
                </div>
              </div>`;
          }).join('')}
        </div>`;
    } else {
      $('registration-plan').innerHTML = '';
    }
  }

  function generateSnippets(api, op) {
    const origin = window.location.origin;
    const path = gatewayPath(api, op?.path);
    const url = `${origin}${path}`;
    const method = op?.method || 'GET';
    const body = op ? requestBody(api, op) : '';
    const hasBody = ['POST', 'PUT', 'PATCH'].includes(method) && Boolean(body);

    let authCurl = '';
    let jsAuth = '';
    let pyAuth = '';
    if (api.auth_type === 'api_key') {
      authCurl = '-H "X-API-Key: YOUR_API_KEY"';
      jsAuth = '    "X-API-Key": "YOUR_API_KEY",\n';
      pyAuth = '    "X-API-Key": "YOUR_API_KEY",\n';
    } else if (api.auth_type === 'jwt' || api.auth_type === 'oidc') {
      authCurl = '-H "Authorization: Bearer YOUR_TOKEN"';
      jsAuth = '    "Authorization": "Bearer YOUR_TOKEN",\n';
      pyAuth = '    "Authorization": "Bearer YOUR_TOKEN",\n';
    }

    const curl = `curl -X ${method} "${url}" \\
  ${authCurl ? authCurl + ' \\\n  ' : ''}${hasBody ? '-H "Content-Type: application/json" \\\n  -d \'' + body.replace(/'/g, "\\'") + '\'' : ''}`.trim().replace(/\\$/, '');

    const js = `const response = await fetch("${url}", {
  method: "${method}",
  headers: {
${jsAuth}${hasBody ? '    "Content-Type": "application/json",\n' : ''}  }${hasBody ? `,\n  body: JSON.stringify(${body.trim() || '{}'})` : ''}
});
const data = await response.json();
console.log(data);`;

    const py = `import requests

url = "${url}"
headers = {
${pyAuth}${hasBody ? '    "Content-Type": "application/json",\n' : ''}}
${hasBody ? `payload = ${body.trim() || '{}'}
response = requests.${method.toLowerCase()}(url, headers=headers, json=payload)` : `response = requests.${method.toLowerCase()}(url, headers=headers)`}

print(response.status_code)
print(response.json())`;

    return { curl, js, py };
  }

  function renderReference(id) {
    const api = findAPI(id);
    if (!api) {
      $('reference-content').innerHTML = `<a class="text-link" href="#catalog"> Back to catalog</a><div class="empty-state"><h3>${state.error ? 'Reference unavailable' : state.loaded ? 'API not found' : 'Loading reference…'}</h3><p>${esc(state.error || (state.loaded ? 'This API may no longer be published. Browse the catalog for available APIs.' : 'Fetching the API catalog.'))}</p></div>`;
      return;
    }
    const spec = api.openapi_spec || {};
    const ops = operations(api);
    const table = (rows, headings) => `<table class="doc-table"><thead><tr>${headings.map((h) => `<th>${esc(h)}</th>`).join('')}</tr></thead><tbody>${rows.map((row) => `<tr>${row.map((cell) => `<td>${esc(cell)}</td>`).join('')}</tr>`).join('')}</tbody></table>`;
    $('reference-content').innerHTML = `
      <a class="text-link" href="#catalog"> Back to catalog</a>
      <div class="reference-heading">
        <div>
          <span class="eyebrow">API REFERENCE</span>
          <h1>${esc(spec.info?.title || api.name)}</h1>
        </div>
        <a class="button primary" href="${link('register', api.id)}">Get access </a>
      </div>
      <p class="lead">${esc(api.description || specDescription(api) || 'The API owner has not described this API yet.')}</p>
      <div class="spec-meta">
        <span class="tag ${apiKind(api).cls}">${apiKind(api).label}</span>
        ${spec.info?.version ? `<span class="tag">VERSION ${esc(spec.info.version)}</span>` : ''}
        <span class="tag">${esc(authName(api))}</span>
        ${api.require_approval ? '<span class="tag">APPROVAL REQUIRED</span>' : ''}
      </div>
      <div class="reference-layout">
        <div>
          <div class="panel-heading">
            <h2>Endpoints <span class="count">${ops.length}</span></h2>
            <div style="display:flex;gap:8px">
              <a class="button small" href="/portal/api/apis/${api.id}/openapi.json" target="_blank" download="${esc(api.name)}-openapi.json">Download OpenAPI 3.0</a>
            </div>
          </div>
          ${ops.length ? ops.map((op, index) => {
            const params = parameters(api, op).filter((p) => p.in !== 'body');
            const body = requestBody(api, op);
            const responses = Object.entries(op.operation.responses || {}).map(([code, raw]) => [code, resolve(spec, raw).description || '']);
            const snips = generateSnippets(api, op);
            return `
              <details class="operation" ${index === 0 ? 'open' : ''}>
                <summary>
                  <span class="method ${op.method.toLowerCase()}">${op.method}</span>
                  <code>${esc(gatewayPath(api, op.path))}</code>
                  <span class="endpoint-title">${esc(op.operation.summary || '')}</span>
                </summary>
                <div class="operation-description">
                  <p>${esc(op.operation.description || op.operation.summary || 'No endpoint description supplied.')}</p>
                  ${params.length ? '<h4>Parameters</h4>' + table(params.map((p) => [p.name, p.in, p.required ? 'Required' : 'Optional', p.description || '']), ['Name', 'In', 'Required', 'Description']) : ''}
                  ${body ? `<h4>Example request body</h4><pre class="response-output">${esc(body)}</pre>` : ''}
                  ${responses.length ? '<h4>Responses</h4>' + table(responses, ['Status', 'Description']) : ''}
                  
                  <h4>Integration Code Snippets</h4>
                  <div class="code-snippets" style="margin:8px 0">
                    <div style="font-size:12px;font-weight:600;margin-bottom:4px">cURL</div>
                    <pre class="response-output" style="user-select:all;margin-bottom:8px"><code>${esc(snips.curl)}</code></pre>
                    <div style="font-size:12px;font-weight:600;margin-bottom:4px">JavaScript (Fetch)</div>
                    <pre class="response-output" style="user-select:all;margin-bottom:8px"><code>${esc(snips.js)}</code></pre>
                    <div style="font-size:12px;font-weight:600;margin-bottom:4px">Python (requests)</div>
                    <pre class="response-output" style="user-select:all;margin-bottom:8px"><code>${esc(snips.py)}</code></pre>
                  </div>

                  <button class="button small" type="button" data-operation="${index}">Open in workbench </button>
                </div>
              </details>`;
          }).join('') : '<div class="empty-state"><h3>No endpoint reference supplied</h3><p>You can send a request to the base path in the workbench. Ask the API provider for endpoint documentation.</p></div>'}

          <!-- Quickstart Onboarding Guidance -->
          <div class="card" style="padding:16px;margin-top:20px;border-left:4px solid var(--accent, #6366f1);background:#f8fafc">
            <h3 style="font-size:15px;margin-bottom:8px">Quickstart Onboarding Guide for ${esc(api.name)}</h3>
            <ol style="margin:0;padding-left:18px;font-size:13px;line-height:1.6">
              <li><b>Obtain credentials:</b> Register your application under <em>Get access</em> to receive an API key or obtain a signed JWT token with valid audience.</li>
              <li><b>Mount the Gateway Base Path:</b> Send all requests to <code>${esc(api.base_path)}</code>. Trailing sub-paths are proxied directly to the upstream destination.</li>
              <li><b>Handle Rate Limits (429):</b> RelayOps gateway attaches <code>X-RateLimit-Limit</code>, <code>X-RateLimit-Remaining</code>, and <code>Retry-After</code> headers. If you receive HTTP 429, respect the <code>Retry-After</code> cooldown window.</li>
              <li><b>Telemetry & Tracing:</b> Every proxied response includes an <code>X-Request-ID</code> header. Provide this ID when diagnosing upstream errors with your operations team.</li>
            </ol>
          </div>
        </div>

        <aside class="info-card">
          <span class="eyebrow">AT A GLANCE</span>
          <h3>Before you connect</h3>
          <div class="overview-list">
            <div><span>Base path</span><code>${esc(api.base_path)}</code></div>
            <div><span>Authentication</span><strong>${esc(authName(api))}</strong></div>
            <div><span>API rate limit</span><strong>${api.rate_limit_per_minute > 0 ? number(api.rate_limit_per_minute) + ' / minute' : 'Not configured'}</strong></div>
            <div><span>API daily quota</span><strong>${api.quota_per_day > 0 ? number(api.quota_per_day) : 'Not configured'}</strong></div>
            <div><span>Subscription</span><strong>${api.require_approval ? 'Approval required' : 'No approval required'}</strong></div>
          </div>
          <p>Your subscription plan can override the API’s limits.</p>
          <a class="button" href="${link('console', api.id)}">Try a request </a>
        </aside>
      </div>`;

    $('reference-content').querySelectorAll('[data-operation]').forEach((button) => button.addEventListener('click', () => {
      prepareRequest(api, ops[Number(button.dataset.operation)]);
      location.hash = 'console';
    }));
  }

  function prepareRequest(api, op = operations(api)[0]) {
    state.response = '';
    $('res-body').textContent = 'Your response will appear here.';
    $('res-headers').textContent = '{}';
    $('response-status').textContent = 'NOT SENT';
    $('response-status').className = 'tag';
    $('response-time').textContent = 'Ready when you are';
    $('copy-response').disabled = true;
    $('console-api').value = api.id;
    $('try-method').value = op?.method || 'GET';
    let path = gatewayPath(api, op?.path);
    const headers = { 'Content-Type': 'application/json' };
    if (op) {
      const query = new URLSearchParams();
      parameters(api, op).forEach((p) => {
        const example = p.example ?? resolve(api.openapi_spec, p.schema).example;
        if (p.in === 'path' && example !== undefined) path = path.replace('{' + p.name + '}', encodeURIComponent(String(example)));
        if (p.in === 'query' && p.required) query.set(p.name, String(example ?? sampleValue(api.openapi_spec, p.schema || p)));
        if (p.in === 'header' && p.required && !['authorization', 'x-api-key'].includes(String(p.name).toLowerCase())) headers[p.name] = String(example ?? sampleValue(api.openapi_spec, p.schema || p));
      });
      if (query.size) path += '?' + query;
    }
    $('try-path').value = path;
    $('try-body').value = op ? requestBody(api, op) : '';
    $('try-headers').value = pretty(headers);
    $('request-error').hidden = true;
    updateAuthHelp(); updateCurl();
  }

  function updateAuthHelp() {
    const api = findAPI($('console-api').value);
    $('auth-help').textContent = api ? `${authName(api)}${api.require_approval ? ' · Subscription approval required' : ''}${api.auth_type === 'none' ? ' · Credentials are not required for this API.' : api.auth_type === 'api_key' ? ' · Enter your application key below.' : ' · Obtain a token from your API provider.'}` : 'Choose an API to see its authentication requirements.';
  }

  function requestHeaders() {
    const headers = JSON.parse($('try-headers').value || '{}');
    if (!headers || Array.isArray(headers) || typeof headers !== 'object' || Object.values(headers).some((value) => typeof value !== 'string')) throw new Error('Headers must be a JSON object with string values.');
    if (Object.keys(headers).some((name) => !/^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/.test(name)) || Object.values(headers).some((value) => /[\r\n]/.test(value))) throw new Error('Headers contain an invalid name or value.');
    if ($('try-key').value.trim()) headers['X-API-Key'] = $('try-key').value.trim();
    if ($('try-bearer').value.trim()) headers.Authorization = 'Bearer ' + $('try-bearer').value.trim();
    return headers;
  }

  function codeSnippet(includeCredentials = false) {
    const lang = state.codeLang || 'curl';
    let headers;
    try { headers = requestHeaders(); } catch (_) { return '# Enter a valid JSON headers object to generate an example.'; }
    const api = findAPI($('console-api').value);
    if (api?.auth_type === 'api_key' && !Object.keys(headers).some((name) => name.toLowerCase() === 'x-api-key')) headers['X-API-Key'] = 'YOUR_API_KEY';
    if (['jwt', 'oidc'].includes(api?.auth_type) && !Object.keys(headers).some((name) => name.toLowerCase() === 'authorization')) headers.Authorization = 'Bearer YOUR_ACCESS_TOKEN';
    const configured = $('gateway-url').value.trim().replace(/\/$/, '') || 'http://127.0.0.1:8080';
    const path = $('try-path').value || '/your-api/endpoint';
    const url = configured + path;
    const method = $('try-method').value;
    const body = ['GET', 'HEAD'].includes(method) ? '' : ($('try-body').value || '');

    if (lang === 'js') {
      const hasBody = Boolean(body);
      const headerLines = Object.entries(headers).map(([k, v]) => `    ${JSON.stringify(k)}: ${JSON.stringify(v)}`).join(',\n');
      return `const response = await fetch(${JSON.stringify(url)}, {
  method: ${JSON.stringify(method)},
  headers: {
${headerLines}
  }${hasBody ? `,\n  body: ${JSON.stringify(body)}` : ''}
});
const data = await response.json();
console.log(data);`;
    }

    if (lang === 'py') {
      const hasBody = Boolean(body);
      const headerLines = Object.entries(headers).map(([k, v]) => `    ${JSON.stringify(k)}: ${JSON.stringify(v)}`).join(',\n');
      return `import requests

url = ${JSON.stringify(url)}
headers = {
${headerLines}
}
${hasBody ? `payload = ${JSON.stringify(body)}
response = requests.${method.toLowerCase()}(url, headers=headers, data=payload)` : `response = requests.${method.toLowerCase()}(url, headers=headers)`}

print(response.status_code)
print(response.text)`;
    }

    // Default: cURL
    const lines = [`curl -X ${method} ${shellQuote(url)}`];
    for (const [name, value] of Object.entries(headers)) {
      const secret = /^(authorization|x-api-key)$/i.test(name);
      lines.push(`  -H ${shellQuote(name + ': ' + (secret && !includeCredentials ? (name.toLowerCase() === 'authorization' ? 'Bearer YOUR_ACCESS_TOKEN' : 'YOUR_API_KEY') : value))}`);
    }
    if (body) lines.push('  --data-raw ' + shellQuote(body));
    return lines.join(' \\\n');
  }

  function curlExample(includeCredentials = false) {
    return codeSnippet(includeCredentials);
  }

  function updateCurl() { $('curl-preview').textContent = codeSnippet(); }

  function fillSamplePayload() {
    const api = findAPI($('console-api').value);
    if (!api) {
      toast('Please choose an API from the dropdown first');
      return;
    }
    const ops = operations(api);
    if (ops.length > 0) {
      prepareRequest(api, ops[0]);
      toast(`Loaded sample: ${ops[0].method} ${ops[0].path}`);
    } else {
      $('try-path').value = api.base_path;
      $('try-method').value = 'GET';
      $('try-headers').value = JSON.stringify({ "Content-Type": "application/json", "Accept": "application/json" }, null, 2);
      $('try-body').value = '';
      updateCurl();
      toast(`Loaded base path: ${api.base_path}`);
    }
  }

  async function sendRequest(event) {
    event.preventDefault();
    $('request-error').hidden = true;
    const path = $('try-path').value.trim();
    try {
      if (!path.startsWith('/') || path.startsWith('//') || /[\r\n#\\]/.test(path)) throw new Error('Use a gateway path starting with a single /, without fragments or backslashes.');
      if (/\{[^}]+\}/.test(path)) throw new Error('Replace the {path} parameters with real values before sending.');
      const headers = requestHeaders();
      $('try-send').disabled = true; $('try-send').textContent = 'Sending…';
      $('response-status').textContent = 'SENDING'; $('response-status').className = 'tag';
      $('copy-response').disabled = true;
      const start = performance.now();
      const data = await fetchJSON('/portal/api/try', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ method: $('try-method').value, path, headers, body: ['GET', 'HEAD'].includes($('try-method').value) ? '' : $('try-body').value }) });
      $('response-time').textContent = `${Math.round(performance.now() - start)} ms · round trip`;
      $('response-status').textContent = String(data.status || 'Unknown');
      $('response-status').className = 'tag ' + (data.status >= 400 ? 'error' : 'success');
      state.response = data.body || data.error || '(empty response)';
      try { state.response = pretty(JSON.parse(state.response)); } catch (_) { /* Preserve non-JSON and SSE bodies. */ }
      $('res-body').textContent = state.response;
      $('res-headers').textContent = pretty(data.headers || {});
      $('copy-response').disabled = false;
    } catch (error) {
      $('request-error').textContent = error.name === 'TimeoutError' ? 'The request timed out. Check your gateway and upstream service.' : error.message;
      $('request-error').hidden = false;
      $('response-status').textContent = 'NOT COMPLETED'; $('response-status').className = 'tag error';
      $('response-time').textContent = 'Request did not complete';
      $('res-body').textContent = 'No response received for this request.';
      $('res-headers').textContent = '{}'; state.response = '';
      $('copy-response').disabled = true;
    } finally { $('try-send').disabled = false; $('try-send').textContent = 'Send request '; }
  }

  async function register(event) {
    event.preventDefault(); $('registration-error').hidden = true;
    const ids = [...document.querySelectorAll('[name="reg-api"]:checked')].map((el) => el.value);
    if (!ids.length) { $('registration-error').textContent = 'Select at least one API for your application.'; $('registration-error').hidden = false; return; }
    const button = $('register-submit'); button.disabled = true; button.textContent = 'Registering…';
    try {
      const data = await fetchJSON('/portal/api/register', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: $('reg-name').value.trim(), email: $('reg-email').value.trim(), api_ids: ids }) });
      if (!data.api_key) throw new Error('The server did not return an API key. Contact your API provider before trying again.');
      $('registration-form').hidden = true; $('reg-result').hidden = false;
      $('reg-key').value = data.api_key; $('reg-key').type = 'password';
      $('reveal-key').textContent = 'Show'; $('reveal-key').setAttribute('aria-pressed', 'false');
      $('try-key').value = data.api_key;
      $('reg-message').textContent = data.message || 'Save your key before leaving this page. APIs marked approval required need administrator approval.';
      if (data.consumer) {
        activeConsumer = data.consumer;
        localStorage.setItem('relayops_consumer', JSON.stringify(data.consumer));
      }
      localStorage.setItem('relayops_last_key', data.api_key);
      const api = ids.map(findAPI).find((api) => api?.auth_type === 'api_key') || findAPI(ids[0]);
      if (api) prepareRequest(api);
      $('reg-result').focus(); updateCurl();
    } catch (error) { $('registration-error').textContent = error.message; $('registration-error').hidden = false; }
    finally { button.disabled = false; button.textContent = 'Register application '; }
  }

  let activeConsumer = JSON.parse(localStorage.getItem('relayops_consumer') || 'null');

  async function renderMyApps() {
    if (!activeConsumer) {
      $('apps-list').innerHTML = '<div class="empty-state">Register in the "Get Access" tab to create your developer profile first.</div>';
      $('subs-list').innerHTML = '<div class="empty-state">Register in the "Get Access" tab to link APIs.</div>';
      $('keys-list').innerHTML = '<div class="empty-state">Register to issue API keys.</div>';
      return;
    }
    const [apps, subs] = await Promise.all([
      fetchJSON('/portal/api/apps?consumer_id=' + encodeURIComponent(activeConsumer.id)).catch(() => []),
      fetchJSON('/portal/api/subscriptions?consumer_id=' + encodeURIComponent(activeConsumer.id)).catch(() => [])
    ]);

    // 1. Apps
    const appEl = $('apps-list');
    if (apps.length === 0) {
      appEl.innerHTML = '<div class="empty-state">No apps created yet. Create your first application above.</div>';
    } else {
      appEl.innerHTML = apps.map((a) => `
        <div class="card" style="padding:12px;margin-bottom:8px;display:flex;justify-content:space-between;align-items:center">
          <div>
            <b>${esc(a.name)}</b> <span class="tag ${a.environment === 'production' ? 'success' : 'warn'}">${esc(a.environment)}</span>
            <div class="muted small">${esc(a.description || 'No description')}</div>
          </div>
          <button class="button small danger" data-delapp="${esc(a.id)}">Delete</button>
        </div>
      `).join('');
      appEl.querySelectorAll('[data-delapp]').forEach((btn) => {
        btn.onclick = async () => {
          await fetchJSON('/portal/api/apps/' + encodeURIComponent(btn.dataset.delapp), { method: 'DELETE' });
          toast('Application removed');
          renderMyApps();
        };
      });
    }

    // 2. Subscriptions
    const subEl = $('subs-list');
    if (subs.length === 0) {
      subEl.innerHTML = '<div class="empty-state">No active subscriptions. Select APIs during registration or via workbench.</div>';
    } else {
      subEl.innerHTML = subs.map((s) => {
        const statusBadge = s.status === 'approved'
          ? '<span class="tag success"> Approved & Active</span>'
          : s.status === 'pending'
          ? '<span class="tag warn">⏳ Pending Admin Approval</span>'
          : `<span class="tag error"> Rejected: ${esc(s.rejection_reason || 'Denied')}</span>`;
        return `
          <div class="card" style="padding:12px;margin-bottom:8px">
            <div style="display:flex;justify-content:space-between;align-items:center">
              <b>${esc(s.api_name)}</b>
              ${statusBadge}
            </div>
            <div class="muted small" style="margin-top:4px">Plan: <b>${esc(s.plan_name || 'API Default')}</b></div>
          </div>
        `;
      }).join('');
    }

    // 3. Keys & Zero-Downtime Rotation
    const keyEl = $('keys-list');
    const storedKey = localStorage.getItem('relayops_last_key');
    keyEl.innerHTML = `
      <div class="card" style="padding:14px">
        <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:8px">
          <div><b>Primary Key (Active)</b></div>
          <button class="button small" id="rotate-key-btn">Rotate Key (7-Day Grace)</button>
        </div>
        <div class="secret-box mono small" style="word-break:break-all">${esc(storedKey || '••••••••••••••••••••••••••••••••••••••••••••')}</div>
        <div id="rotation-status" style="margin-top:10px" hidden></div>
      </div>
    `;

    $('rotate-key-btn').onclick = async () => {
      const btn = $('rotate-key-btn');
      btn.disabled = true;
      btn.textContent = 'Rotating…';
      try {
        const res = await fetchJSON('/portal/api/keys/' + encodeURIComponent(activeConsumer.id) + '/rotate', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ grace_hours: 168 })
        });
        localStorage.setItem('relayops_last_key', res.new_key);
        $('try-key').value = res.new_key;
        updateCurl();
        toast('Key rotated with zero downtime!', 'success');
        const rotBox = $('rotation-status');
        rotBox.hidden = false;
        rotBox.innerHTML = `
          <div class="callout warning" style="margin-top:8px">
            <b>New Key Generated (copy now):</b>
            <div class="mono" style="word-break:break-all;padding:6px 0;color:var(--green)">${esc(res.new_key)}</div>
            <div class="small"><b>Dual-active grace period:</b> active until ${esc(new Date(res.grace_until).toLocaleString())}. Both old and new keys are accepted concurrently.</div>
            <button class="button small" id="retire-prev-btn" style="margin-top:8px">Retire Previous Key Early</button>
          </div>
        `;
        $('retire-prev-btn').onclick = async () => {
          await fetchJSON('/portal/api/keys/' + encodeURIComponent(res.previous_key_id) + '/retire', { method: 'POST' });
          toast('Previous secondary key retired immediately', 'success');
          rotBox.innerHTML = '<div class="callout success">Previous key retired. Only new primary key is active.</div>';
        };
      } catch (e) {
        toast(e.message, 'error');
      } finally {
        btn.disabled = false;
        btn.textContent = ' Rotate Key (7-Day Grace)';
      }
    };
  }

  async function renderUsage() {
    if (!activeConsumer) {
      $('use-total').textContent = '0';
      $('use-errs').textContent = '0';
      $('use-lat').textContent = '–';
      $('usage-log-body').innerHTML = '<tr><td colspan="6" class="empty">Register an application to track personal usage telemetry.</td></tr>';
      return;
    }
    const data = await fetchJSON('/portal/api/usage?consumer_id=' + encodeURIComponent(activeConsumer.id)).catch(() => ({
      total_requests: 0, error_requests: 0, avg_latency_ms: 0, tokens_consumed: 0, recent_logs: []
    }));
    $('use-total').textContent = number(data.total_requests);
    $('use-errs').textContent = number(data.error_requests);
    $('use-lat').textContent = (data.avg_latency_ms || 0).toFixed(1) + ' ms';

    const logs = data.recent_logs || [];
    const tbody = $('usage-log-body');
    if (!logs.length) {
      tbody.innerHTML = '<tr><td colspan="6" class="empty">No requests recorded yet for this application.</td></tr>';
    } else {
      tbody.innerHTML = logs.map((l) => {
        const cls = l.status >= 500 ? 'tag error' : l.status >= 400 ? 'tag warn' : 'tag success';
        return `
          <tr>
            <td class="mono muted">${esc(new Date(l.ts).toLocaleTimeString([], { hour12: false }))}</td>
            <td><span class="method ${(l.method || 'get').toLowerCase()}">${esc(l.method)}</span></td>
            <td class="mono">${esc(l.path)}</td>
            <td><span class="${cls}">${l.status}</span></td>
            <td class="mono">${(l.latency_ms || 0).toFixed(1)} ms</td>
            <td><span class="tag gray">${esc(l.decision_reason || 'ok')}</span></td>
          </tr>
        `;
      }).join('');
    }
  }

  function getActiveKey() {
    return $('mcp-custom-key')?.value.trim() ||
      $('reg-key')?.value.trim() ||
      localStorage.getItem('relayops_portal_key') ||
      localStorage.getItem('relayops_last_key') ||
      'YOUR_API_KEY';
  }

  function getTargetServerURL() {
    const loc = window.location;
    return `${loc.protocol}//${loc.host}`;
  }

  function generateMCPConfig(client, transport, apiKey) {
    const serverURL = getTargetServerURL();
    apiKey = apiKey || 'YOUR_API_KEY';

    if (transport === 'sse' || client === 'sse') {
      const sseURL = `${serverURL}/mcp/sse?key=${encodeURIComponent(apiKey)}`;
      if (client === 'sse') {
        return `// Remote Model Context Protocol (SSE Endpoint)\n` +
          `Endpoint URL:\n${sseURL}\n\n` +
          `HTTP Headers:\n` +
          `Authorization: Bearer ${apiKey}\n` +
          `X-API-Key: ${apiKey}\n\n` +
          `// Generic MCP JSON Configuration:\n` +
          JSON.stringify({
            mcpServers: {
              relayops: {
                url: sseURL
              }
            }
          }, null, 2);
      }
      return JSON.stringify({
        mcpServers: {
          relayops: {
            url: sseURL
          }
        }
      }, null, 2);
    }

    if (client === 'continue') {
      return JSON.stringify({
        experimental: {
          modelContextProtocol: [
            {
              transport: {
                type: 'stdio',
                command: 'relayopsctl',
                args: ['mcp', '--server', serverURL, '--api-key', apiKey]
              }
            }
          ]
        }
      }, null, 2);
    }

    if (client === 'cline') {
      return JSON.stringify({
        mcpServers: {
          relayops: {
            command: 'relayopsctl',
            args: ['mcp', '--server', serverURL, '--api-key', apiKey],
            disabled: false,
            autoApprove: []
          }
        }
      }, null, 2);
    }

    // Cursor, Claude Desktop, Windsurf standard schema
    return JSON.stringify({
      mcpServers: {
        relayops: {
          command: 'relayopsctl',
          args: ['mcp', '--server', serverURL, '--api-key', apiKey]
        }
      }
    }, null, 2);
  }

  function updateMCPConfig() {
    const client = state.mcpClient || 'cursor';
    const transport = $('mcp-transport-select')?.value || 'stdio';
    const key = getActiveKey();

    const targetMap = {
      cursor: 'File: .cursor/mcp.json',
      cline: 'File: ~/Library/Application Support/Code/User/globalStorage/saoudrizwan.claude-dev/settings/cline_mcp_settings.json',
      continue: 'File: ~/.continue/config.json',
      claude: 'File: ~/Library/Application Support/Claude/claude_desktop_config.json',
      windsurf: 'File: ~/.codeium/windsurf/mcp_config.json',
      sse: 'Remote Endpoint URL & SSE Stream'
    };

    if ($('mcp-file-target')) {
      $('mcp-file-target').textContent = targetMap[client] || 'Configuration file';
    }

    const configText = generateMCPConfig(client, transport, key);
    if ($('mcp-config-preview')) {
      $('mcp-config-preview').textContent = configText;
    }
  }

  function renderMCP() {
    const sel = $('mcp-app-select');
    if (sel && sel.options.length <= 1) {
      const savedKey = localStorage.getItem('relayops_portal_key') || localStorage.getItem('relayops_last_key');
      if (savedKey) {
        const opt = document.createElement('option');
        opt.value = savedKey;
        opt.textContent = `Default Registered App (${savedKey.slice(0, 10)}...)`;
        opt.selected = true;
        sel.appendChild(opt);
        if ($('mcp-custom-key')) $('mcp-custom-key').value = savedKey;
      }
    }
    updateMCPConfig();
  }

  function route(scroll = true) {
    let [view, encoded] = location.hash.slice(1).split('/');
    let id = ''; try { id = decodeURIComponent(encoded || ''); } catch (_) { /* Invalid links fall back to not found. */ }
    if (!['catalog', 'reference', 'guide', 'console', 'register', 'myapps', 'usage', 'mcp'].includes(view)) view = 'catalog';
    document.querySelectorAll('.view').forEach((el) => { el.hidden = el.id !== 'view-' + view; });
    document.querySelectorAll('[data-nav]').forEach((el) => {
      const active = el.dataset.nav === (view === 'reference' ? 'catalog' : view);
      el.classList.toggle('active', active);
      if (active) el.setAttribute('aria-current', 'page'); else el.removeAttribute('aria-current');
    });
    const title = {
      catalog: 'API catalog', reference: 'API reference', guide: 'Getting started',
      console: 'API workbench', register: 'Get access', myapps: 'My apps & keys', usage: 'Usage & logs',
      mcp: 'Connect AI & IDE (MCP)'
    }[view];
    $('page-label').textContent = title; document.title = `${title} · RelayOps Developer Hub`;
    if (view === 'reference') renderReference(id);
    if (view === 'console' && findAPI(id)) prepareRequest(findAPI(id));
    if (view === 'register' && findAPI(id)) document.querySelectorAll('[name="reg-api"]').forEach((input) => { input.checked = input.value === id; });
    if (view === 'myapps') renderMyApps();
    if (view === 'usage') renderUsage();
    if (view === 'mcp') renderMCP();
    if (scroll) { window.scrollTo({ top: 0, behavior: 'instant' }); $('main').focus({ preventScroll: true }); }
  }

  $('catalog-search').addEventListener('input', renderCatalog);
  $('catalog-sort').addEventListener('change', renderCatalog);
  document.querySelectorAll('[data-filter]').forEach((button) => {
    button.setAttribute('aria-pressed', String(button.classList.contains('active')));
    button.addEventListener('click', () => {
      state.filter = button.dataset.filter;
      document.querySelectorAll('[data-filter]').forEach((el) => { el.classList.toggle('active', el === button); el.setAttribute('aria-pressed', String(el === button)); });
      renderCatalog();
    });
  });
  $('refresh-catalog').addEventListener('click', loadCatalog);

  const setCatalogLayout = mode => {
    $('catalog-grid').classList.toggle('catalog-list', mode === 'list');
    $('catalog-cards').setAttribute('aria-pressed', String(mode === 'cards'));
    $('catalog-list').setAttribute('aria-pressed', String(mode === 'list'));
  };
  $('catalog-cards').onclick = () => setCatalogLayout('cards');
  $('catalog-list').onclick = () => setCatalogLayout('list');
  $('dismiss-quickstart').onclick = () => { $('catalog-quickstart').hidden = true; sessionStorage.setItem('relayops_quickstart_dismissed', '1'); };
  $('catalog-quickstart').hidden = sessionStorage.getItem('relayops_quickstart_dismissed') === '1';

  $('catalog-grid').addEventListener('click', (event) => { if (event.target.closest('[data-retry]')) loadCatalog(); });
  $('request-form').addEventListener('submit', sendRequest);
  $('registration-form').addEventListener('submit', register);
  $('console-api').addEventListener('change', () => { const api = findAPI($('console-api').value); if (api) prepareRequest(api); else updateAuthHelp(); });
  ['try-method', 'try-path', 'try-key', 'try-bearer', 'try-headers', 'try-body', 'gateway-url'].forEach((id) => $(id).addEventListener('input', updateCurl));
  $('fill-sample')?.addEventListener('click', fillSamplePayload);
  document.querySelectorAll('[data-lang]').forEach((btn) => {
    btn.addEventListener('click', () => {
      state.codeLang = btn.dataset.lang;
      document.querySelectorAll('[data-lang]').forEach((el) => {
        el.classList.toggle('active', el === btn);
        el.setAttribute('aria-pressed', String(el === btn));
      });
      updateCurl();
    });
  });
  $('clear-credentials').addEventListener('click', () => { $('try-key').value = ''; $('try-bearer').value = ''; $('reg-key').value = ''; updateCurl(); toast('Credentials cleared'); });
  $('copy-curl').addEventListener('click', () => copy(codeSnippet()));
  $('copy-key').addEventListener('click', () => copy($('reg-key').value));
  $('copy-response').addEventListener('click', () => copy($('res-headers').hidden ? state.response : $('res-headers').textContent));
  $('reveal-key').addEventListener('click', () => { const show = $('reg-key').type === 'password'; $('reg-key').type = show ? 'text' : 'password'; $('reveal-key').textContent = show ? 'Hide' : 'Show'; $('reveal-key').setAttribute('aria-pressed', String(show)); });
  $('new-registration').addEventListener('click', () => { $('reg-result').hidden = true; $('registration-form').hidden = false; $('registration-form').reset(); $('reg-key').value = ''; $('try-key').value = ''; updateCurl(); $('reg-name').focus(); });
  $('refresh-usage')?.addEventListener('click', renderUsage);
  $('app-create-form')?.addEventListener('submit', async (e) => {
    e.preventDefault();
    if (!activeConsumer) { toast('Please register an account first in Get Access', 'error'); return; }
    try {
      await fetchJSON('/portal/api/apps', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          consumer_id: activeConsumer.id,
          name: $('app-name').value.trim(),
          environment: $('app-env').value,
          description: $('app-desc').value.trim()
        })
      });
      toast('Application created successfully!', 'success');
      $('app-create-form').reset();
      renderMyApps();
    } catch (err) {
      toast(err.message, 'error');
    }
  });
  document.querySelectorAll('[data-response]').forEach((button) => {
    button.setAttribute('aria-pressed', String(button.classList.contains('active')));
    button.addEventListener('click', () => {
      document.querySelectorAll('[data-response]').forEach((el) => { el.classList.toggle('active', el === button); el.setAttribute('aria-pressed', String(el === button)); });
      $('res-body').hidden = button.dataset.response !== 'body'; $('res-headers').hidden = button.dataset.response !== 'headers';
    });
  });
  document.querySelectorAll('[data-mcp-client]').forEach((btn) => {
    btn.addEventListener('click', () => {
      state.mcpClient = btn.dataset.mcpClient;
      document.querySelectorAll('[data-mcp-client]').forEach((el) => {
        el.classList.toggle('active', el === btn);
        el.setAttribute('aria-pressed', String(el === btn));
      });
      if (btn.dataset.mcpClient === 'sse') {
        if ($('mcp-transport-select')) $('mcp-transport-select').value = 'sse';
      }
      updateMCPConfig();
    });
  });

  $('mcp-transport-select')?.addEventListener('change', () => {
    updateMCPConfig();
  });

  $('mcp-app-select')?.addEventListener('change', (e) => {
    if (e.target.value) {
      if ($('mcp-custom-key')) $('mcp-custom-key').value = e.target.value;
    }
    updateMCPConfig();
  });

  $('mcp-custom-key')?.addEventListener('input', updateMCPConfig);

  const copyMCP = () => {
    const text = $('mcp-config-preview')?.textContent || '';
    copy(text);
    toast('Copied MCP configuration to clipboard!', 'success');
  };
  $('copy-mcp-config-link')?.addEventListener('click', copyMCP);
  $('copy-mcp-config-btn')?.addEventListener('click', copyMCP);

  document.querySelectorAll('.mcp-prompt-card').forEach((card) => {
    card.addEventListener('click', () => {
      const p = card.dataset.prompt;
      if (p) {
        copy(p);
        toast('Copied prompt to clipboard! Paste it into your AI assistant.', 'success');
      }
    });
  });

  window.addEventListener('hashchange', () => route());
  route(false); updateCurl(); loadCatalog();
})();
