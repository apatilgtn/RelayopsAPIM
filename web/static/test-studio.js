/* RelayOps API Test Studio — Visual Suite Builder, In-Process Runner, and Comparison Diff Viewer
 * Follows RelayOps design tokens, Developer Hub aesthetics, and release safety workflows.
 */
(() => {
  'use strict';

  window.initTestStudio = function(ctx) {
    const { views, api, toast, esc, openModal, statusPill, methodBadge, fmtMs, fmtDate, fmtNum, $ } = ctx;

    let currentTab = 'suites';
    let pollInterval = null;

    function iconHtml(name) {
      if (window.RelayUI && window.RelayUI.icon) {
        return window.RelayUI.icon(name) || '';
      }
      return '';
    }

    views.tests = async (param, soft = false) => {
      clearInterval(pollInterval);

      if (param && param.startsWith('run-')) {
        await renderRunDetail(param);
        return;
      }

      const container = $('#view');
      container.innerHTML = `
        <div class="card" style="padding:48px; text-align:center">
          <p class="muted">Loading Test Studio resources…</p>
        </div>
      `;

      let suites = [], envs = [], runs = [], apis = [];
      let loadError = null;

      try {
        const [sRes, eRes, rRes, aRes] = await Promise.all([
          api('GET', '/api/tests/suites'),
          api('GET', '/api/tests/environments'),
          api('GET', '/api/tests/runs?limit=30'),
          api('GET', '/api/apis')
        ]);
        suites = sRes || [];
        envs = eRes || [];
        runs = rRes || [];
        apis = aRes || [];
      } catch (err) {
        loadError = err;
      }

      if (loadError) {
        const isForbidden = loadError.status === 403 || String(loadError.message).toLowerCase().includes('denied') || String(loadError.message).toLowerCase().includes('forbidden');
        container.innerHTML = `
          <div class="card" style="padding:48px; text-align:center; border-left:4px solid var(--red)">
            <h3 style="color:var(--red); margin-bottom:8px">${isForbidden ? 'Access Denied' : 'Service Unavailable'}</h3>
            <p class="muted" style="max-width:540px; margin:0 auto 20px">
              ${isForbidden ? 'You do not have the required permissions to view Test Studio resources for this tenant.' : 'Failed to connect to the RelayOps control plane: ' + esc(loadError.message)}
            </p>
            <button class="button small" id="btn-retry-tests">${iconHtml('refresh-cw')} Retry</button>
          </div>
        `;
        container.querySelector('#btn-retry-tests').onclick = () => views.tests();
        return;
      }

      container.innerHTML = `
        <div class="toolbar">
          <div class="muted">Design repeatable test suites, verify gateway revision decisions, and validate candidate releases against baselines.</div>
          <div class="spacer"></div>
          <button class="button small ghost" id="btn-import-suite">${iconHtml('download')} Import spec</button>
          <button class="button small ghost" id="btn-gate-policies">${iconHtml('shield-check')} Promotion gates</button>
          <button class="button small ghost" id="btn-manage-envs">${iconHtml('settings')} Environments</button>
          <button class="button primary" id="btn-create-suite">New test suite</button>
        </div>

        <div class="tabs" role="tablist" style="margin-top:14px; margin-bottom:18px; display:flex; gap:12px; border-bottom:1px solid var(--border)">
          <button role="tab" class="button ghost small tab-btn ${currentTab === 'suites' ? 'active' : ''}" data-tab="suites">Test suites (${suites.length})</button>
          <button role="tab" class="button ghost small tab-btn ${currentTab === 'runs' ? 'active' : ''}" data-tab="runs">Run history (${runs.length})</button>
          <button role="tab" class="button ghost small tab-btn ${currentTab === 'envs' ? 'active' : ''}" data-tab="envs">Environments (${envs.length})</button>
        </div>

        <div id="test-tab-content"></div>
      `;

      if (window.RelayUI?.replaceIcons) window.RelayUI.replaceIcons(container);

      $('#btn-create-suite').onclick = () => openSuiteModal(null, apis);
      $('#btn-import-suite').onclick = () => openImportModal(apis);
      $('#btn-gate-policies').onclick = () => openGatePoliciesModal(apis, suites);
      $('#btn-manage-envs').onclick = () => {
        currentTab = 'envs';
        container.querySelectorAll('.tab-btn').forEach(b => {
          const on = b.dataset.tab === 'envs';
          b.classList.toggle('active', on);
          b.setAttribute('aria-selected', String(on));
          b.tabIndex = on ? 0 : -1;
        });
        switchTab();
      };

      const studioTabs = [...container.querySelectorAll('.tab-btn')];
      studioTabs.forEach((btn, index) => {
        btn.setAttribute('aria-selected', String(btn.classList.contains('active')));
        btn.tabIndex = btn.classList.contains('active') ? 0 : -1;
        btn.onkeydown = e => {
          let next;
          if (e.key === 'ArrowRight') next = (index + 1) % studioTabs.length;
          if (e.key === 'ArrowLeft') next = (index + studioTabs.length - 1) % studioTabs.length;
          if (e.key === 'Home') next = 0;
          if (e.key === 'End') next = studioTabs.length - 1;
          if (next !== undefined) { e.preventDefault(); studioTabs[next].click(); studioTabs[next].focus(); }
        };
      });
      container.querySelectorAll('.tab-btn').forEach(btn => {
        btn.onclick = () => {
          currentTab = btn.dataset.tab;
          container.querySelectorAll('.tab-btn').forEach(b => { b.classList.toggle('active', b === btn); b.setAttribute('aria-selected', String(b === btn)); b.tabIndex = b === btn ? 0 : -1; });
          switchTab();
        };
      });

      function switchTab() {
        const target = $('#test-tab-content');
        if (currentTab === 'suites') {
          renderSuitesTable(target, suites, envs, apis);
        } else if (currentTab === 'runs') {
          renderRunsTable(target, runs);
        } else {
          renderEnvsTable(target, envs);
        }
        if (window.RelayUI?.replaceIcons) window.RelayUI.replaceIcons(target);
      }

      switchTab();
    };

    function renderSuitesTable(root, suites, envs, apis) {
      if (suites.length === 0) {
        root.innerHTML = `
          <div class="card" style="padding:48px; text-align:center">
            <h3 style="margin-bottom:8px">No test suites yet</h3>
            <p class="muted" style="max-width:540px; margin:0 auto 20px">Create repeatable verification suites to assert HTTP responses, validate gateway policy routing, and gate production deployments.</p>
            <button class="button primary" id="btn-empty-create">Create your first suite</button>
          </div>
        `;
        root.querySelector('#btn-empty-create').onclick = () => openSuiteModal(null, apis);
        return;
      }

      root.innerHTML = `
        <div class="card">
          <div class="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Suite Name</th>
                  <th>API Target</th>
                  <th>Ownership</th>
                  <th>Version</th>
                  <th>Created</th>
                  <th style="text-align:right">Actions</th>
                </tr>
              </thead>
              <tbody>
                ${suites.map(s => {
                  const apiObj = apis.find(a => a.id === s.api_id);
                  const apiLabel = apiObj ? `${apiObj.name} (${apiObj.base_path})` : 'Global / All APIs';
                  return `
                    <tr>
                      <td>
                        <strong>${esc(s.name)}</strong>
                        ${s.description ? `<br><small class="muted">${esc(s.description)}</small>` : ''}
                      </td>
                      <td><span class="tag ${s.api_id ? '' : 'gray'}">${esc(apiLabel)}</span></td>
                      <td><span class="tag gray">${esc(s.ownership || 'team')}</span></td>
                      <td><span class="tag">v${s.current_version || 1}</span></td>
                      <td><small class="muted">${fmtDate(s.created_at)}</small></td>
                      <td style="text-align:right">
                        <div style="display:inline-flex; gap:6px; justify-content:flex-end">
                          <button class="button small" data-run="${esc(s.id)}">${iconHtml('rocket')} Run</button>
                          <button class="button small ghost" data-compare="${esc(s.id)}">${iconHtml('git-compare-arrows')} Compare</button>
                          <button class="button small ghost" data-edit="${esc(s.id)}">Edit</button>
                          <button class="button small ghost danger" data-delete="${esc(s.id)}">Delete</button>
                        </div>
                      </td>
                    </tr>
                  `;
                }).join('')}
              </tbody>
            </table>
          </div>
        </div>
      `;

      root.querySelectorAll('[data-run]').forEach(btn => {
        btn.onclick = () => openRunModal(btn.dataset.run, envs, 'standard');
      });
      root.querySelectorAll('[data-compare]').forEach(btn => {
        btn.onclick = () => openRunModal(btn.dataset.compare, envs, 'comparison');
      });
      root.querySelectorAll('[data-edit]').forEach(btn => {
        btn.onclick = async () => {
          try {
            const detail = await api('GET', `/api/tests/suites/${btn.dataset.edit}`);
            openSuiteModal(detail, apis);
          } catch (err) {
            toast('Failed to load suite details: ' + err.message, 'error');
          }
        };
      });
      root.querySelectorAll('[data-delete]').forEach(btn => {
        btn.onclick = async () => {
          if (confirm('Delete this test suite and all historical runs?')) {
            try {
              await api('DELETE', `/api/tests/suites/${btn.dataset.delete}`);
              toast('Test suite deleted', 'success');
              views.tests();
            } catch (err) {
              toast('Delete failed: ' + err.message, 'error');
            }
          }
        };
      });
    }

    function renderRunsTable(root, runs) {
      if (runs.length === 0) {
        root.innerHTML = `<div class="card" style="padding:32px; text-align:center"><p class="muted">No test execution history found.</p></div>`;
        return;
      }

      root.innerHTML = `
        <div class="card">
          <div class="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Run ID</th>
                  <th>Suite</th>
                  <th>Mode</th>
                  <th>State</th>
                  <th>Steps Passed</th>
                  <th>Revision</th>
                  <th>Triggered</th>
                  <th style="text-align:right">Details</th>
                </tr>
              </thead>
              <tbody>
                ${runs.map(r => {
                  const stateClass = r.lifecycle_state === 'completed' && r.failed_steps === 0 ? 'success' :
                                     r.lifecycle_state === 'completed' && r.failed_steps > 0 ? 'error' :
                                     r.lifecycle_state === 'running' ? 'blue' :
                                     r.lifecycle_state === 'queued' ? 'warn' : 'gray';
                  return `
                    <tr>
                      <td class="mono"><strong><a href="#/tests/run-${esc(r.id)}">${esc(r.id.substring(0, 14))}…</a></strong></td>
                      <td>${esc(r.suite_id)}</td>
                      <td><span class="tag ${r.mode === 'comparison' ? 'warn' : 'gray'}">${esc(r.mode)}</span></td>
                      <td><span class="tag ${stateClass}">${esc(r.lifecycle_state)}</span></td>
                      <td>
                        <span class="tag success" style="display:inline-flex; align-items:center; gap:4px">
                          ${iconHtml('check')} ${r.passed_steps}
                        </span>
                        ${r.failed_steps > 0 ? `
                          <span class="tag error" style="display:inline-flex; align-items:center; gap:4px; margin-left:4px">
                            ${iconHtml('x')} ${r.failed_steps}
                          </span>
                        ` : ''}
                        ${r.skipped_steps > 0 ? `<span class="muted" style="margin-left:6px">- ${r.skipped_steps}</span>` : ''}
                        <small class="muted">/ ${r.total_steps}</small>
                      </td>
                      <td>${r.actual_revision ? `<span class="tag">rev_${r.actual_revision}</span>` : '<span class="muted">—</span>'}</td>
                      <td><small class="muted">${fmtDate(r.created_at)} by ${esc(r.actor)}</small></td>
                      <td style="text-align:right">
                        <a class="button small ghost" href="#/tests/run-${esc(r.id)}">Inspect ${iconHtml('arrow-right')}</a>
                      </td>
                    </tr>
                  `;
                }).join('')}
              </tbody>
            </table>
          </div>
        </div>
      `;
    }

    function renderEnvsTable(root, envs) {
      root.innerHTML = `
        <div class="card">
          <div class="card-header" style="display:flex; justify-content:space-between; align-items:center">
            <h3>Test Environments</h3>
            <button class="button small primary" id="btn-add-env">${iconHtml('check')} Add Environment</button>
          </div>
          <div class="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Environment Name</th>
                  <th>Gateway Target URL</th>
                  <th>Variables Defined</th>
                  <th>Updated</th>
                  <th style="text-align:right">Actions</th>
                </tr>
              </thead>
              <tbody>
                ${envs.length === 0 ? `<tr><td colspan="5" style="text-align:center; padding:24px" class="muted">No custom environments configured. Test runs default to loopback gateway target.</td></tr>` :
                  envs.map(e => `
                    <tr>
                      <td><strong>${esc(e.name)}</strong></td>
                      <td><code class="mono">${esc(e.gateway_target || 'http://127.0.0.1:8080')}</code></td>
                      <td><span class="tag">${Object.keys(e.variables || {}).length} variables</span></td>
                      <td><small class="muted">${fmtDate(e.updated_at || e.created_at)}</small></td>
                      <td style="text-align:right">
                        <button class="button small ghost" data-edit-env="${esc(e.id)}">Edit</button>
                        <button class="button small ghost danger" data-delete-env="${esc(e.id)}">Delete</button>
                      </td>
                    </tr>
                  `).join('')}
              </tbody>
            </table>
          </div>
        </div>
      `;

      root.querySelector('#btn-add-env').onclick = () => openEnvModal(null);
      root.querySelectorAll('[data-edit-env]').forEach(b => {
        b.onclick = () => openEnvModal(envs.find(e => e.id === b.dataset.editEnv));
      });
      root.querySelectorAll('[data-delete-env]').forEach(b => {
        b.onclick = async () => {
          if (confirm('Delete this test environment?')) {
            try {
              await api('DELETE', `/api/tests/environments/${b.dataset.deleteEnv}`);
              toast('Environment deleted', 'success');
              views.tests();
            } catch (err) {
              toast('Delete failed: ' + err.message, 'error');
            }
          }
        };
      });
    }

    async function renderRunDetail(runIdWithPrefix) {
      const runId = runIdWithPrefix.replace(/^run-/, '');
      const container = $('#view');

      async function fetchAndRender() {
        let detail;
        try { detail = await api('GET', `/api/tests/runs/${runId}`); }
        catch (err) {
          container.innerHTML = `<div class="card" role="alert"><h3>Unable to load test run</h3><p>${esc(err.message)}</p><button class="button" id="retry-run">Retry</button></div>`;
          container.querySelector('#retry-run').onclick = fetchAndRender;
          return;
        }
        if (!detail || !detail.run) {
          container.innerHTML = `<div class="card"><div class="form-error">Test run not found</div></div>`;
          return;
        }

        const run = detail.run;
        const steps = detail.steps || [];
        const isTerminal = ['completed', 'failed', 'cancelled', 'interrupted'].includes(run.lifecycle_state);

        let comparisonView = '';
        if (run.mode === 'comparison' && run.comparison_summary) {
          const comp = run.comparison_summary;
          const diffCount = comp.behavioral_diffs || 0;
          const allMatched = diffCount === 0 && run.lifecycle_state === 'completed' && run.failed_steps === 0 && run.total_steps > 0 && comp.steps_compared === run.total_steps;
          comparisonView = `
            <div class="card" style="margin-top:16px; border-left:4px solid ${allMatched ? 'var(--green)' : 'var(--amber)'}">
              <div class="card-header" style="display:flex; justify-content:space-between; align-items:center">
                <h3>Baseline vs Candidate Comparison Summary</h3>
                <span class="tag ${allMatched ? 'success' : 'warn'}">
                  ${allMatched ? 'Compared responses matched' : diffCount > 0 ? `${diffCount} response differences detected` : 'Comparison incomplete or failed'}
                </span>
              </div>
              <div class="grid two" style="gap:16px; margin-top:12px">
                <div class="card kpi">
                  <div class="label">Baseline Revision</div>
                  <div class="value mono">rev_${run.actual_revision || comp.baseline_revision || '—'}</div>
                  <div class="sub">Verified via X-RelayOps-Revision</div>
                </div>
                <div class="card kpi">
                  <div class="label">Candidate Revision</div>
                  <div class="value mono" style="color:var(--amber)">rev_${run.actual_candidate_revision || comp.candidate_revision || '—'}</div>
                  <div class="sub">Pinned via X-RelayOps-Target-Revision</div>
                </div>
              </div>
              ${comp.comparisons && comp.comparisons.length > 0 ? `
                <div style="margin-top:16px">
                  <h4 style="margin-bottom:8px">Step Comparisons:</h4>
                  <div class="table-wrap">
                    <table>
                      <thead>
                        <tr>
                          <th>Step</th>
                          <th>Request</th>
                          <th>Baseline Status</th>
                          <th>Candidate Status</th>
                          <th>Checked evidence</th>
                          <th>Differences</th>
                        </tr>
                      </thead>
                      <tbody>
                        ${comp.comparisons.map(c => `
                          <tr>
                            <td><strong>#${c.step_index}</strong></td>
                            <td>${esc(c.request_name)}</td>
                            <td>${statusPill(c.baseline_status)}</td>
                            <td>${statusPill(c.candidate_status)}</td>
                            <td><span class="tag ${c.behavioral_diff ? 'warn' : 'success'}">${c.behavioral_diff ? 'Diff Detected' : 'Checked fields match'}</span><div class="muted" style="font-size:12px">Headers: ${esc((c.compared_headers || []).join(', ') || 'not recorded')}<br>Latency: ${fmtMs(c.baseline_latency_ms)} → ${fmtMs(c.candidate_latency_ms)}; ${c.latency_gate_enabled ? (c.latency_regression ? 'regression detected' : 'gate enabled') : 'gate disabled'}</div></td>
                            <td>
                              ${(c.diffs || []).map(d => `<div style="font-size:12px" class="${d.ignored ? 'muted' : 'warn'}"><strong>${esc(d.field)}:</strong> base=<code>${esc(d.baseline)}</code> cand=<code>${esc(d.candidate)}</code> ${d.ignored ? '(ignored)' : ''}</div>`).join('') || '<span class="muted">—</span>'}
                            </td>
                          </tr>
                        `).join('')}
                      </tbody>
                    </table>
                  </div>
                </div>
              ` : '<p class="muted" style="margin-top:12px">Compare the recorded status codes and captured response bodies. Passing results apply only to the requests and assertions in this suite.</p>'}
            </div>
          `;
        }

        container.innerHTML = `
          <div class="toolbar">
            <button class="button small ghost" id="btn-back-tests">${iconHtml('arrow-right')} Back to Test Studio</button>
            <div class="spacer"></div>
            ${!isTerminal ? `<button class="button small danger" id="btn-cancel-run">Cancel Execution</button>` : ''}
            <button class="button small" id="btn-refresh-run">${iconHtml('refresh-cw')} Refresh</button>
          </div>

          <div class="card" style="margin-top:16px">
            <div class="card-header" style="display:flex; justify-content:space-between; align-items:center">
              <div>
                <h2>Test Run <span class="mono">${esc(run.id)}</span></h2>
                <span class="muted small">Suite: <strong>${esc(run.suite_id)}</strong> · Mode: <strong>${esc(run.mode)}</strong> · Triggered by <strong>${esc(run.actor)}</strong></span>
              </div>
              <div>
                <span class="tag ${run.lifecycle_state === 'completed' && run.failed_steps === 0 ? 'success' : run.lifecycle_state === 'completed' ? 'error' : run.lifecycle_state === 'running' ? 'blue' : 'warn'}">
                  ${esc(run.lifecycle_state)}
                </span>
              </div>
            </div>

            <div class="grid three" style="gap:16px; margin-top:16px">
              <div class="card kpi">
                <div class="label">Steps Passed</div>
                <div class="value" style="color:var(--green)">${run.passed_steps}</div>
                <div class="sub">${run.failed_steps} failed · ${run.skipped_steps} skipped</div>
              </div>
              <div class="card kpi">
                <div class="label">Target Gateway Revision</div>
                <div class="value mono">rev_${run.actual_revision || 'auto'}</div>
                <div class="sub">Verified via X-RelayOps-Revision</div>
              </div>
              <div class="card kpi">
                <div class="label">Execution Duration</div>
                <div class="value">${run.completed_at && run.started_at ? fmtMs((new Date(run.completed_at) - new Date(run.started_at))) : isTerminal ? '—' : 'In Progress…'}</div>
                <div class="sub">Executed by RelayOps runner</div>
              </div>
            </div>
            ${run.failure_reason ? `<div class="form-error" style="margin-top:16px"><strong>Execution Error:</strong> ${esc(run.failure_reason)}</div>` : ''}
          </div>

          ${comparisonView}

          <div class="card" style="margin-top:16px">
            <div class="card-header">
              <h3>Executed Steps &amp; Policy Trace</h3>
            </div>
            <div class="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Step</th>
                    <th>Request</th>
                    <th>Observed Status</th>
                    <th>Observed Revision</th>
                    <th>Latency</th>
                    <th>Decision Policy</th>
                    <th>Assertions</th>
                  </tr>
                </thead>
                <tbody>
                  ${steps.length === 0 ? `<tr><td colspan="7" style="text-align:center; padding:24px" class="muted">${isTerminal ? 'No step logs recorded.' : 'Executing requests…'}</td></tr>` :
                    steps.map(s => {
                      const allPassed = (s.assertion_results || []).every(a => a.passed);
                      return `
                        <tr>
                          <td><strong>#${s.step_index}</strong><br><span class="tag">${esc(s.cohort || 'standard')}</span></td>
                          <td>
                            ${methodBadge(s.method)} <code class="mono">${esc(s.url)}</code>
                            <br><small class="muted">${esc(s.request_name)}</small>
                          </td>
                          <td>${statusPill(s.status_code)}</td>
                          <td class="mono"><span class="tag">rev_${s.observed_revision}</span></td>
                          <td>${fmtMs(s.duration_ms)}</td>
                          <td>
                            <span class="tag gray">${esc(s.decision_policy || 'proxy')}</span>
                            ${s.decision_reason ? `<br><small class="muted">${esc(s.decision_reason)}</small>` : ''}
                          </td>
                          <td>
                            <span class="tag ${allPassed ? 'success' : 'error'}" style="display:inline-flex; align-items:center; gap:4px">
                              ${allPassed ? `${iconHtml('check')} Passed` : `${iconHtml('x')} Failed`}
                            </span>
                            ${s.assertion_results ? `
                              <div style="font-size:12px; margin-top:4px">
                                ${s.assertion_results.map(a => `<div style="color:${a.passed ? 'var(--green)' : 'var(--red)'}">${a.passed ? iconHtml('check') : iconHtml('x')} ${esc(a.type)}: ${esc(a.expected)} ${!a.passed ? `(got ${esc(a.actual || a.error)})` : ''}</div>`).join('')}
                              </div>
                            ` : ''}
                          </td>
                        </tr>
                      `;
                    }).join('')}
                </tbody>
              </table>
            </div>
          </div>
        `;

        if (window.RelayUI?.replaceIcons) window.RelayUI.replaceIcons(container);

        $('#btn-back-tests').onclick = () => { location.hash = '#/tests'; };
        $('#btn-refresh-run').onclick = () => fetchAndRender();
        const cancelBtn = $('#btn-cancel-run');
        if (cancelBtn) {
          cancelBtn.onclick = async () => {
            await api('POST', `/api/tests/runs/${run.id}/cancel`);
            toast('Cancellation requested', 'warn');
            fetchAndRender();
          };
        }

        if (!isTerminal) {
          pollInterval = setTimeout(fetchAndRender, 2000);
        }
      }

      await fetchAndRender();
    }

    // -------------------------------------------------------------------------
    // Suite Builder Modal (Tabbed Request Editor)
    // -------------------------------------------------------------------------
    function openSuiteModal(existing, apis) {
      const isEdit = !!existing;
      const suite = existing?.suite || {};
      const def = existing?.version?.definition || { requests: [], variables: [], ignore_paths: [] };

      let requests = def.requests || [];
      if (requests.length === 0) {
        requests = [{
          id: 'req-1',
          name: 'Step 1: Health Check',
          method: 'GET',
          path: '/healthz',
          headers: {},
          query_params: {},
          body: '',
          auth: { type: 'none' },
          assertions: [{ type: 'status_code', target: 'status_code', expected: '200' }],
          extracts: []
        }];
      }

      function renderReqTabs(r, idx) {
        const queryParams = Object.entries(r.query_params || {});
        const headers = Object.entries(r.headers || {});
        const auth = r.auth || { type: 'none' };
        const assertions = r.assertions || [{ type: 'status_code', target: 'status_code', expected: '200' }];
        const extracts = r.extracts || [];

        return `
          <div class="card req-item" data-idx="${idx}" style="padding:16px; background:var(--surface); border:1px solid var(--border); margin-bottom:14px">
            <div style="display:flex; gap:10px; align-items:center; margin-bottom:12px">
              <span class="mono" style="font-weight:700; color:var(--muted)">#${idx + 1}</span>
              <input type="text" class="req-name" value="${esc(r.name || 'Step ' + (idx + 1))}" placeholder="Request Name" style="flex:1" />
              <button type="button" class="button ghost small danger btn-del-req" title="Remove request step">${iconHtml('x')} Delete</button>
            </div>

            <div style="display:flex; gap:10px; margin-bottom:12px">
              <select class="req-method" style="width:115px">
                ${['GET','POST','PUT','DELETE','PATCH','HEAD','OPTIONS'].map(m => `<option value="${m}" ${r.method === m ? 'selected' : ''}>${m}</option>`).join('')}
              </select>
              <input type="text" class="req-path" value="${esc(r.path || '/')}" placeholder="/v1/endpoint (relative to gateway base URL)" style="flex:1" />
            </div>

            <!-- Sub-tabs navigation -->
            <div class="req-sub-tabs" style="display:flex; gap:8px; border-bottom:1px solid var(--border); margin-bottom:12px; padding-bottom:6px">
              <button type="button" class="button ghost small active sub-tab-btn" data-subtab="params">Params (${queryParams.length})</button>
              <button type="button" class="button ghost small sub-tab-btn" data-subtab="headers">Headers (${headers.length})</button>
              <button type="button" class="button ghost small sub-tab-btn" data-subtab="auth">Auth (${auth.type || 'none'})</button>
              <button type="button" class="button ghost small sub-tab-btn" data-subtab="body">Body</button>
              <button type="button" class="button ghost small sub-tab-btn" data-subtab="assertions">Assertions (${assertions.length})</button>
              <button type="button" class="button ghost small sub-tab-btn" data-subtab="extracts">Extracts (${extracts.length})</button>
            </div>

            <!-- Panel: Params -->
            <div class="sub-panel sub-panel-params">
              <div class="kv-params-list" style="display:flex; flex-direction:column; gap:6px; margin-bottom:8px">
                ${queryParams.map(([k, v]) => `
                  <div class="kv-row" style="display:flex; gap:8px">
                    <input type="text" class="kv-key" placeholder="Parameter Name" value="${esc(k)}" style="flex:1" />
                    <input type="text" class="kv-val" placeholder="Value (e.g. {{USER_ID}})" value="${esc(v)}" style="flex:2" />
                    <button type="button" class="button ghost small danger btn-del-kv">${iconHtml('x')}</button>
                  </div>
                `).join('')}
              </div>
              <button type="button" class="button ghost small btn-add-param">+ Add Query Parameter</button>
            </div>

            <!-- Panel: Headers -->
            <div class="sub-panel sub-panel-headers" style="display:none">
              <div class="kv-headers-list" style="display:flex; flex-direction:column; gap:6px; margin-bottom:8px">
                ${headers.map(([k, v]) => `
                  <div class="kv-row" style="display:flex; gap:8px">
                    <input type="text" class="kv-key" placeholder="Header Name (e.g. Accept)" value="${esc(k)}" style="flex:1" />
                    <input type="text" class="kv-val" placeholder="Header Value" value="${esc(v)}" style="flex:2" />
                    <button type="button" class="button ghost small danger btn-del-kv">${iconHtml('x')}</button>
                  </div>
                `).join('')}
              </div>
              <button type="button" class="button ghost small btn-add-header">+ Add Request Header</button>
            </div>

            <!-- Panel: Auth -->
            <div class="sub-panel sub-panel-auth" style="display:none">
              <div class="grid two" style="gap:10px">
                <div class="field" style="margin:0">
                  <label style="font-size:12px">Authentication Type</label>
                  <select class="req-auth-type">
                    <option value="none" ${auth.type === 'none' ? 'selected' : ''}>None</option>
                    <option value="bearer_token" ${auth.type === 'bearer_token' ? 'selected' : ''}>Bearer Token</option>
                    <option value="api_key" ${auth.type === 'api_key' ? 'selected' : ''}>API Key (X-API-Key)</option>
                  </select>
                </div>
                <div class="field" style="margin:0">
                  <label style="font-size:12px">Token or Key Value (Supports {{VAR}})</label>
                  <input type="text" class="req-auth-val" value="${esc(auth.token_value || '')}" placeholder="e.g. {{AUTH_TOKEN}}" />
                </div>
              </div>
            </div>

            <!-- Panel: Body -->
            <div class="sub-panel sub-panel-body" style="display:none">
              <textarea class="req-body-text" rows="4" style="font-family:monospace; font-size:12px; width:100%" placeholder='{"key": "value"}'>${esc(r.body || '')}</textarea>
            </div>

            <!-- Panel: Assertions -->
            <div class="sub-panel sub-panel-assertions" style="display:none">
              <div class="assertions-list" style="display:flex; flex-direction:column; gap:8px; margin-bottom:8px">
                ${assertions.map(a => `
                  <div class="assert-row" style="display:flex; gap:8px; align-items:center">
                    <select class="assert-type" style="width:170px">
                      <option value="status_code" ${a.type === 'status_code' ? 'selected' : ''}>Status Code Equals</option>
                      <option value="header_equals" ${a.type === 'header_equals' ? 'selected' : ''}>Header Equals</option>
                      <option value="header_exists" ${a.type === 'header_exists' ? 'selected' : ''}>Header Exists</option>
                      <option value="json_path_equals" ${a.type === 'json_path_equals' ? 'selected' : ''}>JSON Pointer Equals</option>
                      <option value="json_path_exists" ${a.type === 'json_path_exists' ? 'selected' : ''}>JSON Pointer Exists</option>
                      <option value="body_contains" ${a.type === 'body_contains' ? 'selected' : ''}>Body Contains Text</option>
                      <option value="response_time_ms" ${a.type === 'response_time_ms' ? 'selected' : ''}>Response Time &lt; (ms)</option>
                    </select>
                    <input type="text" class="assert-target" placeholder="Target (/data/id, Header)" value="${esc(a.target || '')}" style="flex:1" />
                    <input type="text" class="assert-expected" placeholder="Expected Value" value="${esc(a.expected || '')}" style="flex:1" />
                    <button type="button" class="button ghost small danger btn-del-assert">${iconHtml('x')}</button>
                  </div>
                `).join('')}
              </div>
              <button type="button" class="button ghost small btn-add-assert">+ Add Assertion Rule</button>
            </div>

            <!-- Panel: Extracts -->
            <div class="sub-panel sub-panel-extracts" style="display:none">
              <div class="extracts-list" style="display:flex; flex-direction:column; gap:8px; margin-bottom:8px">
                ${extracts.map(ext => `
                  <div class="extract-row" style="display:flex; gap:8px; align-items:center">
                    <select class="ext-source" style="width:140px">
                      <option value="json_path" ${ext.source === 'json_path' ? 'selected' : ''}>JSON Pointer</option>
                      <option value="header" ${ext.source === 'header' ? 'selected' : ''}>Response Header</option>
                    </select>
                    <input type="text" class="ext-target" placeholder="Target (/data/token, X-Header)" value="${esc(ext.target || '')}" style="flex:1" />
                    <input type="text" class="ext-var" placeholder="Variable Name (e.g. AUTH_TOKEN)" value="${esc(ext.var_name || '')}" style="flex:1" />
                    <button type="button" class="button ghost small danger btn-del-extract">${iconHtml('x')}</button>
                  </div>
                `).join('')}
              </div>
              <button type="button" class="button ghost small btn-add-extract">+ Add Variable Extraction</button>
            </div>
          </div>
        `;
      }

      const bodyHtml = `
        <div class="field">
          <label>Suite Name *</label>
          <input type="text" id="suite-name" value="${esc(suite.name || '')}" placeholder="e.g. Core Checkout & Auth API Suite" />
        </div>
        <div class="field">
          <label>Description</label>
          <input type="text" id="suite-desc" value="${esc(suite.description || '')}" placeholder="e.g. End-to-end integration and smoke assertions" />
        </div>
        <div class="grid two" style="gap:12px">
          <div class="field">
            <label>API Association (Optional)</label>
            <select id="suite-api">
              <option value="">None (Global)</option>
              ${apis.map(a => `<option value="${esc(a.id)}" ${suite.api_id === a.id ? 'selected' : ''}>${esc(a.name)} (${esc(a.base_path)})</option>`).join('')}
            </select>
          </div>
          <div class="field">
            <label>Ownership</label>
            <select id="suite-ownership">
              <option value="team" ${suite.ownership === 'team' ? 'selected' : ''}>Team (Shared)</option>
              <option value="personal" ${suite.ownership === 'personal' ? 'selected' : ''}>Personal</option>
            </select>
          </div>
        </div>

        <fieldset style="margin-top:16px; padding:16px; border:1px solid var(--border); border-radius:8px">
          <legend>Release comparison policy</legend>
          <p class="muted">Applies to comparative runs. Saved with this suite version; changing these checks requires fresh comparison evidence for promotion.</p>
          <div class="field"><label for="suite-compare-headers">Response headers to compare</label>
            <input id="suite-compare-headers" value="${esc((def.comparison?.headers || ['content-type']).join(', '))}" placeholder="content-type, cache-control" />
            <small>Comma separated. Content-Type is checked by default. Sensitive and RelayOps internal headers are excluded.</small>
          </div>
          <div class="field"><label for="suite-ignore-paths">Ignored body fields</label>
            <textarea id="suite-ignore-paths" rows="2" placeholder="/timestamp&#10;/request_id">${esc((def.ignore_paths || []).join('\n'))}</textarea>
            <small>One exact, case-sensitive JSON pointer per line. A pointer ignores its subtree. Use ~1 for / and ~0 for ~. No fields are ignored automatically.</small>
          </div>
          <div class="grid two">
            <div class="field"><label for="suite-latency-percent">Maximum latency increase (%)</label>
              <input id="suite-latency-percent" type="number" min="0" step="any" value="${esc(def.comparison?.max_latency_increase_percent || 0)}" />
            </div>
            <div class="field"><label for="suite-latency-floor">Minimum significant increase (ms)</label>
              <input id="suite-latency-floor" type="number" min="0" step="any" value="${esc(def.comparison?.min_latency_increase_ms || 0)}" />
            </div>
          </div>
          <small>A percentage of 0 disables the timing gate. A regression must exceed both thresholds. Single-request timing can vary; this is not a load benchmark.</small>
        </fieldset>

        <div style="margin-top:16px; border-top:1px solid var(--border); padding-top:16px">
          <div style="display:flex; justify-content:space-between; align-items:center; margin-bottom:12px">
            <h3>Test Requests &amp; Assertions (<span id="req-count">${requests.length}</span>)</h3>
            <button type="button" class="button small" id="btn-add-req">+ Add Request Step</button>
          </div>
          <div id="req-builder-list" style="display:flex; flex-direction:column; gap:14px; max-height:480px; overflow-y:auto; padding-right:4px">
            ${requests.map((r, i) => renderReqTabs(r, i)).join('')}
          </div>
        </div>
      `;

      openModal({
        title: isEdit ? `Edit Test Suite (v${suite.current_version || 1})` : 'New test suite',
        large: true,
        body: bodyHtml,
        actions: [
          {
            label: isEdit ? 'Save changes' : 'Create suite',
            primary: true,
            onClick: async (close, wrap) => {
              const name = wrap.querySelector('#suite-name').value.trim();
              if (!name) { toast('Suite name is required', 'warn'); return; }

              const builtRequests = [];
              wrap.querySelectorAll('.req-item').forEach((item, idx) => {
                const rName = item.querySelector('.req-name').value.trim() || `Step ${idx + 1}`;
                const method = item.querySelector('.req-method').value;
                const path = item.querySelector('.req-path').value.trim() || '/';

                // Query params
                const qp = {};
                item.querySelectorAll('.kv-params-list .kv-row').forEach(row => {
                  const k = row.querySelector('.kv-key').value.trim();
                  const v = row.querySelector('.kv-val').value.trim();
                  if (k) qp[k] = v;
                });

                // Headers
                const hdrs = {};
                item.querySelectorAll('.kv-headers-list .kv-row').forEach(row => {
                  const k = row.querySelector('.kv-key').value.trim();
                  const v = row.querySelector('.kv-val').value.trim();
                  if (k) hdrs[k] = v;
                });

                // Auth
                const authType = item.querySelector('.req-auth-type').value;
                const authVal = item.querySelector('.req-auth-val').value.trim();

                // Body
                const bodyStr = item.querySelector('.req-body-text').value;

                // Assertions
                const assertions = [];
                item.querySelectorAll('.assertions-list .assert-row').forEach(row => {
                  const aType = row.querySelector('.assert-type').value;
                  const target = row.querySelector('.assert-target').value.trim();
                  const expected = row.querySelector('.assert-expected').value.trim();
                  assertions.push({
                    type: aType,
                    target: target || aType,
                    expected: expected
                  });
                });
                if (assertions.length === 0) {
                  assertions.push({ type: 'status_code', target: 'status_code', expected: '200' });
                }

                // Extracts
                const extracts = [];
                item.querySelectorAll('.extracts-list .extract-row').forEach(row => {
                  const src = row.querySelector('.ext-source').value;
                  const trg = row.querySelector('.ext-target').value.trim();
                  const vName = row.querySelector('.ext-var').value.trim();
                  if (vName && trg) {
                    extracts.push({ source: src, target: trg, var_name: vName });
                  }
                });

                builtRequests.push({
                  id: `req-${idx + 1}`,
                  name: rName,
                  method: method,
                  path: path,
                  query_params: qp,
                  headers: hdrs,
                  body: bodyStr,
                  auth: { type: authType, token_value: authVal },
                  assertions: assertions,
                  extracts: extracts
                });
              });

              if (builtRequests.length === 0) {
                toast('Suite must contain at least one request step', 'warn');
                return;
              }

              const payload = {
                name: name,
                description: wrap.querySelector('#suite-desc').value.trim(),
                api_id: wrap.querySelector('#suite-api').value || undefined,
                ownership: wrap.querySelector('#suite-ownership').value,
                visibility: 'tenant',
                definition: {
                  name: name,
                  description: wrap.querySelector('#suite-desc').value.trim(),
                  requests: builtRequests,
                  variables: def.variables || [],
                  ignore_paths: wrap.querySelector('#suite-ignore-paths').value.split(/\r?\n/).map(p => p.trim()).filter(Boolean),
                  comparison: {
                    headers: wrap.querySelector('#suite-compare-headers').value.split(',').map(h => h.trim()).filter(Boolean),
                    max_latency_increase_percent: Number(wrap.querySelector('#suite-latency-percent').value),
                    min_latency_increase_ms: Number(wrap.querySelector('#suite-latency-floor').value)
                  }
                }
              };

              try {
                if (isEdit) {
                  await api('PUT', `/api/tests/suites/${suite.id}`, payload);
                  toast('Test suite updated successfully', 'success');
                } else {
                  await api('POST', '/api/tests/suites', payload);
                  toast('Test suite created successfully', 'success');
                }
                close();
                views.tests();
              } catch (err) {
                toast(err.message, 'error');
              }
            }
          }
        ],
        onOpen: (wrap) => {
          const list = wrap.querySelector('#req-builder-list');

          // Keyboard navigation follows the tab pattern within each request.
          function setupTabs() {
            list.querySelectorAll('.req-item').forEach(item => {
              const tabs = [...item.querySelectorAll('.sub-tab-btn')];
              if (!tabs.length) return;
              tabs[0].parentElement.setAttribute('role', 'tablist');
              tabs.forEach((tab, i) => {
                tab.setAttribute('role', 'tab');
                tab.setAttribute('aria-selected', String(tab.classList.contains('active')));
                tab.tabIndex = tab.classList.contains('active') ? 0 : -1;
                tab.onkeydown = e => {
                  let next;
                  if (e.key === 'ArrowRight') next = (i + 1) % tabs.length;
                  if (e.key === 'ArrowLeft') next = (i + tabs.length - 1) % tabs.length;
                  if (e.key === 'Home') next = 0;
                  if (e.key === 'End') next = tabs.length - 1;
                  if (next !== undefined) { e.preventDefault(); tabs[next].click(); tabs[next].focus(); }
                };
              });
            });
          }
          setupTabs();
          // Sub-tabs switching
          list.addEventListener('click', (e) => {
            const tabBtn = e.target.closest('.sub-tab-btn');
            if (tabBtn) {
              const reqItem = tabBtn.closest('.req-item');
              const targetPanel = tabBtn.dataset.subtab;
              reqItem.querySelectorAll('.sub-tab-btn').forEach(b => { b.classList.toggle('active', b === tabBtn); b.setAttribute('aria-selected', String(b === tabBtn)); b.tabIndex = b === tabBtn ? 0 : -1; });
              reqItem.querySelectorAll('.sub-panel').forEach(p => {
                p.style.display = p.classList.contains(`sub-panel-${targetPanel}`) ? '' : 'none';
              });
              return;
            }

            // Remove request
            if (e.target.closest('.btn-del-req')) {
              e.target.closest('.req-item').remove();
              wrap.querySelector('#req-count').textContent = list.querySelectorAll('.req-item').length;
              return;
            }

            // Delete KV / Assert / Extract row
            if (e.target.closest('.btn-del-kv')) {
              e.target.closest('.kv-row').remove();
              return;
            }
            if (e.target.closest('.btn-del-assert')) {
              e.target.closest('.assert-row').remove();
              return;
            }
            if (e.target.closest('.btn-del-extract')) {
              e.target.closest('.extract-row').remove();
              return;
            }

            // Add Param
            if (e.target.closest('.btn-add-param')) {
              const pList = e.target.closest('.sub-panel-params').querySelector('.kv-params-list');
              const row = document.createElement('div');
              row.className = 'kv-row';
              row.style.cssText = 'display:flex; gap:8px';
              row.innerHTML = `
                <input type="text" class="kv-key" placeholder="Parameter Name" style="flex:1" />
                <input type="text" class="kv-val" placeholder="Value (e.g. {{VAR}})" style="flex:2" />
                <button type="button" class="button ghost small danger btn-del-kv">${iconHtml('x')}</button>
              `;
              pList.appendChild(row);
              return;
            }

            // Add Header
            if (e.target.closest('.btn-add-header')) {
              const hList = e.target.closest('.sub-panel-headers').querySelector('.kv-headers-list');
              const row = document.createElement('div');
              row.className = 'kv-row';
              row.style.cssText = 'display:flex; gap:8px';
              row.innerHTML = `
                <input type="text" class="kv-key" placeholder="Header Name" style="flex:1" />
                <input type="text" class="kv-val" placeholder="Header Value" style="flex:2" />
                <button type="button" class="button ghost small danger btn-del-kv">${iconHtml('x')}</button>
              `;
              hList.appendChild(row);
              return;
            }

            // Add Assertion
            if (e.target.closest('.btn-add-assert')) {
              const aList = e.target.closest('.sub-panel-assertions').querySelector('.assertions-list');
              const row = document.createElement('div');
              row.className = 'assert-row';
              row.style.cssText = 'display:flex; gap:8px; align-items:center';
              row.innerHTML = `
                <select class="assert-type" style="width:170px">
                  <option value="status_code">Status Code Equals</option>
                  <option value="header_equals">Header Equals</option>
                  <option value="header_exists">Header Exists</option>
                  <option value="json_path_equals">JSON Pointer Equals</option>
                  <option value="json_path_exists">JSON Pointer Exists</option>
                  <option value="body_contains">Body Contains Text</option>
                  <option value="response_time_ms">Response Time &lt; (ms)</option>
                </select>
                <input type="text" class="assert-target" placeholder="Target (/data/id, Header)" style="flex:1" />
                <input type="text" class="assert-expected" placeholder="Expected Value" style="flex:1" />
                <button type="button" class="button ghost small danger btn-del-assert">${iconHtml('x')}</button>
              `;
              aList.appendChild(row);
              return;
            }

            // Add Extract
            if (e.target.closest('.btn-add-extract')) {
              const eList = e.target.closest('.sub-panel-extracts').querySelector('.extracts-list');
              const row = document.createElement('div');
              row.className = 'extract-row';
              row.style.cssText = 'display:flex; gap:8px; align-items:center';
              row.innerHTML = `
                <select class="ext-source" style="width:140px">
                  <option value="json_path">JSON Pointer</option>
                  <option value="header">Response Header</option>
                </select>
                <input type="text" class="ext-target" placeholder="Target (/data/id, Header)" style="flex:1" />
                <input type="text" class="ext-var" placeholder="Variable Name" style="flex:1" />
                <button type="button" class="button ghost small danger btn-del-extract">${iconHtml('x')}</button>
              `;
              eList.appendChild(row);
              return;
            }
          });

          // Add Step button
          wrap.querySelector('#btn-add-req').onclick = () => {
            const count = list.querySelectorAll('.req-item').length;
            const div = document.createElement('div');
            div.innerHTML = renderReqTabs({
              name: `Step ${count + 1}`,
              method: 'GET',
              path: '/',
              headers: {},
              query_params: {},
              body: '',
              auth: { type: 'none' },
              assertions: [{ type: 'status_code', target: 'status_code', expected: '200' }],
              extracts: []
            }, count);
            list.appendChild(div.firstElementChild);
            setupTabs();
            wrap.querySelector('#req-count').textContent = count + 1;
          };
        }
      });
    }

    // -------------------------------------------------------------------------
    // Execution Modal
    // -------------------------------------------------------------------------
    function openRunModal(suiteId, envs, defaultMode = 'standard') {
      openModal({
        title: defaultMode === 'comparison' ? 'Execute Comparative Run (Baseline vs Candidate)' : 'Execute Test Suite',
        body: `
          <div class="field">
            <label>Execution Mode</label>
            <select id="run-mode">
              <option value="standard" ${defaultMode === 'standard' ? 'selected' : ''}>Standard Run (Single Revision)</option>
              <option value="comparison" ${defaultMode === 'comparison' ? 'selected' : ''}>Comparative Run (v_N vs v_N+1 Diff)</option>
            </select>
          </div>
          <div class="field">
            <label>Target Environment</label>
            <select id="run-env">
              <option value="">Default Gateway Target</option>
              ${envs.map(e => `<option value="${esc(e.id)}">${esc(e.name)} (${esc(e.gateway_target || 'Loopback')})</option>`).join('')}
            </select>
          </div>
          <div id="standard-fields" style="${defaultMode === 'comparison' ? 'display:none' : ''}">
            <div class="field">
              <label>Target Revision (Optional)</label>
              <input type="number" id="run-target-rev" placeholder="e.g. 5 (Leave empty for active gateway revision)" />
            </div>
          </div>
          <div id="comparison-fields" style="${defaultMode === 'comparison' ? '' : 'display:none'}">
            <div class="grid two" style="gap:12px">
              <div class="field">
                <label>Baseline Revision *</label>
                <input type="number" id="run-baseline-rev" placeholder="e.g. 1" />
              </div>
              <div class="field">
                <label>Candidate Revision *</label>
                <input type="number" id="run-candidate-rev" placeholder="e.g. 2" />
              </div>
            </div>
            <div class="field">
              <label>Ignore JSON Paths (Comma-separated)</label>
              <input type="text" id="run-ignore-paths" value="/timestamp, /id, /created_at" placeholder="/timestamp, /uuid" />
              <small class="muted">JSON pointer paths ignored during deep structural diffing.</small>
            </div>
          </div>
        `,
        actions: [
          {
            label: 'Start Execution',
            primary: true,
            onClick: async (close, wrap) => {
              const mode = wrap.querySelector('#run-mode').value;
              const envId = wrap.querySelector('#run-env').value;

              const payload = {
                suite_id: suiteId,
                environment_id: envId || undefined,
                execution_mode: mode
              };

              if (mode === 'standard') {
                const rev = wrap.querySelector('#run-target-rev').value.trim();
                if (rev) payload.target_revision = parseInt(rev, 10);
              } else {
                const bRev = wrap.querySelector('#run-baseline-rev').value.trim();
                const cRev = wrap.querySelector('#run-candidate-rev').value.trim();
                if (!bRev || !cRev) {
                  toast('Baseline and candidate revisions are required for comparison mode', 'warn');
                  return;
                }
                payload.baseline_revision = parseInt(bRev, 10);
                payload.candidate_revision = parseInt(cRev, 10);
                const ignore = wrap.querySelector('#run-ignore-paths').value;
                payload.ignore_json_paths = ignore.split(',').map(s => s.trim()).filter(Boolean);
              }

              try {
                const run = await api('POST', '/api/tests/runs', payload);
                toast('Test run queued successfully', 'success');
                close();
                location.hash = `#/tests/run-${run.id}`;
              } catch (err) {
                toast(err.message, 'error');
              }
            }
          }
        ],
        onOpen: (wrap) => {
          const modeSel = wrap.querySelector('#run-mode');
          modeSel.onchange = () => {
            const isComp = modeSel.value === 'comparison';
            wrap.querySelector('#standard-fields').style.display = isComp ? 'none' : '';
            wrap.querySelector('#comparison-fields').style.display = isComp ? '' : 'none';
          };
        }
      });
    }

    // -------------------------------------------------------------------------
    // Import Modal
    // -------------------------------------------------------------------------
    function openImportModal(apis) {
      openModal({
        title: 'Import OpenAPI or Postman collection',
        large: true,
        body: `
          <div class="field">
            <label>Format</label>
            <select id="import-format">
              <option value="openapi">OpenAPI 3.0 / 3.1 / Swagger 2.0 (JSON or YAML)</option>
              <option value="postman">Postman Collection v2.1 (JSON)</option>
            </select>
          </div>
          <div class="field">
            <label>Associated API</label>
            <select id="import-api">
              <option value="">None (Global)</option>
              ${apis.map(a => `<option value="${esc(a.id)}">${esc(a.name)} (${esc(a.base_path)})</option>`).join('')}
            </select>
          </div>
          <div class="field">
            <label>Paste Spec Content</label>
            <textarea id="import-content" rows="12" style="font-family:monospace; font-size:12px" placeholder="Paste OpenAPI JSON/YAML or Postman Collection JSON here…"></textarea>
          </div>
        `,
        actions: [
          {
            label: 'Import as suite',
            primary: true,
            onClick: async (close, wrap) => {
              const format = wrap.querySelector('#import-format').value;
              const apiId = wrap.querySelector('#import-api').value;
              const content = wrap.querySelector('#import-content').value.trim();

              if (!content) {
                toast('Spec content cannot be empty', 'warn');
                return;
              }

              try {
                const imported = await api('POST', '/api/tests/import', {
                  format: format,
                  api_id: apiId || undefined,
                  content: content
                });
                toast(`Imported suite "${imported.name}" with ${imported.definition?.requests?.length || 0} steps`, 'success');
                close();
                views.tests();
              } catch (err) {
                toast('Import failed: ' + err.message, 'error');
              }
            }
          }
        ]
      });
    }

    // -------------------------------------------------------------------------
    // Promotion Gate Policies Modal
    // -------------------------------------------------------------------------
    function openGatePoliciesModal(apis, suites) {
      if (apis.length === 0) {
        toast('No APIs defined to configure gate policies for', 'warn');
        return;
      }

      openModal({
        title: 'Promotion gates',
        body: `
          <div class="field">
            <label>Select API</label>
            <select id="gate-api-select">
              ${apis.map(a => `<option value="${esc(a.id)}">${esc(a.name)} (${esc(a.base_path)})</option>`).join('')}
            </select>
          </div>
          <div id="gate-settings-container" style="margin-top:16px">Loading policy settings…</div>
        `,
        actions: [
          {
            label: 'Save gates',
            primary: true,
            onClick: async (close, wrap) => {
              const apiId = wrap.querySelector('#gate-api-select').value;
              const enforce = wrap.querySelector('#gate-enforce')?.checked || false;
              const selectedSuites = [...wrap.querySelectorAll('.gate-suite-cb:checked')].map(cb => cb.value);
              const freshness = parseInt(wrap.querySelector('#gate-freshness')?.value || '3600', 10);

              try {
                await api('PUT', `/api/tests/gates/${apiId}`, {
                  enforcement_enabled: enforce,
                  required_suite_ids: selectedSuites,
                  freshness_seconds: freshness,
                  target_environment: 'canary'
                });
                toast('Promotion gate policy updated', 'success');
                close();
              } catch (err) {
                toast(err.message, 'error');
              }
            }
          }
        ],
        onOpen: (wrap) => {
          const sel = wrap.querySelector('#gate-api-select');
          const container = wrap.querySelector('#gate-settings-container');

          async function loadPolicy(apiId) {
            container.innerHTML = '<p class="muted">Loading policy…</p>';
            const res = await api('GET', `/api/tests/gates/${apiId}`).catch(() => null);
            const policy = res?.policy || {};
            const required = policy.required_suite_ids || [];

            container.innerHTML = `
              <div class="field-checkbox" style="margin-bottom:14px">
                <label style="display:flex; gap:8px; align-items:center; cursor:pointer">
                  <input type="checkbox" id="gate-enforce" ${policy.enforcement_enabled ? 'checked' : ''} />
                  <strong>Enforce Test Studio Gate on Promotion</strong>
                </label>
                <small class="muted" style="display:block; margin-left:24px; margin-top:4px">When enabled, promoting canary releases to 100% of fleet gateways is strictly blocked unless passing test suite evidence is recorded.</small>
              </div>

              <div class="field">
                <label>Evidence Freshness Window (Seconds)</label>
                <input type="number" id="gate-freshness" value="${policy.freshness_seconds || 3600}" />
                <small class="muted">Test evidence older than this window will not satisfy promotion criteria.</small>
              </div>

              <div class="field" style="margin-top:14px">
                <label>Required Test Suites</label>
                <div style="max-height:160px; overflow-y:auto; border:1px solid var(--border); border-radius:4px; padding:8px">
                  ${suites.length === 0 ? '<p class="muted small">No test suites created yet.</p>' :
                    suites.map(s => `
                      <label style="display:flex; gap:8px; align-items:center; margin-bottom:6px; font-size:12px; cursor:pointer">
                        <input type="checkbox" class="gate-suite-cb" value="${esc(s.id)}" ${required.includes(s.id) ? 'checked' : ''} />
                        <span>${esc(s.name)} (v${s.current_version || 1})</span>
                      </label>
                    `).join('')}
                </div>
              </div>
            `;
          }

          sel.onchange = () => loadPolicy(sel.value);
          if (sel.value) loadPolicy(sel.value);
        }
      });
    }

    // -------------------------------------------------------------------------
    // Environment Modal (Dynamic Key-Value Editor, No Raw JSON)
    // -------------------------------------------------------------------------
    function openEnvModal(existing) {
      const isEdit = !!existing;
      const env = existing || {};
      const vars = Object.entries(env.variables || {});

      openModal({
        title: isEdit ? 'Edit Test Environment' : 'Add test environment',
        large: true,
        body: `
          <div class="field">
            <label>Environment Name *</label>
            <input type="text" id="env-name" value="${esc(env.name || '')}" placeholder="e.g. Staging Gateway / Canary Target" />
          </div>
          <div class="field">
            <label>Gateway Target Address</label>
            <input type="text" id="env-target" value="${esc(env.gateway_target || 'http://127.0.0.1:8080')}" placeholder="http://127.0.0.1:8080" />
            <small class="muted">Internal gateway URL used to execute test requests. Cloud metadata endpoints (169.254.169.254) and internal database ports (5432, 6379, 22) are blocked.</small>
          </div>
          <div class="field" style="margin-top:16px">
            <div style="display:flex; justify-content:space-between; align-items:center; margin-bottom:8px">
              <label style="margin:0">Environment Variables</label>
              <button type="button" class="button small ghost" id="btn-add-env-var">+ Add Variable</button>
            </div>
            <div id="env-vars-container" style="display:flex; flex-direction:column; gap:8px; max-height:220px; overflow-y:auto; padding-right:4px">
              ${vars.length === 0 ? `
                <div class="env-var-row" style="display:flex; gap:8px">
                  <input type="text" class="env-var-key" placeholder="Variable Name (e.g. BASE_URL)" style="flex:1" />
                  <input type="text" class="env-var-val" placeholder="Value (e.g. /api/v1)" style="flex:2" />
                  <button type="button" class="button ghost small danger btn-del-env-var">${iconHtml('x')}</button>
                </div>
              ` : vars.map(([k, v]) => `
                <div class="env-var-row" style="display:flex; gap:8px">
                  <input type="text" class="env-var-key" placeholder="Variable Name" value="${esc(k)}" style="flex:1" />
                  <input type="text" class="env-var-val" placeholder="Value" value="${esc(v)}" style="flex:2" />
                  <button type="button" class="button ghost small danger btn-del-env-var">${iconHtml('x')}</button>
                </div>
              `).join('')}
            </div>
            <small class="muted" style="display:block; margin-top:6px">Reference variables inside request paths, headers, and body using <code>{{VARIABLE_NAME}}</code>.</small>
          </div>
        `,
        actions: [
          {
            label: isEdit ? 'Save Environment' : 'Create environment',
            primary: true,
            onClick: async (close, wrap) => {
              const name = wrap.querySelector('#env-name').value.trim();
              if (!name) { toast('Environment name is required', 'warn'); return; }

              const parsedVars = {};
              wrap.querySelectorAll('#env-vars-container .env-var-row').forEach(row => {
                const k = row.querySelector('.env-var-key').value.trim();
                const v = row.querySelector('.env-var-val').value.trim();
                if (k) parsedVars[k] = v;
              });

              const payload = {
                name: name,
                gateway_target: wrap.querySelector('#env-target').value.trim(),
                variables: parsedVars
              };

              try {
                if (isEdit) {
                  await api('PUT', `/api/tests/environments/${env.id}`, payload);
                  toast('Environment updated', 'success');
                } else {
                  await api('POST', '/api/tests/environments', payload);
                  toast('Environment created', 'success');
                }
                close();
                views.tests();
              } catch (err) {
                toast(err.message, 'error');
              }
            }
          }
        ],
        onOpen: (wrap) => {
          const container = wrap.querySelector('#env-vars-container');
          wrap.querySelector('#btn-add-env-var').onclick = () => {
            const row = document.createElement('div');
            row.className = 'env-var-row';
            row.style.cssText = 'display:flex; gap:8px';
            row.innerHTML = `
              <input type="text" class="env-var-key" placeholder="Variable Name" style="flex:1" />
              <input type="text" class="env-var-val" placeholder="Value" style="flex:2" />
              <button type="button" class="button ghost small danger btn-del-env-var">${iconHtml('x')}</button>
            `;
            container.appendChild(row);
          };
          container.addEventListener('click', (e) => {
            if (e.target.closest('.btn-del-env-var')) {
              e.target.closest('.env-var-row').remove();
            }
          });
        }
      });
    }
  };
})();
