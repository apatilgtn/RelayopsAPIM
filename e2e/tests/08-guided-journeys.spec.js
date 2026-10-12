const {test,expect,signIn,eventually,unique,upstreamURL}=require('../fixtures');

for(const approval of [false,true]) test(`get connected: ${approval?'approval':'immediate'} access, one-time key and gateway proof`,async({page,admin,gateway})=>{
 const id=unique(),base=`/connected-${id}`;
 const api=await(await admin.post('/api/apis',{data:{name:`Connected ${id}`,base_path:base,upstream_url:upstreamURL,auth_type:'api_key',require_approval:approval,openapi_spec:{openapi:'3.0.3',info:{title:'Orders API',version:'1'},paths:{'/items':{get:{responses:{200:{description:'OK'}}}}}}}})).json();
 await page.goto(`/portal#connect/${api.id}?step=4`);
 await expect(page.locator('#connect-register')).toBeVisible();
 await page.locator('#connect-name').fill(`Storefront ${id}`);await page.locator('#connect-email').fill(`pilot-${id}@example.test`);
 await page.locator('#connect-register input[type=checkbox]').check();await page.getByRole('button',{name:'Register and request access'}).click();
 await expect(page.locator('#connect-key')).toBeVisible();const key=await page.locator('#connect-key').inputValue();expect(key).toMatch(/^rk_/);
 expect(await page.evaluate(()=>JSON.stringify(localStorage))).not.toContain(key);
 if(approval){
  await expect(page.locator('#view-connect h2')).toHaveText('Awaiting approval');
  const subs=await(await admin.get('/api/subscriptions')).json();const sub=subs.find(s=>s.api_id===api.id);
  await admin.post(`/api/subscriptions/${sub.id}/approve`);
  await page.getByRole('button',{name:'Refresh access status'}).click();
 }
 await expect(page.locator('#view-connect h2')).toHaveText('Access ready');
 await page.getByRole('link',{name:'Continue to first request'}).click();await page.getByRole('link',{name:'Prepare request'}).click();
 await expect(page.locator('#try-path')).toHaveValue(`${base}/items`);
 await eventually(async()=>expect((await gateway.get(`${base}/items`,{headers:{'X-API-Key':key}})).status()).toBe(200));
 await page.locator('#try-send').click();await expect(page.locator('#response-status')).toHaveText('200');
 await page.getByRole('link',{name:'Return to connection progress'}).click();await expect(page.locator('#view-connect h2')).toHaveText('You are connected');
 await page.setViewportSize({width:390,height:844});await page.screenshot({path:`test-results/consumer-${approval?'approval':'immediate'}-mobile.png`,fullPage:true});
 await page.reload();await expect(page.locator('#view-connect')).toContainText('Application not identified');
 await page.locator('#connect-existing').fill(key);await page.getByRole('button',{name:'Load application'}).click();await expect(page.locator('#view-connect h2')).toHaveText('You are connected');
});

