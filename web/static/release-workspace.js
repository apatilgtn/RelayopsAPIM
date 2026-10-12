/* Guided releases use server-owned drafts and gate decisions. No credentials are persisted here. */
(() => {
  'use strict';
  window.initReleaseWorkspace = ({ views, api, esc, $, toast }) => {
    const labels = ['Import', 'Protect', 'Test', 'Review change', 'Canary', 'Release decision', 'Evidence'];
    const stateLabel = s => ({ not_started: 'Not started', in_progress: 'In progress', passed: 'Passed', blocked: 'Blocked', stale: 'Stale', unavailable: 'Unavailable' }[s] || 'Unavailable');
    const download = (name, data) => {
      const url = URL.createObjectURL(new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' }));
      const a = document.createElement('a'); a.href = url; a.download = name; a.click(); setTimeout(() => URL.revokeObjectURL(url), 1000);
    };
    async function run(button, fn) {
      button.disabled = true;
      const error = $('#journey-error'); if (error) error.hidden = true;
      try { await fn(); } catch (e) { if (error) { error.textContent = e.message; error.hidden = false; error.focus(); } else toast(esc(e.message), 'error'); }
      finally { button.disabled = false; }
    }
    const go = (id, step) => { location.hash = '#/release/' + id + '?step=' + step; };
    let renderVersion = 0;
    function revealCurrentStep() {
      const nav = document.querySelector('.journey-steps'), active = nav?.querySelector('[aria-current=step]');
      if (active && nav.scrollWidth > nav.clientWidth) nav.scrollLeft += active.getBoundingClientRect().left - nav.getBoundingClientRect().left - (nav.clientWidth - active.offsetWidth) / 2;
    }
    window.addEventListener('resize', () => requestAnimationFrame(revealCurrentStep));
    views.release = async param => {
      const render = ++renderVersion;
      const [id, query] = (param || '').split('?');
      const step = Math.min(6, Math.max(0, Number(new URLSearchParams(query).get('step')) || 0));
      if (!id || id === 'new' || id.startsWith('api-')) return start(id, render);
      const [data, tenants] = await Promise.all([api('GET', `/api/release-drafts/${encodeURIComponent(id)}/journey`), api('GET', '/api/tenants')]);
      if (render !== renderVersion) return;
      const d = data.draft, cfg = d.configuration, a = cfg.apis[0];
      const tenantHeaders = { 'X-RelayOps-Tenant': d.tenant_id };
      const save = async (extra = {}) => api('PUT', `/api/release-drafts/${d.id}`, {
        version: d.version, configuration: cfg, suite_ids: d.suite_ids, environment_id: d.environment_id || null, ...extra
      }, tenantHeaders);
      $('#view').innerHTML = `<div class="journey-heading"><div><span class="eyebrow">SAFE API DELIVERY</span><h2>${esc(a.name)}</h2><p>Prepare a change, prove it, then release with confidence.</p></div><a class="button ghost" href="#/release">All release drafts</a></div>
        <div class="journey-context"><span>Tenant <b>${esc(tenants.find(t => t.id === d.tenant_id)?.name || d.tenant_id)}</b></span><span>Environment <b>${esc(d.environment_id ? 'Selected test environment' : 'Not selected')}</b></span><span>Baseline <b>${d.baseline_revision || 'Not reviewed'}</b></span><span>Candidate <b>${d.candidate_revision || 'Not deployed'}</b></span><span>Draft <b>v${d.version}</b></span></div>
        <div class="journey-grid"><nav class="journey-steps" aria-label="Release steps"><ol>${labels.map((label, i) => `<li><a href="#/release/${d.id}?step=${i}" ${i === step ? 'aria-current="step"' : ''}><span class="journey-number">${i + 1}</span><span><b>${label}</b><small class="journey-state ${esc(data.steps[i].state)}">${stateLabel(data.steps[i].state)}</small></span></a></li>`).join('')}</ol></nav>
        <section class="journey-content" aria-labelledby="journey-title"><div class="journey-stage-heading"><span class="eyebrow">STEP ${step + 1} OF 7</span><h2 id="journey-title" tabindex="-1">${labels[step]}</h2><p>${esc(data.steps[step].detail)}</p></div><div id="journey-error" class="form-error" role="alert" tabindex="-1" hidden></div><div id="journey-stage"></div></section>
        <aside class="journey-review"><span class="eyebrow">RELEASE SUMMARY</span><h3>Your next decision</h3><p>${esc(data.steps[step].detail)}</p><dl><dt>API route</dt><dd>${esc(a.base_path)}</dd><dt>Authentication</dt><dd>${esc(a.auth_type || 'none')}</dd><dt>Request limit</dt><dd>${a.rate_limit_per_minute || 'Unlimited'} / minute</dd><dt>Outcome</dt><dd>${esc(data.outcome)}</dd></dl><div class="journey-notice">Releases affect the shared fleet. Candidate test evidence is checked again when you promote.</div>${(data.blockers || []).length ? `<h4>Needs attention</h4><ul>${data.blockers.map(b => `<li>${esc(b)} <a class="text-link" href="${/fleet|node|acknowledge|converg|traffic|request|rollback/i.test(b) ? '#/fleet' : '#/release/' + d.id + '?step=2'}">Resolve blocker</a></li>`).join('')}</ul>` : ''}<a class="text-link" href="#/fleet">Advanced fleet view</a></aside></div>`;
      const stage = $('#journey-stage');
      const next = (title, body, action, disabled = false) => { stage.innerHTML = body + `<div class="journey-actions">${step ? `<a class="button ghost" href="#/release/${d.id}?step=${step - 1}">Back</a>` : ''}<button class="button primary" id="journey-next" ${disabled ? 'disabled' : ''}>${title}</button></div>`; $('#journey-next').onclick = e => run(e.currentTarget, action); };
      if (step === 0) {
        next('Configure protection', `<div class="journey-notice success">Saved separately from live configuration.</div><dl class="journey-facts"><dt>API</dt><dd>${esc(a.name)}</dd><dt>Upstream</dt><dd>${esc(a.upstream_url)}</dd><dt>Published paths</dt><dd>${Object.keys(a.openapi_spec?.paths || {}).length}</dd></dl><details><summary>Review imported specification</summary><pre>${esc(JSON.stringify(a.openapi_spec || {}, null, 2))}</pre></details>`, () => go(d.id, 1));
      } else if (step === 1) {
        next('Save protection and continue', `<form id="protection-form" class="journey-form"><label>API name<input id="j-name" value="${esc(a.name)}" required></label><label>Gateway base path<input id="j-path" value="${esc(a.base_path)}" required></label><label>Upstream URL<input id="j-upstream" type="url" value="${esc(a.upstream_url)}" required></label><label>Authentication<select id="j-auth">${['api_key', 'oidc', 'jwt', 'none'].map(v => `<option ${a.auth_type === v ? 'selected' : ''} value="${v}">${({ api_key: 'API key', oidc: 'OIDC', jwt: 'JWT', none: 'Public — no authentication' })[v]}</option>`).join('')}</select></label><div class="journey-pair"><label>Requests per minute<input id="j-limit" type="number" min="0" value="${a.rate_limit_per_minute || 60}"></label><label>Daily quota<input id="j-quota" type="number" min="0" value="${a.quota_per_day || 5000}"></label></div><label>JWT secret reference<input id="j-secret" value="${esc(a.jwt_secret || '')}" placeholder="\${secret:API_JWT_SECRET}" autocomplete="off"></label><label>JWKS URL<input id="j-jwks" value="${esc(a.jwks_url || '')}" placeholder="https://identity.example/.well-known/jwks.json"></label><label>OIDC issuer<input id="j-issuer" value="${esc(a.oidc_issuer || '')}"></label><label>OIDC audience<input id="j-audience" value="${esc(a.oidc_audience || '')}"></label><label class="journey-check"><input type="checkbox" id="j-approval" ${a.require_approval ? 'checked' : ''}>Require subscription approval</label><p class="muted">Use secret references only. A public API is intentionally accessible without credentials.</p></form>`, async () => {
          if (!$('#protection-form').reportValidity()) return;
          Object.assign(a, { name: $('#j-name').value.trim(), base_path: $('#j-path').value.trim(), upstream_url: $('#j-upstream').value.trim(), auth_type: $('#j-auth').value, rate_limit_per_minute: Number($('#j-limit').value), quota_per_day: Number($('#j-quota').value), require_approval: $('#j-approval').checked, jwt_secret: $('#j-secret').value.trim(), jwks_url: $('#j-jwks').value.trim(), oidc_issuer: $('#j-issuer').value.trim(), oidc_audience: $('#j-audience').value.trim(), is_draft: false, enabled: true });
          await save(); go(d.id, 2);
        }, !!d.candidate_revision);
      } else if (step === 2) {
        const [suites, envs] = await Promise.all([api('GET', '/api/tests/suites?tenant=' + d.tenant_id), api('GET', '/api/tests/environments?tenant=' + d.tenant_id)]);
        if (render !== renderVersion) return;
        const allowedSuites = suites.filter(s => s.tenant_id === d.tenant_id);
        next(d.candidate_revision ? 'Refresh candidate evidence' : 'Save test selection', `<p>Choose the assertions that must pass. For a new API, baseline calls may return 404; candidate evidence remains mandatory.</p><label class="journey-field">Test environment<select id="j-env"><option value="">Choose an environment</option>${envs.filter(e => e.tenant_id === d.tenant_id).map(e => `<option value="${e.id}" ${e.id === d.environment_id ? 'selected' : ''}>${esc(e.name)}</option>`).join('')}</select></label><fieldset class="journey-suite-list"><legend>Required test suites</legend>${allowedSuites.map(s => `<label class="journey-check"><input type="checkbox" name="j-suite" value="${s.id}" ${d.suite_ids.includes(s.id) ? 'checked' : ''} ${d.candidate_revision ? 'disabled' : ''}>${esc(s.name)}</label>`).join('') || '<p>No suites yet. Create a smoke check below or use Test Studio.</p>'}</fieldset><div class="journey-actions"><button class="button" id="j-baseline" ${d.candidate_revision ? 'disabled' : ''}>Run baseline checks</button><button class="button" id="j-candidate" ${!d.candidate_revision ? 'disabled' : ''}>Run candidate checks</button></div><div id="j-run-result" role="status"></div><details><summary>Create a smoke check</summary><p>Choose a safe GET endpoint and expected status. This sends a real request only when you run the check.</p><label class="journey-field">Request path<input id="j-smoke-path" value="${esc(a.base_path + (Object.keys(a.openapi_spec?.paths || {}).find(p => a.openapi_spec.paths[p].get && !p.includes('{')) || '/'))}"></label><label class="journey-field">Expected status<input id="j-smoke-status" type="number" min="200" max="599" value="200"></label><button class="button" id="j-create-suite" ${d.candidate_revision ? 'disabled' : ''}>Create smoke suite</button></details><details><summary>Create a test environment</summary><label class="journey-field">Environment name<input id="j-env-name" placeholder="Release verification"></label><p>Uses the configured gateway. Configure credential bindings in Test Studio when your assertions need consumer credentials.</p><button class="button" id="j-create-env">Create environment</button></details><a class="text-link" href="#/tests">Advanced Test Studio</a>`, async () => {
          if (!d.candidate_revision) {
            const ids = [...document.querySelectorAll('[name="j-suite"]:checked')].map(e => e.value);
            if (!ids.length || !$('#j-env').value) throw new Error('Select at least one suite and a test environment.');
            await save({ suite_ids: ids, environment_id: $('#j-env').value }); go(d.id, 3);
          } else { go(d.id, 5); }
        });
        const queueRuns = async candidate => {
          const ids = d.candidate_revision ? d.suite_ids : [...document.querySelectorAll('[name="j-suite"]:checked')].map(e => e.value);
          if (!ids.length || !$('#j-env').value) throw new Error('Select suites and an environment first.');
          const revision = candidate ? d.candidate_revision : (await api('GET', '/api/fleet/status')).target_revision;
          for (const suite_id of ids) await api('POST', '/api/tests/runs', { suite_id, environment_id: $('#j-env').value, execution_mode: 'standard', target_revision: revision });
          $('#j-run-result').textContent = 'Checks queued. Refresh evidence after execution completes.';
        };
        $('#j-baseline').onclick = e => run(e.currentTarget, () => queueRuns(false));
        $('#j-candidate').onclick = e => run(e.currentTarget, () => queueRuns(true));
        $('#j-create-suite').onclick = e => run(e.currentTarget, async () => {
          const path = $('#j-smoke-path').value.trim();
          if (!path.startsWith('/') || path.includes('{')) throw new Error('Enter a concrete gateway path.');
          const name = a.name + ' release check ' + Date.now();
          const result = await api('POST', '/api/tests/suites', { tenant_id: d.tenant_id, name, definition: { name, requests: [{ id: 'smoke', name: 'Expected response', method: 'GET', path, assertions: [{ type: 'status_code', expected: $('#j-smoke-status').value }] }] } }, tenantHeaders);
          await save({ suite_ids: [...d.suite_ids, result.suite.id] }); await views.release(d.id + '?step=2');
        });
        $('#j-create-env').onclick = e => run(e.currentTarget, async () => {
          const env = await api('POST', '/api/tests/environments', { tenant_id: d.tenant_id, name: $('#j-env-name').value.trim(), gateway_target: '', variables: {}, credential_bindings: {} }, tenantHeaders);
          await save({ environment_id: env.id }); await views.release(d.id + '?step=2');
        });
      } else if (step === 3) {
        const reviewed = await api('POST', `/api/release-drafts/${d.id}/plan`, {});
        if (render !== renderVersion) return;
        next('I reviewed this change — continue', `<div class="journey-notice">This change creates a fleet-wide revision. Existing callers remain on the baseline until selected for the canary.</div><div class="journey-pair"><div class="journey-metric"><span>Creates</span><b>${reviewed.plan.summary.create}</b></div><div class="journey-metric"><span>Updates</span><b>${reviewed.plan.summary.update}</b></div></div>${reviewed.plan.changes.filter(c => c.action !== 'unchanged').map(c => `<article class="journey-change"><h3>${esc(c.name)} <span class="tag">${esc(c.action)}</span></h3>${(c.changes || []).map(f => `<div><b>${esc(f.field)}</b><pre>${esc(JSON.stringify(f.before))} → ${esc(JSON.stringify(f.after))}</pre></div>`).join('')}${c.consumer_impact ? `<details><summary>Consumer impact</summary><pre>${esc(JSON.stringify(c.consumer_impact, null, 2))}</pre></details>` : ''}</article>`).join('')}<p>Warnings</p><ul>${(reviewed.plan.warnings || []).map(v => `<li>${esc(v)}</li>`).join('') || '<li>No plan warnings reported.</li>'}</ul><label class="journey-check"><input id="j-review" type="checkbox">I reviewed the changes, affected consumers, and shared-fleet scope.</label>`, () => { if (!$('#j-review').checked) throw new Error('Confirm that you reviewed the change.'); go(d.id, 4); }, !reviewed.plan.has_changes);
      } else if (step === 4) {
        next(d.candidate_revision ? 'Test the candidate' : 'Launch canary', `<div class="journey-notice">A canary sends a limited share of real callers to this revision. Launching it is an explicit production action.</div>${d.candidate_revision ? `<p>Candidate revision <b>${d.candidate_revision}</b> is deployed.</p>` : `<label class="journey-field">Caller exposure<select id="j-exposure"><option value="10">10% — recommended starting point</option value="1">1%</option><option value="5">5%</option><option value="25">25%</option></select></label><label class="journey-check"><input id="j-launch-confirm" type="checkbox">I authorize this candidate to receive live traffic.</label>`}`, async () => {
          if (!d.candidate_revision) {
            if (!$('#j-launch-confirm').checked) throw new Error('Confirm live traffic exposure first.');
            await api('POST', `/api/release-drafts/${d.id}/canary`, { version: d.version, plan_hash: d.plan_hash, traffic_percent: Number($('#j-exposure').value) });
          }
          go(d.id, 2);
        });
      } else if (step === 5) {
        let status = null;
        if (d.candidate_revision) status = await api('GET', `/api/revisions/${d.candidate_revision}/canary-status`);
        const candidate = status?.comparison?.canary, baseline = status?.comparison?.baseline;
        next('Promote to the full fleet', `<div class="journey-pair"><div class="journey-metric"><span>Candidate requests</span><b>${candidate?.requests ?? 'Unavailable'}</b><small>${candidate?.error_rate_pct ?? '—'}% server errors</small></div><div class="journey-metric"><span>Baseline requests</span><b>${baseline?.requests ?? 'Unavailable'}</b><small>${baseline?.error_rate_pct ?? '—'}% server errors</small></div></div><h3>Required evidence</h3>${(data.gate_status?.suites || []).map(s => `<div class="journey-evidence"><span class="tag ${s.eligible ? 'success' : 'warn'}">${s.eligible ? 'Passed' : 'Blocked'}</span><span>${esc(s.suite_id)}<small>${esc(s.reason_code || 'Fresh passing evidence')} ${s.expires_at ? '· Expires ' + esc(new Date(s.expires_at).toLocaleString()) : ''}</small></span></div>`).join('') || '<p>Evidence is not available.</p>'}<div class="journey-actions"><button id="j-refresh" class="button">Refresh review</button><a class="button" href="#/release/${d.id}?step=2">Run required checks</a></div><label class="journey-check"><input id="j-final-review" type="checkbox">I reviewed the latest evidence, health, and consumer impact. Promote this candidate to all callers.</label><p class="muted">Promotion rechecks evidence on the server. Gateway convergence follows asynchronously.</p><button class="button danger" id="j-abort" ${!data.can_abort ? 'disabled' : ''}>Abort canary</button>`, async () => {
          if (!$('#j-final-review').checked) throw new Error('Complete the final release review.');
          await api('POST', `/api/release-drafts/${d.id}/decide/promote`, { reviewed: true }); go(d.id, 6);
        }, !data.can_promote);
        $('#j-refresh').onclick = () => views.release(d.id + '?step=5');
        $('#j-abort').onclick = e => run(e.currentTarget, async () => {
          if (!confirm('Abort this candidate and restore baseline configuration for the fleet?')) return;
          await api('POST', `/api/release-drafts/${d.id}/decide/abort`, { reviewed: true }); go(d.id, 6);
        });
      } else {
        next('Download evidence JSON', `<div class="journey-notice ${data.outcome === 'active' ? 'success' : ''}"><strong>Release outcome: ${esc(data.outcome)}</strong><p>Candidate ${d.candidate_revision || 'not deployed'} · ${data.fleet?.converged ? 'Fleet converged' : 'Fleet convergence pending or unavailable'}</p></div><p>The export contains release identity, the recorded outcome, and the evidence sealed during promotion. It excludes API credentials and request payloads.</p><button class="button" id="j-print">Print release summary</button>`, async () => download('relayops-release-' + d.id + '.json', await api('GET', `/api/release-drafts/${d.id}/evidence`)), !d.candidate_revision);
        $('#j-print').onclick = () => window.print();
      }
      if (render !== renderVersion) return;
      $('#journey-title').focus({ preventScroll: true });
      revealCurrentStep();
    };
    async function start(id, render) {
      if (id?.startsWith('api-')) {
        const d = await api('POST', '/api/release-drafts', { api_id: id.slice(4) }); go(d.id, 1); return;
      }
      const [drafts, tenants] = await Promise.all([api('GET', '/api/release-drafts'), api('GET', '/api/tenants')]);
      if (render !== renderVersion) return;
      $('#view').innerHTML = `<div class="journey-heading"><div><span class="eyebrow">RELEASE WORKSPACE</span><h2>One clear path to a safe release</h2><p>Import → protect → test → review → canary → decide → evidence.</p></div></div><div class="journey-start-grid"><section class="card"><h3>Start a new API release</h3><p>Save your OpenAPI document as a separate draft. Importing here does not change live traffic.</p><div id="journey-error" class="form-error" role="alert" tabindex="-1" hidden></div><label class="journey-field">Workspace<select id="j-tenant">${tenants.map(t => `<option value="${t.id}">${esc(t.name || t.slug)}</option>`).join('')}</select></label><label class="journey-field">Upload OpenAPI<input id="j-file" type="file" accept=".json,.yaml,.yml"></label><label class="journey-field">Or paste OpenAPI JSON / YAML<textarea id="j-spec" rows="10" placeholder="openapi: 3.0.3"></textarea></label><button class="button primary" id="j-import">Save release draft</button></section><section class="card"><h3>Continue a saved release</h3>${drafts.map(d => `<a class="journey-draft" href="#/release/${d.id}?step=${d.candidate_revision ? 5 : 1}"><span><b>${esc(d.configuration.apis?.[0]?.name || 'API draft')}</b><small>Draft v${d.version} · ${d.candidate_revision ? 'Candidate ' + d.candidate_revision : 'Not deployed'}</small></span><span aria-hidden="true">→</span></a>`).join('') || '<div class="empty-state">Your saved releases will appear here.</div>'}</section></div>`;
      $('#j-file').onchange = async e => { if (e.target.files[0]) $('#j-spec').value = await e.target.files[0].text(); };
      $('#j-import').onclick = e => run(e.currentTarget, async () => {
        if (!$('#j-spec').value.trim()) throw new Error('Upload or paste an OpenAPI document.');
        const d = await api('POST', '/api/release-drafts', { spec: $('#j-spec').value }, { 'X-RelayOps-Tenant': $('#j-tenant').value }); go(d.id, 0);
      });
    }
  };
})();
