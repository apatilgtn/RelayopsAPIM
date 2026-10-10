/* RelayOps APIOps & Release Passports
 * Manages declarative delivery lifecycles, environment promotions,
 * gate-protected release passports, and bundle validations.
 */
(() => {
  'use strict';

  window.initAPIOps = function(ctx) {
    const { views, api, toast, esc, openModal, statusPill, fmtDate, $ } = ctx;

    let currentTab = 'deployments';

    views.apiops = async (param, soft = false) => {
      if (param && ['deployments', 'environments', 'inspector'].includes(param)) {
        currentTab = param;
      }

      const container = $('#view');
      container.innerHTML = `
        <div class="toolbar" style="margin-bottom:16px; display:flex; justify-content:space-between; align-items:center;">
          <div class="filter-group">
            <button class="filter ${currentTab === 'deployments' ? 'active' : ''}" data-tab="deployments">Deployments</button>
            <button class="filter ${currentTab === 'environments' ? 'active' : ''}" data-tab="environments">Target environments</button>
            <button class="filter ${currentTab === 'inspector' ? 'active' : ''}" data-tab="inspector">Bundle inspector</button>
          </div>
          <div id="apiops-tab-actions"></div>
        </div>
        <div id="apiops-tab-content">
          <div class="loading-state" role="status"><span class="loading-spinner" aria-hidden="true"></span>Loading APIOps lifecycle state…</div>
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
      const actionsEl = $('#apiops-tab-actions');
      const contentEl = $('#apiops-tab-content');
      if (!contentEl) return;

      if (currentTab === 'deployments') {
        await renderDeploymentsTab(actionsEl, contentEl);
      } else if (currentTab === 'environments') {
        await renderEnvironmentsTab(actionsEl, contentEl);
      } else if (currentTab === 'inspector') {
        await renderInspectorTab(actionsEl, contentEl);
      }
    }

    // -------------------------------------------------------------------------
    // 1. Deployments Tab
    // -------------------------------------------------------------------------
    async function renderDeploymentsTab(actionsEl, contentEl) {
      actionsEl.innerHTML = `
        <button class="button small primary" id="btn-plan-dep">Plan deployment</button>
        <button class="button small ghost" id="btn-refresh-dep">Refresh</button>
      `;

      $('#btn-plan-dep').onclick = () => openPlanDeploymentModal();
      $('#btn-refresh-dep').onclick = () => renderDeploymentsTab(actionsEl, contentEl);

      try {
        const [deployments, envs] = await Promise.all([
          api('GET', '/api/apiops/deployments'),
          api('GET', '/api/apiops/environments')
        ]);

        const envMap = {};
        (envs || []).forEach(e => { envMap[e.id] = e.name; });

        if (!deployments || deployments.length === 0) {
          contentEl.innerHTML = `
            <div class="card empty" style="text-align:center; padding:48px 24px;">
              <div style="font-size:32px; margin-bottom:12px;"><span data-icon="git-pull-request"></span></div>
              <h3>No deployments yet</h3>
              <p class="muted" style="max-width:540px; margin:0 auto 16px;">
                Deployments track the complete delivery lifecycle from source commit hash to sealed Release Passport.
                Plan a deployment via the CLI (<code>relayopsctl apply</code>) or the console to begin.
              </p>
              <button class="button primary" id="btn-empty-plan">Plan deployment</button>
            </div>
          `;
          $('#btn-empty-plan').onclick = () => openPlanDeploymentModal();
          return;
        }

        contentEl.innerHTML = `
          <div class="card">
            <div class="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Commit / Branch</th>
                    <th>Target Env</th>
                    <th>Plan Hash</th>
                    <th>Base Rev</th>
                    <th>Status</th>
                    <th>Created</th>
                    <th style="text-align:right;">Lifecycle Actions</th>
                  </tr>
                </thead>
                <tbody>
                  ${deployments.map(d => {
                    const statusClass = d.status === 'promoted' ? 'green' : (d.status === 'aborted' ? 'alert' : 'blue');
                    const commitDisplay = d.commit_sha ? d.commit_sha.substring(0, 7) : '—';
                    const branchDisplay = d.branch || 'main';
                    const envName = envMap[d.environment_id] || d.environment_id;
                    const planShort = d.plan_hash ? (d.plan_hash.startsWith('sha256:') ? d.plan_hash.substring(7, 15) : d.plan_hash.substring(0, 8)) : '—';
                    
                    return `
                      <tr>
                        <td>
                          <strong><code>${esc(commitDisplay)}</code></strong>
                          <div class="muted small">${esc(branchDisplay)}</div>
                        </td>
                        <td><span class="tag">${esc(envName)}</span></td>
                        <td><code title="${esc(d.plan_hash)}">${esc(planShort)}</code></td>
                        <td>rev_${d.expected_base_revision}</td>
                        <td><span class="tag ${statusClass}">${esc(d.status.toUpperCase())}</span></td>
                        <td class="muted small">${fmtDate(d.created_at)}</td>
                        <td style="text-align:right;">
                          ${d.status === 'planned' ? `
                            <button class="button small" data-verify-dep="${esc(d.id)}">Verify</button>
                            <button class="button small primary" data-promote-dep="${esc(d.id)}">Promote</button>
                            <button class="button small danger ghost" data-abort-dep="${esc(d.id)}">Abort</button>
                          ` : ''}
                          ${d.status === 'verifying' ? `
                            <button class="button small primary" data-promote-dep="${esc(d.id)}">Promote</button>
                            <button class="button small danger ghost" data-abort-dep="${esc(d.id)}">Abort</button>
                          ` : ''}
                          ${d.status === 'promoted' ? `
                            <button class="button small ghost" data-view-passport="${esc(d.id)}"><span data-icon="shield-check"></span> Release Passport</button>
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

        // Wire event handlers
        contentEl.querySelectorAll('[data-verify-dep]').forEach(btn => {
          btn.onclick = async () => {
            btn.disabled = true;
            try {
              const res = await api('POST', `/api/apiops/deployments/${btn.dataset.verifyDep}/verify`);
              toast(`Evidence verification started: ${res.evidence_digest ? res.evidence_digest.substring(0, 12) : 'active'}`);
              renderDeploymentsTab(actionsEl, contentEl);
            } catch (err) {
              toast(`Verification error: ${err.message}`, 'alert');
              btn.disabled = false;
            }
          };
        });

        contentEl.querySelectorAll('[data-promote-dep]').forEach(btn => {
          btn.onclick = () => openPromoteModal(btn.dataset.promoteDep, () => renderDeploymentsTab(actionsEl, contentEl));
        });

        contentEl.querySelectorAll('[data-abort-dep]').forEach(btn => {
          btn.onclick = async () => {
            if (!confirm('Abort this deployment? Any active verification or canary stages will be cancelled.')) return;
            try {
              await api('POST', `/api/apiops/deployments/${btn.dataset.abortDep}/abort`);
              toast('Deployment aborted');
              renderDeploymentsTab(actionsEl, contentEl);
            } catch (err) {
              toast(`Abort failed: ${err.message}`, 'alert');
            }
          };
        });

        contentEl.querySelectorAll('[data-view-passport]').forEach(btn => {
          btn.onclick = () => openPassportModal(btn.dataset.viewPassport);
        });

      } catch (err) {
        contentEl.innerHTML = `<div class="card alert"><p>Failed to load deployments: ${esc(err.message)}</p></div>`;
      }
    }

    // -------------------------------------------------------------------------
    // 2. Target environments Tab
    // -------------------------------------------------------------------------
    async function renderEnvironmentsTab(actionsEl, contentEl) {
      actionsEl.innerHTML = `
        <button class="button small primary" id="btn-create-env">New environment</button>
        <button class="button small ghost" id="btn-refresh-env" title="Refresh">↻</button>
      `;

      $('#btn-create-env').onclick = () => openCreateEnvironmentModal();
      $('#btn-refresh-env').onclick = () => renderEnvironmentsTab(actionsEl, contentEl);

      try {
        const envs = await api('GET', '/api/apiops/environments');
        if (!envs || envs.length === 0) {
          contentEl.innerHTML = `
            <div class="card empty" style="text-align:center; padding:48px 24px;">
              <div style="font-size:32px; margin-bottom:12px;"><span data-icon="network"></span></div>
              <h3>No Environments Configured</h3>
              <p class="muted" style="max-width:540px; margin:0 auto 16px;">
                Define logical target environments (e.g. dev, staging, prod) with specific overlay bindings.
              </p>
              <button class="button primary" id="btn-empty-env">Add environment</button>
            </div>
          `;
          $('#btn-empty-env').onclick = () => openCreateEnvironmentModal();
          return;
        }

        contentEl.innerHTML = `
          <div class="card">
            <div class="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Name</th>
                    <th>Type</th>
                    <th>Target gateway</th>
                    <th>Created</th>
                  </tr>
                </thead>
                <tbody>
                  ${envs.map(e => `
                    <tr>
                      <td><strong>${esc(e.name)}</strong>${e.description ? `<div class="small muted">${esc(e.description)}</div>` : ''}</td>
                      <td><span class="tag ${e.is_production ? 'warn' : 'gray'}">${e.is_production ? 'Production' : 'Non-production'}</span></td>
                      <td class="mono small">${e.target_gateway_url ? esc(e.target_gateway_url) : '<span class="muted">This gateway fleet</span>'}</td>
                      <td class="muted small">${fmtDate(e.created_at)}</td>
                    </tr>
                  `).join('')}
                </tbody>
              </table>
            </div>
          </div>
        `;
      } catch (err) {
        contentEl.innerHTML = `<div class="card alert"><p>Failed to load environments: ${esc(err.message)}</p></div>`;
      }
    }

    // -------------------------------------------------------------------------
    // 3. Bundle inspector Tab
    // -------------------------------------------------------------------------
    async function renderInspectorTab(actionsEl, contentEl) {
      actionsEl.innerHTML = '';

      contentEl.innerHTML = `
        <div class="card" style="padding:24px;">
          <h3>APIOps Bundle Validator</h3>
          <p class="muted" style="margin-bottom:16px;">
            Validate a compiled canonical bundle before pushing to remote pipelines.
            Paste bundle JSON below to inspect format consistency and calculate cryptographic digests.
          </p>
          <div class="form-group" style="margin-bottom:16px;">
            <label for="bundle-json-input"><strong>Bundle JSON:</strong></label>
            <textarea id="bundle-json-input" rows="12" style="font-family:monospace; font-size:12px; width:100%; box-sizing:border-box;" placeholder='{"format_version": "1.0", "source_hash": "...", "config": { ... }}'></textarea>
          </div>
          <button class="button primary" id="btn-validate-bundle">Validate Bundle</button>
          <div id="inspector-result" style="margin-top:20px;"></div>
        </div>
      `;

      $('#btn-validate-bundle').onclick = async () => {
        const val = $('#bundle-json-input').value.trim();
        const resEl = $('#inspector-result');
        if (!val) {
          toast('Please enter bundle JSON to validate', 'alert');
          return;
        }
        let parsed;
        try {
          parsed = JSON.parse(val);
        } catch (e) {
          resEl.innerHTML = `<div class="card alert"><p>JSON Parse Error: ${esc(e.message)}</p></div>`;
          return;
        }

        try {
          const res = await api('POST', '/api/apiops/bundles/validate', parsed);
          resEl.innerHTML = `
            <div class="card" style="background:rgba(196,243,107,0.08); border-color:#c4f36b; padding:16px;">
              <h4 style="color:#c4f36b; margin-top:0;">✓ Bundle is Valid</h4>
              <p><strong>Source Hash:</strong> <code>${esc(res.source_hash || '—')}</code></p>
              <p><strong>Rendered Hash:</strong> <code>${esc(res.rendered_hash || '—')}</code></p>
              <p><strong>Declared APIs:</strong> ${res.apis_count} | <strong>Plans:</strong> ${res.plans_count}</p>
            </div>
          `;
        } catch (err) {
          resEl.innerHTML = `
            <div class="card alert" style="padding:16px;">
              <h4 style="margin-top:0;">✗ Validation Failed</h4>
              <p>${esc(err.message)}</p>
            </div>
          `;
        }
      };
    }

    // -------------------------------------------------------------------------
    // Modals
    // -------------------------------------------------------------------------
    function openPlanDeploymentModal() {
      Promise.all([
        api('GET', '/api/apiops/environments').catch(() => []),
        api('GET', '/api/revisions').catch(() => [])
      ]).then(([envs, revs]) => {
        if (!(envs || []).length) {
          openModal({
            title: 'Plan deployment',
            body: '<p>Deployments target an environment. Create one first (for example Staging or Production).</p>',
            confirmText: 'New environment',
            onConfirm: () => { setTimeout(openCreateEnvironmentModal, 0); return true; }
          });
          return;
        }
        const canaries = (revs || []).filter(r => r.status === 'canary');
        const modal = openModal({
          title: 'Plan deployment',
          body: `
            <div class="field">
              <label for="plan-env-id">Target environment</label>
              <select id="plan-env-id" class="input" style="width:100%;">
                ${(envs || []).map(e => `<option value="${esc(e.id)}">${esc(e.name)}${e.is_production ? ' (production)' : ''}</option>`).join('')}
              </select>
            </div>
            <div class="field">
              <label for="plan-candidate-rev">Canary candidate (optional)</label>
              <select id="plan-candidate-rev" class="input" style="width:100%;">
                <option value="0">None: register the change before publishing</option>
                ${canaries.map(c => `<option value="${c.revision}">rev_${c.revision} (canary: ${c.description || 'in-flight'})</option>`).join('')}
              </select>
            </div>
            <div class="field">
              <label for="plan-commit-sha">Git commit SHA</label>
              <input id="plan-commit-sha" class="input" style="width:100%; font-family:monospace;" placeholder="e.g. 7f8a9b1c2d3e">
            </div>
            <div class="field">
              <label for="plan-branch">Branch</label>
              <input id="plan-branch" class="input" style="width:100%;" value="main">
            </div>
            <div class="field">
              <label for="plan-repo-url">Repository URL</label>
              <input id="plan-repo-url" class="input" style="width:100%;" placeholder="https://github.com/org/repo">
            </div>
            <div class="field">
              <label for="plan-hash">Reviewed plan hash (optional)</label>
              <input id="plan-hash" class="input" style="width:100%; font-family:monospace;" placeholder="sha256:… (derived when empty)">
            </div>
          `,
          confirmText: 'Plan deployment',
          onConfirm: async () => {
            const envID = $('#plan-env-id').value;
            const candidateRev = parseInt($('#plan-candidate-rev').value, 10) || 0;
            const commitSHA = $('#plan-commit-sha').value.trim();
            const branch = $('#plan-branch').value.trim();
            const repoURL = $('#plan-repo-url').value.trim();
            const planHash = $('#plan-hash').value.trim();

            if (!commitSHA) {
              toast('Git commit SHA is required', 'alert');
              return false;
            }

            try {
              await api('POST', '/api/apiops/deployments/plan', {
                environment_id: envID,
                candidate_revision: candidateRev,
                commit_sha: commitSHA,
                branch: branch || 'main',
                repo_url: repoURL,
                plan_hash: planHash,
                expected_base_revision: 0
              });
              toast('Deployment successfully planned');
              modal.close();
              views.apiops('deployments');
            } catch (err) {
              toast(`Plan failed: ${err.message}`, 'alert');
              return false;
            }
          }
        });
      });
    }

    function openCreateEnvironmentModal() {
      openModal({
        title: 'New environment',
        body: `
          <div class="field">
            <label for="env-name">Name</label>
            <input id="env-name" class="input" placeholder="e.g. Production EU">
          </div>
          <div class="field">
            <label for="env-desc">Description (optional)</label>
            <input id="env-desc" class="input" placeholder="e.g. Customer-facing gateways in Frankfurt">
          </div>
          <div class="field">
            <label for="env-url">Target gateway URL (optional)</label>
            <input id="env-url" class="input" placeholder="https://api.eu.example.com">
            <div class="help">Leave empty to deploy to the gateways managed by this control plane.</div>
          </div>
          <div class="field check">
            <input type="checkbox" id="env-prod">
            <label for="env-prod">Production environment</label>
            <div class="help">Shown on deployments and Release Passports that target this environment.</div>
          </div>
        `,
        confirmText: 'Create environment',
        onConfirm: async (wrap) => {
          const name = wrap.querySelector('#env-name').value.trim();
          if (!name) {
            toast('Enter a name for the environment', 'error');
            return false;
          }
          try {
            await api('POST', '/api/apiops/environments', {
              name,
              description: wrap.querySelector('#env-desc').value.trim(),
              target_gateway_url: wrap.querySelector('#env-url').value.trim(),
              is_production: wrap.querySelector('#env-prod').checked
            });
            toast(`Created environment ${esc(name)}`, 'success');
            views.apiops('environments');
          } catch (err) {
            toast(`Create failed: ${esc(err.message)}`, 'error');
            return false;
          }
        }
      });
    }

    function openPromoteModal(deploymentID, onSuccess) {
      const modal = openModal({
        title: 'Promote deployment',
        body: `
          <p>
            Promoting seals the release into an immutable <strong>Release Passport</strong>.
            All enforced Test Gate policies must pass before promotion can succeed.
          </p>
          <div class="field">
            <label for="promote-override">Override reason (superadmins, optional)</label>
            <input id="promote-override" class="input" style="width:100%;" placeholder="Required only if bypassing a failed or pending test gate">
          </div>
        `,
        confirmText: 'Promote and seal passport',
        onConfirm: async () => {
          const override = $('#promote-override').value.trim();
          try {
            const res = await api('POST', `/api/apiops/deployments/${deploymentID}/promote`, {
              override_reason: override
            });
            toast('Deployment successfully promoted and sealed');
            modal.close();
            if (onSuccess) onSuccess();
            if (res.passport) {
              openPassportModal(deploymentID);
            }
          } catch (err) {
            toast(`Promotion failed: ${err.message}`, 'alert');
            return false;
          }
        }
      });
    }

    async function openPassportModal(deploymentID) {
      try {
        const passport = await api('GET', `/api/apiops/deployments/${deploymentID}/passport`);
        const jsonStr = JSON.stringify(passport, null, 2);

        openModal({
          title: `Release Passport · ${passport.commit_sha ? passport.commit_sha.substring(0, 7) : 'Sealed'}`,
          body: `
            <div class="passport-facts">
              <p><strong>Passport ID:</strong> ${esc(passport.id)}</p>
              <p><strong>Deployment:</strong> ${esc(passport.deployment_id)}</p>
              <p><strong>Commit SHA:</strong> ${esc(passport.commit_sha)} (${esc(passport.branch)})</p>
              <p><strong>Target Revision:</strong> rev_${passport.target_revision}</p>
              <p><strong>Plan Hash:</strong> ${esc(passport.plan_hash)}</p>
              <p><strong>Evidence Digest:</strong> ${esc(passport.evidence_digest)}</p>
              <p><strong>Sealed By:</strong> ${esc(passport.sealed_by)}</p>
              <p><strong>Sealed At:</strong> ${fmtDate(passport.sealed_at)}</p>
              ${passport.override_reason ? `<p style="color:#f59e0b;"><strong>Override Reason:</strong> ${esc(passport.override_reason)}</p>` : ''}
            </div>
            <div style="display:flex; gap:10px;">
              <button class="button small" id="btn-copy-passport">Copy JSON</button>
              <button class="button small" id="btn-dl-passport">Download passport</button>
            </div>
          `,
          confirmText: 'Close',
          onConfirm: () => true
        });

        setTimeout(() => {
          const copyBtn = $('#btn-copy-passport');
          const dlBtn = $('#btn-dl-passport');
          if (copyBtn) {
            copyBtn.onclick = () => {
              navigator.clipboard.writeText(jsonStr).then(() => toast('Passport JSON copied to clipboard'));
            };
          }
          if (dlBtn) {
            dlBtn.onclick = () => {
              const blob = new Blob([jsonStr], { type: 'application/json' });
              const url = URL.createObjectURL(blob);
              const a = document.createElement('a');
              a.href = url;
              a.download = `release-passport-${passport.commit_sha ? passport.commit_sha.substring(0, 7) : passport.id}.json`;
              a.click();
              URL.revokeObjectURL(url);
            };
          }
        }, 50);

      } catch (err) {
        toast(`Failed to load passport: ${err.message}`, 'alert');
      }
    }
  };
})();