test('release workspace: baseline, refresh, canary candidate checks, promotion and sealed evidence',async({page,admin,gateway})=>{
 test.setTimeout(120000);
 const id=unique(),path=`/guided-${id}`;
 const live=await(await admin.post('/api/apis',{data:{name:`Guided ${id}`,base_path:path,upstream_url:upstreamURL,auth_type:'none',rate_limit_per_minute:10000}})).json();
 await eventually(async()=>expect((await gateway.get(path+'/items')).status()).toBe(200));
 const env=await(await admin.post('/api/tests/environments',{data:{name:`Guided ${id}`,gateway_target:'',variables:{},credential_bindings:{}}})).json();
 const suite=await(await admin.post('/api/tests/suites',{data:{name:`Guided ${id}`,definition:{name:'HTTP release check',requests:[{id:'get',name:'Get items',method:'GET',path:path+'/items',assertions:[{type:'status_code',expected:'200'}]}]}}})).json();
 let draft=await(await admin.post('/api/release-drafts',{data:{api_id:live.id}})).json();
 draft.configuration.apis[0].description='Reviewed release change';
 draft=await(await admin.put(`/api/release-drafts/${draft.id}`,{data:{version:draft.version,configuration:draft.configuration,suite_ids:[suite.suite.id],environment_id:env.id}})).json();
 await signIn(page);await page.goto(`/#/release/${draft.id}?step=2`);
 await page.getByRole('button',{name:'Run baseline checks'}).click();
 await eventually(async()=>{const j=await(await admin.get(`/api/release-drafts/${draft.id}/journey`)).json();expect(j.steps[2].state).toBe('passed')},20000);
 await page.locator('#journey-next').click();await expect(page.locator('#journey-title')).toHaveText('Review change');
 await page.locator('#j-review').check();await page.locator('#journey-next').click();await expect(page.locator('#journey-title')).toHaveText('Canary');
 await page.locator('#j-launch-confirm').check();await page.locator('#journey-next').click();await expect(page.locator('#journey-title')).toHaveText('Test');
 await page.reload();await expect(page.locator('#j-candidate')).toBeEnabled();
 await page.locator('#j-candidate').click();
 await eventually(async()=>{const j=await(await admin.get(`/api/release-drafts/${draft.id}/journey`)).json();expect(j.gate_status?.eligible).toBe(true)},20000);
 await admin.post('/api/revisions/auto-rollback/config',{data:{enabled:true,min_requests:1,min_baseline_requests:1,evaluation_window_seconds:300}});
 for(let i=0;i<80;i++) await gateway.get(path+'/items');
 await eventually(async()=>{const j=await(await admin.get(`/api/release-drafts/${draft.id}/journey`)).json();expect(j.can_promote,JSON.stringify(j.blockers)).toBe(true)},30000);
 await page.locator('#journey-next').click();await expect(page.locator('#journey-title')).toHaveText('Release decision');
 await page.screenshot({path:'test-results/release-decision-desktop.png',fullPage:true});
 await page.setViewportSize({width:390,height:844});await page.screenshot({path:'test-results/release-decision-mobile.png',fullPage:true});
 await page.locator('#j-final-review').check();await page.locator('#journey-next').click();await expect(page.locator('#journey-title')).toHaveText('Evidence');
 const evidence=await(await admin.get(`/api/release-drafts/${draft.id}/evidence`)).json();expect(evidence.passport.manifest.gate_report.eligible).toBe(true);expect(evidence.passport.manifest.gate_report.suites[0].evidence_id).toBeTruthy();
 // A second change exercises the blocked decision and explicit abort path.
 let abortDraft=await(await admin.post('/api/release-drafts',{data:{api_id:live.id}})).json();
 abortDraft.configuration.apis[0].description='Change to abort';
 abortDraft=await(await admin.put(`/api/release-drafts/${abortDraft.id}`,{data:{version:abortDraft.version,configuration:abortDraft.configuration,suite_ids:[suite.suite.id],environment_id:env.id}})).json();
 await page.goto(`/#/release/${abortDraft.id}?step=2`);await page.getByRole('button',{name:'Run baseline checks'}).click();
 await eventually(async()=>{const j=await(await admin.get(`/api/release-drafts/${abortDraft.id}/journey`)).json();expect(j.steps[2].state).toBe('passed')},20000);
 await page.locator('#journey-next').click();await page.locator('#j-review').check();await page.locator('#journey-next').click();
 await page.locator('#j-launch-confirm').check();await page.locator('#journey-next').click();
 await expect(page.locator('#journey-title')).toHaveText('Test');
 await page.goto(`/#/release/${abortDraft.id}?step=5`);await expect(page.locator('#journey-title')).toHaveText('Release decision');await expect(page.locator('#journey-next')).toBeDisabled();
 page.once('dialog',dialog=>dialog.accept());await page.locator('#j-abort').click();await expect(page.locator('#journey-title')).toHaveText('Evidence');
 await expect(page.locator('#journey-stage')).toContainText('aborted');

});
