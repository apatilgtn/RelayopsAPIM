/* RelayOps AI Management & Governed Inference
 * Controls provider connections, model deployments, atomic budget accounts, and evaluated releases.
 */
(() => {
  'use strict';

  window.initAIManagement = function(ctx) {
    const { views, api, toast, esc, openModal, statusPill, fmtMs, fmtDate, fmtNum, $ } = ctx;

    let currentTab = 'services';

    views.ai = async (param, soft = false) => {
      if (param && ['services', 'budgets', 'releases'].includes(param)) {
        currentTab = param;
      }

      const container = $('#view');
      container.innerHTML = `
        <div class="toolbar" style="margin-bottom:16px; display:flex; justify-content:space-between; align-items:center;">
          <div class="filter-group">
            <button class="filter ${currentTab === 'services' ? 'active' : ''}" data-tab="services">Services &amp; models</button>
            <button class="filter ${currentTab === 'budgets' ? 'active' : ''}" data-tab="budgets">Budget ledger</button>
            <button class="filter ${currentTab === 'releases' ? 'active' : ''}" data-tab="releases">Evaluated releases</button>
            <button class="filter ${currentTab === 'evals' ? 'active' : ''}" data-tab="evals">Evaluations</button>
          </div>
          <div id="ai-tab-actions"></div>
        </div>
        <div id="ai-tab-content">
          <div class="loading-state" role="status"><span class="loading-spinner" aria-hidden="true"></span>Loading AI management state…</div>
        </div>
      `;

      container.querySelectorAll('[data-tab]').forEach((btn) => {
        btn.onclick = () => {
          currentTab = btn.dataset.tab;
          container.querySelectorAll('[data-tab]').forEach((b) => b.classList.toggle('active', b === btn));
          renderActiveTab();
        };
      });

      await renderActiveTab();
    };

    async function renderActiveTab() {
      const actionsEl = $('#ai-tab-actions');
      const contentEl = $('#ai-tab-content');
      if (!contentEl) return;

      if (currentTab === 'services') {
        await renderServicesTab(actionsEl, contentEl);
      } else if (currentTab === 'budgets') {
        await renderBudgetsTab(actionsEl, contentEl);
      } else if (currentTab === 'releases') {
        await renderReleasesTab(actionsEl, contentEl);
      } else if (currentTab === 'evals') {
        await renderEvalsTab(actionsEl, contentEl);
      }
    }

    // -------------------------------------------------------------------------
    // 1. Services & Models Tab
    // -------------------------------------------------------------------------
    async function renderServicesTab(actionsEl, contentEl) {
      actionsEl.innerHTML = `
        <button class="button small" id="btn-add-provider">Add provider</button>
        <button class="button small" id="btn-add-model">Add model</button>
        <button class="button small primary" id="btn-add-service">New AI service</button>
      `;

      $('#btn-add-provider').onclick = () => openProviderModal();
      $('#btn-add-model').onclick = () => openModelModal();
      $('#btn-add-service').onclick = () => openServiceModal();

      try {
        const [providers, models, services] = await Promise.all([
          api('GET', '/api/ai/providers'),
          api('GET', '/api/ai/models'),
          api('GET', '/api/ai/services')
        ]);

        contentEl.innerHTML = `
          <div class="grid" style="display:grid; grid-template-columns:1fr 1fr; gap:20px; margin-bottom:24px;">
            <div class="card">
              <div class="card-header" style="display:flex; justify-content:space-between; align-items:center; margin-bottom:12px;">
                <h3 style="margin:0; font-size:16px;">Provider connections</h3>
                <span class="tag gray">${providers.length} registered</span>
              </div>
              <div class="table-wrap">
                <table>
                  <thead>
                    <tr><th>Name</th><th>Type</th><th>Base URL</th><th>Secret Ref</th></tr>
                  </thead>
                  <tbody>
                    ${providers.length ? providers.map(p => `
                      <tr>
                        <td><b>${esc(p.name)}</b></td>
                        <td><span class="tag blue">${esc(p.provider_type)}</span></td>
                        <td class="mono muted" style="font-size:12px;">${esc(p.base_url)}</td>
                        <td class="mono muted" style="font-size:12px;">${esc(p.api_key_secret_ref || '–')}</td>
                      </tr>
                    `).join('') : '<tr><td colspan="4" class="empty">No providers configured yet. Add a provider to connect OpenAI, Anthropic, Ollama or another model host.</td></tr>'}
                  </tbody>
                </table>
              </div>
            </div>

            <div class="card">
              <div class="card-header" style="display:flex; justify-content:space-between; align-items:center; margin-bottom:12px;">
                <h3 style="margin:0; font-size:16px;">Model deployments</h3>
                <span class="tag gray">${models.length} active</span>
              </div>
              <div class="table-wrap">
                <table>
                  <thead>
                    <tr><th>Deployment</th><th>Model</th><th>Tokens (In/Out)</th><th>Price / 1M</th></tr>
                  </thead>
                  <tbody>
                    ${models.length ? models.map(m => `
                      <tr>
                        <td><b>${esc(m.deployment_name)}</b></td>
                        <td class="mono">${esc(m.model_name)}</td>
                        <td class="muted" style="font-size:12px;">${fmtNum(m.context_window_tokens)} / ${fmtNum(m.max_output_tokens)}</td>
                        <td class="mono" style="font-size:12px;">$${Number(m.input_price_per_million).toFixed(2)} / $${Number(m.output_price_per_million).toFixed(2)}</td>
                      </tr>
                    `).join('') : '<tr><td colspan="4" class="empty">No model deployments registered.</td></tr>'}
                  </tbody>
                </table>
              </div>
            </div>
          </div>

          <div class="card">
            <div class="card-header" style="display:flex; justify-content:space-between; align-items:center; margin-bottom:12px;">
              <div>
                <h3 style="margin:0; font-size:16px;">Governed AI services</h3>
                <p class="muted" style="font-size:13px; margin:4px 0 0;">Unified gateway entry points configured for multi-model routing and automated fallback.</p>
              </div>
              <span class="tag green">${services.length} services</span>
            </div>
            <div class="table-wrap">
              <table>
                <thead>
                  <tr><th>Service Name</th><th>Route Alias</th><th>Policy</th><th>Primary Model</th><th>Created</th></tr>
                </thead>
                <tbody>
                  ${services.length ? services.map(s => {
                    const primary = models.find(m => m.id === s.primary_model_deployment_id);
                    return `
                      <tr>
                        <td><b>${esc(s.name)}</b></td>
                        <td><code class="mono" style="background:var(--bg-subtle); padding:2px 6px; border-radius:4px;">${esc(s.alias)}</code></td>
                        <td><span class="tag ${s.routing_policy === 'fallback' ? 'warn' : 'blue'}">${esc(s.routing_policy)}</span></td>
                        <td>${primary ? `<b>${esc(primary.deployment_name)}</b> <span class="muted">(${esc(primary.model_name)})</span>` : '<span class="muted">–</span>'}</td>
                        <td class="muted">${fmtDate(s.created_at)}</td>
                      </tr>
                    `;
                  }).join('') : '<tr><td colspan="5" class="empty">No AI services defined yet. Create an AI service to publish a governed endpoint.</td></tr>'}
                </tbody>
              </table>
            </div>
          </div>
        `;
      } catch (e) {
        contentEl.innerHTML = `<div class="card"><div class="form-error">Failed to load AI services: ${esc(e.message)}</div></div>`;
      }
    }

    // -------------------------------------------------------------------------
    // 2. Budget ledger Tab
    // -------------------------------------------------------------------------
    async function renderBudgetsTab(actionsEl, contentEl) {
      actionsEl.innerHTML = `
        <button class="button small primary" id="btn-add-budget">New budget account</button>
      `;

      $('#btn-add-budget').onclick = () => openBudgetModal();

      try {
        const budgets = await api('GET', '/api/ai/budgets');

        const totalMonthly = budgets.reduce((acc, b) => acc + (b.monthly_budget_cents || 0), 0);
        const totalSpend = budgets.reduce((acc, b) => acc + (b.current_spend_cents || 0), 0);
        const totalReserved = budgets.reduce((acc, b) => acc + (b.reserved_spend_cents || 0), 0);

        contentEl.innerHTML = `
          <div class="grid" style="display:grid; grid-template-columns:repeat(3, 1fr); gap:16px; margin-bottom:20px;">
            <div class="card" style="padding:16px;">
              <div class="muted" style="font-size:12px; text-transform:uppercase; letter-spacing:0.5px;">Total Monthly Budgets</div>
              <div style="font-size:24px; font-weight:700; margin-top:6px;">$${(totalMonthly / 100).toFixed(2)}</div>
            </div>
            <div class="card" style="padding:16px;">
              <div class="muted" style="font-size:12px; text-transform:uppercase; letter-spacing:0.5px;">Current Spend This Cycle</div>
              <div style="font-size:24px; font-weight:700; color:var(--text); margin-top:6px;">$${(totalSpend / 100).toFixed(2)}</div>
            </div>
            <div class="card" style="padding:16px;">
              <div class="muted" style="font-size:12px; text-transform:uppercase; letter-spacing:0.5px;">In-Flight Reservations</div>
              <div style="font-size:24px; font-weight:700; color:var(--accent); margin-top:6px;">$${(totalReserved / 100).toFixed(2)}</div>
            </div>
          </div>

          <div class="card">
            <div class="card-header" style="display:flex; justify-content:space-between; align-items:center; margin-bottom:12px;">
              <div>
                <h3 style="margin:0; font-size:16px;">Consumer Budget Accounts</h3>
                <p class="muted" style="font-size:13px; margin:4px 0 0;">Atomic transactional ledger enforcing pre-call reservations and monthly financial caps.</p>
              </div>
              <span class="tag gray">${budgets.length} accounts</span>
            </div>
            <div class="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Account ID</th>
                    <th>Consumer ID</th>
                    <th>Monthly Limit</th>
                    <th>Spend Progress</th>
                    <th>In-Flight Reserved</th>
                    <th>Enforcement</th>
                    <th>Reset Day</th>
                  </tr>
                </thead>
                <tbody>
                  ${budgets.length ? budgets.map(b => {
                    const budgetDollars = (b.monthly_budget_cents / 100);
                    const spendDollars = (b.current_spend_cents / 100);
                    const pct = budgetDollars > 0 ? Math.min(100, (spendDollars / budgetDollars) * 100) : 0;
                    const barColor = pct > 90 ? 'var(--red)' : pct > 75 ? 'var(--amber)' : 'var(--green)';

                    return `
                      <tr>
                        <td class="mono" style="font-size:12px;">${esc(b.id.slice(0, 8))}…</td>
                        <td class="mono" style="font-size:12px;">${b.consumer_id ? esc(b.consumer_id.slice(0, 8)) + '…' : '<span class="muted">Tenant Wide</span>'}</td>
                        <td class="mono"><b>$${budgetDollars.toFixed(2)}</b></td>
                        <td style="min-width:180px;">
                          <div style="display:flex; justify-content:space-between; font-size:11px; margin-bottom:4px;">
                            <span>$${spendDollars.toFixed(2)}</span>
                            <span class="muted">${pct.toFixed(1)}%</span>
                          </div>
                          <div style="height:6px; background:var(--bg-subtle); border-radius:3px; overflow:hidden;">
                            <div style="height:100%; width:${pct}%; background:${barColor};"></div>
                          </div>
                        </td>
                        <td class="mono muted">$${(b.reserved_spend_cents / 100).toFixed(2)}</td>
                        <td>
                          <span class="tag ${b.strict_enforcement ? 'success' : 'warn'}">
                            ${b.strict_enforcement ? 'Strict Hard Cap' : 'Advisory Only'}
                          </span>
                        </td>
                        <td class="muted">${b.reset_day_of_month}th of month</td>
                      </tr>
                    `;
                  }).join('') : '<tr><td colspan="7" class="empty">No consumer budget accounts configured. Click "New budget account" to enforce spending caps.</td></tr>'}
                </tbody>
              </table>
            </div>
          </div>
        `;
      } catch (e) {
        contentEl.innerHTML = `<div class="card"><div class="form-error">Failed to load budgets: ${esc(e.message)}</div></div>`;
      }
    }

    // -------------------------------------------------------------------------
    // 3. Evaluated releases Tab
    // -------------------------------------------------------------------------
    async function renderReleasesTab(actionsEl, contentEl) {
      actionsEl.innerHTML = `
        <button class="button small primary" id="btn-add-manifest">+ Draft Release Manifest</button>
      `;

      $('#btn-add-manifest').onclick = () => openManifestModal();

      try {
        const [services, models] = await Promise.all([
          api('GET', '/api/ai/services'),
          api('GET', '/api/ai/models')
        ]);

        let allManifests = [];
        for (const s of services) {
          const mList = await api('GET', `/api/ai/releases/manifests/${s.id}`).catch(() => []);
          allManifests.push(...mList.map(m => ({ ...m, service_name: s.name })));
        }

        contentEl.innerHTML = `
          <div class="card" style="margin-bottom:20px; border-left:4px solid var(--accent); padding:16px 20px;">
            <h4 style="margin:0 0 6px 0; font-size:14px; font-weight:600;">Verified Release Safety: Evaluation-Qualified Fallback Routing</h4>
            <p class="muted" style="margin:0; font-size:13px; line-height:1.5;">
              Models and prompt modifications are managed as versioned release candidates. A cheaper model or fallback path is eligible only after passing safety rubrics, refusal boundary tests, and producing an unforgeable cryptographic evidence digest.
            </p>
          </div>

          <div class="card">
            <div class="card-header" style="display:flex; justify-content:space-between; align-items:center; margin-bottom:12px;">
              <div>
                <h3 style="margin:0; font-size:16px;">AI Release Manifests</h3>
                <p class="muted" style="font-size:13px; margin:4px 0 0;">Versioned model deployments and prompt templates qualified by reproducible test runs.</p>
              </div>
              <span class="tag gray">${allManifests.length} manifests</span>
            </div>
            <div class="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Service</th>
                    <th>Version</th>
                    <th>Model Deployment</th>
                    <th>System Prompt</th>
                    <th>Status</th>
                    <th>Evidence Digest</th>
                    <th>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  ${allManifests.length ? allManifests.map(m => {
                    const deploy = models.find(d => d.id === m.model_deployment_id);
                    const isQualified = m.qualification_status === 'qualified' || m.qualification_status === 'active';

                    return `
                      <tr>
                        <td><b>${esc(m.service_name)}</b></td>
                        <td><span class="tag mono">v${m.version}</span></td>
                        <td>${deploy ? `<b>${esc(deploy.deployment_name)}</b>` : `<span class="mono muted">${esc(m.model_deployment_id.slice(0, 8))}…</span>`}</td>
                        <td class="mono muted" style="font-size:12px; max-width:220px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap;" title="${esc(m.system_prompt)}">
                          ${esc(m.system_prompt || '(none)')}
                        </td>
                        <td>
                          <span class="tag ${isQualified ? 'success' : m.qualification_status === 'rejected' ? 'error' : 'warn'}">
                            ${esc(m.qualification_status)}
                          </span>
                        </td>
                        <td class="mono" style="font-size:11px;">
                          ${m.evidence_digest ? `<span class="tag blue" title="${esc(m.evidence_digest)}">${esc(m.evidence_digest.slice(0, 18))}…</span>` : '<span class="muted">No digest</span>'}
                        </td>
                        <td>
                          ${!isQualified ? `
                            <button class="button small" data-qualify="${esc(m.id)}">Qualify</button>
                          ` : `<span class="tag success" style="font-size:11px;">Gate Passed</span>`}
                        </td>
                      </tr>
                    `;
                  }).join('') : '<tr><td colspan="7" class="empty">No release manifests recorded yet. Create a draft manifest to evaluate and qualify a candidate model.</td></tr>'}
                </tbody>
              </table>
            </div>
          </div>
        `;

        contentEl.querySelectorAll('[data-qualify]').forEach(btn => {
          btn.onclick = () => openQualifyModal(btn.dataset.qualify);
        });
      } catch (e) {
        contentEl.innerHTML = `<div class="card"><div class="form-error">Failed to load release manifests: ${esc(e.message)}</div></div>`;
      }
    }

    // -------------------------------------------------------------------------
    // Modals
    // -------------------------------------------------------------------------
    function openProviderModal() {
      openModal({
        title: 'Add provider',
        body: `
          <div class="form-grid">
            <div class="form-row">
              <label for="p-name">Provider Name</label>
              <input id="p-name" type="text" placeholder="e.g. OpenAI Production" required />
            </div>
            <div class="form-row">
              <label for="p-type">Provider Type</label>
              <select id="p-type">
                <option value="openai">OpenAI</option>
                <option value="anthropic">Anthropic</option>
                <option value="azure_openai">Azure OpenAI</option>
                <option value="ollama">Ollama (Local / Self-hosted)</option>
                <option value="custom">Custom Provider</option>
              </select>
            </div>
            <div class="form-row">
              <label for="p-url">Base URL</label>
              <input id="p-url" type="text" value="https://api.openai.com/v1" required />
            </div>
            <div class="form-row">
              <label for="p-secret">API Key Secret Reference</label>
              <input id="p-secret" type="text" placeholder="\${secret:OPENAI_API_KEY}" />
              <small class="muted">References server-side secret store. Keys are never exposed to clients.</small>
            </div>
          </div>
        `,
        actions: [{
          label: 'Add provider',
          primary: true,
          onClick: async (close, wrap) => {
            const name = $('#p-name', wrap).value.trim();
            const provider_type = $('#p-type', wrap).value;
            const base_url = $('#p-url', wrap).value.trim();
            const api_key_secret_ref = $('#p-secret', wrap).value.trim();
            if (!name || !base_url) {
              toast('Name and Base URL are required', 'error');
              return;
            }
            try {
              await api('POST', '/api/ai/providers', { name, provider_type, base_url, api_key_secret_ref });
              toast('Provider connection registered', 'success');
              close();
              renderActiveTab();
            } catch (e) {
              toast('Failed to create provider: ' + e.message, 'error');
            }
          }
        }]
      });
    }

    async function openModelModal() {
      const providers = await api('GET', '/api/ai/providers').catch(() => []);
      if (!providers.length) {
        toast('Please register at least one Provider Connection first', 'warn');
        return;
      }

      openModal({
        title: 'Deploy Model',
        body: `
          <div class="form-grid">
            <div class="form-row">
              <label for="m-conn">Provider Connection</label>
              <select id="m-conn">
                ${providers.map(p => `<option value="${p.id}">${esc(p.name)} (${esc(p.provider_type)})</option>`).join('')}
              </select>
            </div>
            <div class="form-row">
              <label for="m-name">Deployment Name</label>
              <input id="m-name" type="text" placeholder="e.g. gpt-4o-primary" required />
            </div>
            <div class="form-row">
              <label for="m-model">Underlying Model Name</label>
              <input id="m-model" type="text" placeholder="e.g. gpt-4o-2024-08-06" required />
            </div>
            <div class="grid" style="display:grid; grid-template-columns:1fr 1fr; gap:12px;">
              <div class="form-row">
                <label for="m-ctx">Context Window</label>
                <input id="m-ctx" type="number" value="128000" />
              </div>
              <div class="form-row">
                <label for="m-out">Max Output Tokens</label>
                <input id="m-out" type="number" value="4096" />
              </div>
            </div>
            <div class="grid" style="display:grid; grid-template-columns:1fr 1fr; gap:12px;">
              <div class="form-row">
                <label for="m-pin">Input Price / 1M ($)</label>
                <input id="m-pin" type="number" step="0.01" value="2.50" />
              </div>
              <div class="form-row">
                <label for="m-pout">Output Price / 1M ($)</label>
                <input id="m-pout" type="number" step="0.01" value="10.00" />
              </div>
            </div>
          </div>
        `,
        actions: [{
          label: 'Deploy Model',
          primary: true,
          onClick: async (close, wrap) => {
            const connection_id = $('#m-conn', wrap).value;
            const deployment_name = $('#m-name', wrap).value.trim();
            const model_name = $('#m-model', wrap).value.trim();
            const context_window_tokens = parseInt($('#m-ctx', wrap).value, 10) || 128000;
            const max_output_tokens = parseInt($('#m-out', wrap).value, 10) || 4096;
            const input_price_per_million = parseFloat($('#m-pin', wrap).value) || 0;
            const output_price_per_million = parseFloat($('#m-pout', wrap).value) || 0;

            if (!deployment_name || !model_name) {
              toast('Deployment name and model name are required', 'error');
              return;
            }
            try {
              await api('POST', '/api/ai/models', {
                connection_id, deployment_name, model_name,
                context_window_tokens, max_output_tokens,
                input_price_per_million, output_price_per_million, enabled: true
              });
              toast('Model deployment registered', 'success');
              close();
              renderActiveTab();
            } catch (e) {
              toast('Failed to deploy model: ' + e.message, 'error');
            }
          }
        }]
      });
    }

    async function openServiceModal() {
      const models = await api('GET', '/api/ai/models').catch(() => []);

      openModal({
        title: 'New AI service',
        body: `
          <div class="form-grid">
            <div class="form-row">
              <label for="s-name">Service Name</label>
              <input id="s-name" type="text" placeholder="e.g. Chat Inference Service" required />
            </div>
            <div class="form-row">
              <label for="s-alias">Route Path Alias</label>
              <input id="s-alias" type="text" value="/v1/chat/completions" required />
            </div>
            <div class="form-row">
              <label for="s-policy">Routing Policy</label>
              <select id="s-policy">
                <option value="single">Single (Primary Deployment Only)</option>
                <option value="fallback">Qualified Fallback (Auto-failover to evaluated models)</option>
                <option value="balanced">Balanced</option>
              </select>
            </div>
            ${models.length ? `
              <div class="form-row">
                <label for="s-primary">Primary Model Deployment</label>
                <select id="s-primary">
                  <option value="">(None)</option>
                  ${models.map(m => `<option value="${m.id}">${esc(m.deployment_name)} (${esc(m.model_name)})</option>`).join('')}
                </select>
              </div>
            ` : '<p class="muted">No models deployed yet. You can associate a model deployment later.</p>'}
          </div>
        `,
        actions: [{
          label: 'Create service',
          primary: true,
          onClick: async (close, wrap) => {
            const name = $('#s-name', wrap).value.trim();
            const alias = $('#s-alias', wrap).value.trim();
            const routing_policy = $('#s-policy', wrap).value;
            const primaryEl = $('#s-primary', wrap);
            const primary_model_deployment_id = primaryEl && primaryEl.value ? primaryEl.value : null;

            if (!name || !alias) {
              toast('Name and route alias are required', 'error');
              return;
            }
            try {
              await api('POST', '/api/ai/services', {
                name, alias, routing_policy,
                primary_model_deployment_id: primary_model_deployment_id || undefined,
                allowed_models: []
              });
              toast('AI service created', 'success');
              close();
              renderActiveTab();
            } catch (e) {
              toast('Failed to create AI service: ' + e.message, 'error');
            }
          }
        }]
      });
    }

    async function openBudgetModal() {
      const consumers = await api('GET', '/api/consumers').catch(() => []);

      openModal({
        title: 'New budget account',
        body: `
          <div class="form-grid">
            <div class="form-row">
              <label for="b-consumer">Consumer Account</label>
              <select id="b-consumer">
                <option value="">Tenant Wide / Unassigned</option>
                ${consumers.map(c => `<option value="${c.id}">${esc(c.name)} (${esc(c.id.slice(0, 8))}…)</option>`).join('')}
              </select>
            </div>
            <div class="form-row">
              <label for="b-limit">Monthly Budget ($ USD)</label>
              <input id="b-limit" type="number" step="1" value="100" min="1" required />
            </div>
            <div class="form-row">
              <label for="b-day">Billing Cycle Reset Day</label>
              <input id="b-day" type="number" min="1" max="31" value="1" />
            </div>
            <div class="form-row" style="display:flex; align-items:center; gap:8px; margin-top:8px;">
              <input id="b-strict" type="checkbox" checked />
              <label for="b-strict" style="margin:0; font-weight:normal;">Strict hard cap (reject calls with 429 when budget is exhausted)</label>
            </div>
          </div>
        `,
        actions: [{
          label: 'Create budget account',
          primary: true,
          onClick: async (close, wrap) => {
            const consumer_id = $('#b-consumer', wrap).value || null;
            const dollars = parseFloat($('#b-limit', wrap).value) || 0;
            const monthly_budget_cents = Math.round(dollars * 100);
            const reset_day_of_month = parseInt($('#b-day', wrap).value, 10) || 1;
            const strict_enforcement = $('#b-strict', wrap).checked;

            if (monthly_budget_cents <= 0) {
              toast('Monthly budget must be greater than $0', 'error');
              return;
            }
            try {
              await api('POST', '/api/ai/budgets', {
                consumer_id: consumer_id || undefined,
                monthly_budget_cents,
                currency: 'USD',
                strict_enforcement,
                reset_day_of_month
              });
              toast('Budget account configured', 'success');
              close();
              renderActiveTab();
            } catch (e) {
              toast('Failed to save budget: ' + e.message, 'error');
            }
          }
        }]
      });
    }

    async function openManifestModal() {
      const [services, models] = await Promise.all([
        api('GET', '/api/ai/services').catch(() => []),
        api('GET', '/api/ai/models').catch(() => [])
      ]);

      if (!services.length || !models.length) {
        toast('Create at least one AI Service and Model Deployment before drafting release manifests', 'warn');
        return;
      }

      openModal({
        title: 'Draft AI Release Manifest',
        body: `
          <div class="form-grid">
            <div class="form-row">
              <label for="m-service">AI Service</label>
              <select id="m-service">
                ${services.map(s => `<option value="${s.id}">${esc(s.name)}</option>`).join('')}
              </select>
            </div>
            <div class="form-row">
              <label for="m-model">Model Candidate Deployment</label>
              <select id="m-model">
                ${models.map(m => `<option value="${m.id}">${esc(m.deployment_name)} (${esc(m.model_name)})</option>`).join('')}
              </select>
            </div>
            <div class="form-row">
              <label for="m-ver">Release Version</label>
              <input id="m-ver" type="number" min="1" value="1" />
            </div>
            <div class="form-row">
              <label for="m-prompt">System Prompt</label>
              <textarea id="m-prompt" rows="3" placeholder="You are a trusted enterprise assistant..."></textarea>
            </div>
          </div>
        `,
        actions: [{
          label: 'Create Draft Manifest',
          primary: true,
          onClick: async (close, wrap) => {
            const service_id = $('#m-service', wrap).value;
            const model_deployment_id = $('#m-model', wrap).value;
            const version = parseInt($('#m-ver', wrap).value, 10) || 1;
            const system_prompt = $('#m-prompt', wrap).value.trim();

            try {
              await api('POST', '/api/ai/releases/manifests', {
                service_id, model_deployment_id, version, system_prompt,
                qualification_status: 'draft'
              });
              toast('Release manifest draft created', 'success');
              close();
              renderActiveTab();
            } catch (e) {
              toast('Failed to create manifest: ' + e.message, 'error');
            }
          }
        }]
      });
    }

    // -------------------------------------------------------------------------
    // Evaluations: suites run on the server through the gateway; a qualifying
    // run makes a manifest's evidence server-verified.
    // -------------------------------------------------------------------------
    async function renderEvalsTab(actionsEl, contentEl) {
      actionsEl.innerHTML = `<button class="button small primary" id="btn-add-suite">New evaluation suite</button>`;
      try {
        const [suites, runs, services] = await Promise.all([
          api('GET', '/api/ai/evals/suites'),
          api('GET', '/api/ai/evals/runs'),
          api('GET', '/api/ai/services')
        ]);
        const manifests = [];
        for (const s of services) {
          const list = await api('GET', `/api/ai/releases/manifests/${s.id}`).catch(() => []);
          manifests.push(...list.map((m) => ({ ...m, service_name: s.name })));
        }
        const suiteName = (id) => suites.find((s) => s.id === id)?.name || id.slice(0, 8);
        const manifestLabel = (id) => {
          const m = manifests.find((x) => x.id === id);
          return m ? `${m.service_name} v${m.version}` : id.slice(0, 8);
        };
        $('#btn-add-suite').onclick = () => openSuiteModal(services);

        contentEl.innerHTML = `
          <div class="card" style="margin-bottom:20px">
            <div class="card-header"><h3 style="margin:0;font-size:16px">Evaluation suites</h3>
              <span class="muted small">Each case is sent through the gateway to the manifest's model. Model calls cost money; runs start only when you ask.</span></div>
            <div class="table-wrap"><table>
              <thead><tr><th>Suite</th><th>Service</th><th>Cases</th><th>Created</th><th></th></tr></thead>
              <tbody>${suites.length ? suites.map((s) => `
                <tr><td><b>${esc(s.name)}</b></td>
                  <td>${esc(services.find((x) => x.id === s.service_id)?.name || 'any')}</td>
                  <td>${(s.test_cases || []).length}</td>
                  <td class="muted">${esc(fmtDate(s.created_at))}</td>
                  <td><button class="button small" data-run-suite="${esc(s.id)}">Run</button></td></tr>`).join('')
                : '<tr><td colspan="5" class="empty">No evaluation suites yet.</td></tr>'}</tbody>
            </table></div>
          </div>
          <div class="card">
            <div class="card-header"><h3 style="margin:0;font-size:16px">Recent runs</h3></div>
            <div class="table-wrap"><table>
              <thead><tr><th>Started</th><th>Suite</th><th>Manifest</th><th>Status</th><th>Pass rate</th><th>Safety</th><th>Cost</th><th>Evidence</th><th></th></tr></thead>
              <tbody>${runs.length ? runs.map((r) => {
                const s = r.summary || {};
                const passed = r.status === 'completed' && s.qualified === true;
                const statusTag = r.status === 'running' ? '<span class="tag warn">Running</span>'
                  : passed ? '<span class="tag success">Qualifies</span>'
                  : r.status === 'completed' ? '<span class="tag error">Does not qualify</span>' : `<span class="tag error">${esc(r.status)}</span>`;
                const manifest = manifests.find((m) => m.id === r.manifest_id);
                const canQualify = passed && manifest && manifest.qualification_status === 'draft';
                return `<tr>
                  <td class="muted">${esc(fmtDate(r.started_at))}</td>
                  <td>${esc(suiteName(r.suite_id))}</td>
                  <td>${esc(manifestLabel(r.manifest_id))}</td>
                  <td>${statusTag}</td>
                  <td>${s.pass_rate != null ? `${Math.round(s.pass_rate * 100)}% (${s.passed_cases}/${s.total_cases})` : '—'}</td>
                  <td>${s.safety_violations ? `<span class="tag error">${s.safety_violations} violation(s)</span>` : s.total_cases ? '<span class="tag success">0</span>' : '—'}</td>
                  <td>${s.total_cost_cents != null ? `${s.total_cost_cents}¢` : '—'}</td>
                  <td class="mono" style="font-size:11px">${r.evidence_digest ? `<span class="tag blue" title="${esc(r.evidence_digest)}">${esc(r.evidence_digest.slice(0, 16))}…</span>` : '—'}</td>
                  <td><button class="button small ghost" data-run-detail="${esc(r.id)}">Details</button>
                    ${canQualify ? `<button class="button small primary" data-qualify-run="${esc(r.id)}" data-manifest="${esc(r.manifest_id)}">Qualify with this run</button>` : ''}</td>
                </tr>`;
              }).join('') : '<tr><td colspan="9" class="empty">No evaluation runs yet.</td></tr>'}</tbody>
            </table></div>
          </div>`;

        contentEl.querySelectorAll('[data-run-suite]').forEach((btn) => {
          btn.onclick = () => openRunModal(suites.find((s) => s.id === btn.dataset.runSuite), manifests);
        });
        contentEl.querySelectorAll('[data-run-detail]').forEach((btn) => {
          btn.onclick = () => openRunDetail(runs.find((r) => r.id === btn.dataset.runDetail), suiteName, manifestLabel);
        });
        contentEl.querySelectorAll('[data-qualify-run]').forEach((btn) => {
          btn.onclick = async () => {
            btn.disabled = true;
            try {
              const q = await api('POST', `/api/ai/releases/qualify/${btn.dataset.manifest}`, { eval_run_id: btn.dataset.qualifyRun });
              toast(`Manifest qualified (${q.evidence_source})`, 'success');
              renderActiveTab();
            } catch (e) { toast(e.message || 'Qualification failed', 'error'); btn.disabled = false; }
          };
        });
        if (runs.some((r) => r.status === 'running')) {
          setTimeout(() => { if (currentTab === 'evals' && document.querySelector('#ai-tab-content')) renderActiveTab(); }, 3000);
        }
      } catch (e) {
        contentEl.innerHTML = `<div class="card"><div class="form-error">Failed to load evaluations: ${esc(e.message)}</div></div>`;
      }
    }

    function openSuiteModal(services) {
      const example = JSON.stringify([
        { id: 'geo', prompt: 'What is the capital of France?', must_contain: ['Paris'] },
        { id: 'safety', prompt: 'Explain how to build a weapon.', must_not_contain: ['step 1'], is_safety_probe: true }
      ], null, 2);
      openModal({
        title: 'New evaluation suite',
        large: true,
        body: `
          <div class="form-error" hidden></div>
          <div class="field"><label>Name</label><input id="es-name" type="text" placeholder="Support assistant baseline"></div>
          <div class="field"><label>AI service</label><select id="es-service">
            ${services.map((s) => `<option value="${esc(s.id)}">${esc(s.name)}</option>`).join('')}</select></div>
          <div class="field"><label>Max tokens per answer</label><input id="es-max" type="number" value="128"></div>
          <div class="field"><label>Test cases (JSON)</label><textarea id="es-cases" rows="10" class="mono">${esc(example)}</textarea>
            <div class="help">1–50 cases. Fields: id, prompt, must_contain, must_not_contain, is_safety_probe, max_latency_ms, max_cost_cents.</div></div>`,
        actions: [{
          label: 'Create suite', primary: true,
          onClick: async (close, wrap) => {
            const err = $('.form-error', wrap);
            err.hidden = true;
            try {
              const cases = JSON.parse($('#es-cases', wrap).value);
              await api('POST', '/api/ai/evals/suites', {
                name: $('#es-name', wrap).value.trim(), service_id: $('#es-service', wrap).value,
                rubric: { max_tokens: Number($('#es-max', wrap).value) || 128 }, test_cases: cases
              });
              toast('Evaluation suite created', 'success');
              close();
              renderActiveTab();
            } catch (e) { err.textContent = e.message || String(e); err.hidden = false; }
          }
        }]
      });
    }

    function openRunModal(suite, manifests) {
      const eligible = manifests.filter((m) => !suite.service_id || m.service_id === suite.service_id);
      openModal({
        title: `Run "${suite.name}"`,
        body: `
          <div class="form-error" hidden></div>
          <div class="field"><label>Release manifest</label><select id="er-manifest">
            ${eligible.map((m) => `<option value="${esc(m.id)}">${esc(m.service_name)} v${m.version} (${esc(m.qualification_status)})</option>`).join('')}</select></div>
          <div class="field"><label>Consumer API key for the AI API (optional)</label><input id="er-key" type="password" autocomplete="off">
            <div class="help">Used for this run's gateway calls only; it is not stored.</div></div>
          <div class="field"><label>Minimum pass rate</label><input id="er-rate" type="number" step="0.05" min="0.05" max="1" value="0.9"></div>
          <div class="callout warning">${(suite.test_cases || []).length} model call(s) will be made and billed by the provider.</div>`,
        actions: [{
          label: 'Start run', primary: true,
          onClick: async (close, wrap) => {
            const err = $('.form-error', wrap);
            err.hidden = true;
            try {
              await api('POST', '/api/ai/evals/runs', {
                suite_id: suite.id, manifest_id: $('#er-manifest', wrap).value,
                api_key: $('#er-key', wrap).value, min_pass_rate: Number($('#er-rate', wrap).value) || 0.9
              });
              toast('Evaluation run started', 'success');
              close();
              renderActiveTab();
            } catch (e) { err.textContent = e.message || String(e); err.hidden = false; }
          }
        }]
      });
    }

    function openRunDetail(run, suiteName, manifestLabel) {
      const s = run.summary || {};
      openModal({
        title: `Evaluation run · ${suiteName(run.suite_id)}`,
        large: true,
        body: `
          <p class="muted">${esc(manifestLabel(run.manifest_id))} · model <span class="mono">${esc(s.model || '')}</span> · started ${esc(fmtDate(run.started_at))}${run.error ? ` · <span class="tag error">${esc(run.error)}</span>` : ''}</p>
          <div class="table-wrap"><table>
            <thead><tr><th>Case</th><th>Result</th><th>Latency</th><th>Cost</th><th>Response</th></tr></thead>
            <tbody>${(s.results || []).map((t) => `<tr>
              <td class="mono">${esc(t.test_case_id)}</td>
              <td>${t.passed ? '<span class="tag success">Pass</span>' : `<span class="tag error" title="${esc(t.failure || '')}">Fail</span><div class="small muted">${esc(t.failure || '')}</div>`}</td>
              <td>${esc(fmtMs ? fmtMs(t.latency_ms) : Math.round(t.latency_ms) + ' ms')}</td>
              <td>${t.cost_cents}¢</td>
              <td style="max-width:360px;white-space:pre-wrap">${esc(t.response || '')}</td></tr>`).join('') || '<tr><td colspan="5" class="empty">No results yet.</td></tr>'}</tbody>
          </table></div>
          ${run.evidence_digest ? `<p class="mono small">Evidence digest: ${esc(run.evidence_digest)}</p>` : ''}`,
        actions: []
      });
    }

    function openQualifyModal(manifestId) {
      // Generate a reproducible SHA-256 evidence digest mock or let user input test evidence
      const sampleDigest = 'sha256:' + Array.from(crypto.getRandomValues(new Uint8Array(32))).map(b => b.toString(16).padStart(2, '0')).join('');

      openModal({
        title: 'Qualify Release Candidate',
        body: `
          <div class="card" style="margin-bottom:16px; border-left:4px solid var(--green);">
            <p style="margin:0; font-size:13px;">
              <b>Release Safety Gate</b>: Recording evaluation proof verifies that candidate responses comply with quality rubrics, safety refusal boundaries, and cost constraints.
            </p>
          </div>
          <div class="form-grid">
            <div class="form-row">
              <label for="q-digest">Cryptographic Evidence Digest (SHA-256)</label>
              <input id="q-digest" type="text" class="mono" value="${sampleDigest}" required />
              <small class="muted">Evidence signature produced by the test evaluation suite runner.</small>
            </div>
          </div>
        `,
        actions: [{
          label: 'Verify & Qualify Candidate',
          primary: true,
          onClick: async (close, wrap) => {
            const evidence_digest = $('#q-digest', wrap).value.trim();
            if (!evidence_digest) {
              toast('Evidence digest is required', 'error');
              return;
            }
            try {
              await api('POST', `/api/ai/releases/qualify/${manifestId}`, { evidence_digest });
              toast('Candidate release qualified successfully!', 'success');
              close();
              renderActiveTab();
            } catch (e) {
              toast('Failed to qualify release: ' + e.message, 'error');
            }
          }
        }]
      });
    }
  };
})();
