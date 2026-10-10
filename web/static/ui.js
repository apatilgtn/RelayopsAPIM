/* Shared presentation only. Authentication and API decisions stay in their owners. */
(() => {
  'use strict';
  const names = new Set(['activity','chart-no-axes-combined','logs','network','users','link','layers','server','shield-check','clipboard-check','book-open','terminal','key-round','layout-grid','list','search','refresh-cw','arrow-up-right','arrow-right','log-out','x','check','triangle-alert','git-compare-arrows','upload','download','rocket','git-branch','settings','ellipsis','menu','chevron-down','code-xml','cpu','bot','braces','zap','shield-x','plus','sparkles','arrow-up']);
  const icon = name => names.has(name) ? `<span class="ui-icon" aria-hidden="true" style="--icon:url('/icons/${name}.svg')"></span>` : '';
  let access = null;
  const setAccess = value => { access=value; enhance(document); };
  window.RelayUI = Object.freeze({icon,setAccess,features:()=>access?.features||{}});
  const descriptions={live:'Monitor gateway traffic and request decisions.',analytics:'Review API usage, latency and errors from gateway request logs.',logs:'Search requests and inspect their decision trails.',apis:'Configure routes, authentication and API policies.',consumers:'Manage applications and their API credentials.',subscriptions:'Review API access and assigned plans.',plans:'Define request limits and usage quotas.',fleet:'Review impact, validate changes and release gradually.',approvals:'Review account and API access requests.',audit:'Inspect changes and their administrators.',team:'Manage workspace users and role permissions.',tenants:'Isolate APIs, plans and memberships by workspace.',providers:'Configure OpenID Connect sign-in for administrators.',ai:'Govern models, configure providers, track budgets and qualify releases.',tests:'Create and run end-to-end API test suites with gate policies.',apiops:'Manage declarative API delivery, environments, and Release Passports.',refusals:'Requests the gateway refused by policy, and why.'};
  // Stacked tables on small screens: label each cell with its column.
  function labelTables(root){
    root.querySelectorAll('#view table').forEach(t=>{
      const heads=[...t.querySelectorAll('thead th')].map(th=>th.textContent.trim());
      if(!heads.length)return;
      t.classList.add('stackable');
      t.querySelectorAll('tbody tr').forEach(tr=>{
        let i=0;
        for(const td of tr.children){
          if(!td.hasAttribute('data-label')&&heads[i])td.setAttribute('data-label',heads[i]);
          i+=Number(td.getAttribute('colspan')||1);
        }
      });
    });
  }
  function enhance(root) {
    labelTables(root);
    const view=root.querySelector('#view');
    if(view?.children.length&&!view.querySelector('.product-page-header')){
      const name=location.hash.replace(/^#\//,'').split('/')[0]||'live';
      const title=root.querySelector('#page-title')?.textContent;
      if(title){const header=document.createElement('div');header.className='product-page-header';const copy=document.createElement('div');const h=document.createElement('h1');h.textContent=title;const p=document.createElement('p');p.textContent=descriptions[name]||'';copy.append(h,p);header.append(copy);const actions=document.createElement('div');actions.className='page-actions';
        // A toolbar made only of a few buttons belongs in the page header;
        // toolbars with filters keep their row and give up only the primary action.
        const bar=view.querySelector(':scope>.toolbar');
        const onlyButtons=bar&&!bar.querySelector('input,select,textarea,.filter-group,.search-box')&&bar.querySelectorAll(':scope>.button').length<=4;
        const moved=onlyButtons?[...bar.querySelectorAll(':scope>.button')]:[!view.querySelector('.filter-group')?view.querySelector('.toolbar .button.primary'):null].filter(Boolean);
        moved.sort((a,b)=>a.classList.contains('primary')-b.classList.contains('primary')).forEach(el=>{if(el.classList.contains('primary'))el.classList.remove('small');actions.append(el);});
        if(actions.children.length)header.append(actions);
        view.prepend(header);view.querySelectorAll('.toolbar>.muted:not([id])').forEach(el=>el.remove());
        if(bar&&!bar.querySelector('button,input,select,a,.search-box,.filter-group')&&!bar.textContent.trim())bar.remove();}
    }
    if(view){
      const permissions=access?.permissions||[],role=access?.user?.role;
      const mutable=permissions.includes('write')||permissions.includes('all');
      view.querySelectorAll('[data-edit],[data-del],[data-publish],[data-promote],[data-abort],[data-rollback],[data-canary],[data-approve],[data-reject],[data-revoke],[data-activate],[data-delkey],[data-delsub],[data-toggle],[data-plan],#new-api,#import-openapi,#new-consumer,#new-plan,#btn-apply-config,#btn-auto-rollback,#kpi-auto-rollback,#kpi-gitops').forEach(el=>{
        if(el.id.startsWith('kpi-')){el.setAttribute('aria-disabled',String(!mutable));el.style.pointerEvents=mutable?'':'none';el.style.cursor=mutable?'pointer':'default';}
        else el.hidden=!mutable;
      });
      view.querySelectorAll('[data-account-approve],[data-edituser],[data-deluser],#new-user').forEach(el=>el.hidden=!permissions.includes('users')&&!permissions.includes('all'));
      if(role==='operator')view.querySelectorAll('[data-del]').forEach(el=>{if(['apis','plans'].includes(location.hash.split('/')[1]))el.hidden=true;});
      root.querySelector('[data-view="team"]')?.toggleAttribute('hidden',!permissions.includes('users')&&!permissions.includes('all'));
      root.querySelector('[data-view="audit"]')?.toggleAttribute('hidden',!permissions.includes('audit')&&!permissions.includes('all'));
      const features=access?.features||{};
      root.querySelector('[data-view="ai"]')?.toggleAttribute('hidden',!features.preview_ai);
      root.querySelector('[data-view="apiops"]')?.toggleAttribute('hidden',!features.preview_apiops);
    }
    root.querySelectorAll('[data-icon]:not([data-icon-ready])').forEach(el => {el.innerHTML=icon(el.dataset.icon);el.dataset.iconReady='true';});
    const fieldNames={'api-search':'Search APIs','lf-api':'Filter by API','lf-status':'Filter by response status','lf-q':'Search request logs','an-window':'Analytics time window'};
    Object.entries(fieldNames).forEach(([id,name])=>root.querySelector('#'+id)?.setAttribute('aria-label',name));
    root.querySelectorAll('button.button, a.button, button.text-link').forEach(el => {
      if(el.querySelector('.ui-icon')||(el.closest('.modal')&&/^(new|add|create)/i.test(el.textContent.trim()))) return;
      const text=el.textContent.trim().toLowerCase();
      const name = /^(refresh|reload)/.test(text)?'refresh-cw':/^(export|download)/.test(text)?'download':/^(new|add|create|plan deployment)\b/.test(text)?'plus':/^(apply|import)/.test(text)?'upload':/^compare/.test(text)?'git-compare-arrows':/^promote/.test(text)?'rocket':/^canary/.test(text)?'git-branch':/^sign out/.test(text)?'log-out':/^close$/.test(text)?'x':null;
      if(name) el.insertAdjacentHTML('afterbegin',icon(name));
    });
    root.querySelectorAll('td.actions:not([data-menu-ready])').forEach(cell => {
      const actions=[...cell.children].filter(el=>el.matches('button,a.button'));
      if(actions.length<3)return;
      cell.dataset.menuReady='true';
      const primary=actions.find(el=>el.classList.contains('primary'))||actions[0];
      const menu=document.createElement('details');menu.className='row-menu';
      menu.innerHTML=`<summary aria-label="More actions">${icon('ellipsis')}<span>Actions</span></summary><div class="row-menu-items"></div>`;
      actions.filter(el=>el!==primary).forEach(el=>menu.lastElementChild.appendChild(el));
      cell.appendChild(menu);
      menu.addEventListener('toggle',()=>{if(menu.open){const box=menu.querySelector('summary').getBoundingClientRect(),panel=menu.lastElementChild;panel.style.top=Math.min(box.bottom+4,window.innerHeight-290)+'px';panel.style.left=Math.max(8,Math.min(box.right-200,window.innerWidth-208))+'px';panel.style.width='200px';}});
      menu.addEventListener('click',e=>{if(e.target.closest('button,a'))menu.open=false;});
    });
    root.querySelectorAll('.row-menu').forEach(menu=>{menu.hidden=![...menu.querySelectorAll('.row-menu-items>button,.row-menu-items>a')].some(el=>!el.hidden);});
    root.querySelectorAll('.modal').forEach(dialog => {
      const heading=dialog.querySelector('h2');
      if(heading&&!heading.id){heading.id='dialog-title-'+crypto.randomUUID();dialog.setAttribute('aria-labelledby',heading.id);}
      dialog.querySelectorAll('label:not([for])').forEach((label,i)=>{if(label.querySelector('input,select,textarea'))return;const field=label.parentElement.querySelector('input,select,textarea');if(field&&!label.contains(field)){if(!field.id)field.id=heading.id+'-field-'+i;label.htmlFor=field.id;}});
    });
  }
  document.addEventListener('DOMContentLoaded',()=>{
    enhance(document);
    let queued=false;
    new MutationObserver(()=>{if(!queued){queued=true;queueMicrotask(()=>{queued=false;enhance(document);});}}).observe(document.body,{childList:true,subtree:true});
    const topbar=document.querySelector('.topbar');
    if(topbar){
      const rail=document.querySelector('.rail'),mobile=matchMedia('(max-width:760px)');
      const btn=document.createElement('button');btn.className='button ui-menu-toggle';btn.type='button';btn.setAttribute('aria-label','Open navigation');btn.setAttribute('aria-expanded','false');btn.innerHTML=icon('menu');topbar.prepend(btn);
      const sync=()=>{rail.inert=mobile.matches&&!document.body.classList.contains('nav-open');};
      const close=()=>{document.body.classList.remove('nav-open');btn.setAttribute('aria-expanded','false');btn.setAttribute('aria-label','Open navigation');sync();};
      btn.onclick=()=>{const open=document.body.classList.toggle('nav-open');btn.setAttribute('aria-expanded',String(open));btn.setAttribute('aria-label',open?'Close navigation':'Open navigation');sync();if(open)rail.querySelector('a')?.focus();};
      mobile.addEventListener('change',()=>{close();sync();});sync();
      document.addEventListener('keydown',e=>{
        if(!document.body.classList.contains('nav-open'))return;
        if(e.key==='Escape'){close();btn.focus();}
        if(e.key==='Tab'){const items=[...rail.querySelectorAll('a[href]:not([hidden])'),btn].filter(el=>el.getClientRects().length);const first=items[0],last=items.at(-1);if(e.shiftKey&&document.activeElement===first){e.preventDefault();last.focus();}else if(!e.shiftKey&&document.activeElement===last){e.preventDefault();first.focus();}}
      });
      document.addEventListener('click',e=>{if(!e.target.closest('.rail,.ui-menu-toggle'))close();});rail.querySelectorAll('a').forEach(a=>a.addEventListener('click',()=>setTimeout(close,0)));
    }
    document.addEventListener('keydown',e=>{if(e.key==='Escape')document.querySelectorAll('.row-menu[open]').forEach(menu=>{menu.open=false;menu.querySelector('summary').focus();});});
    document.addEventListener('click',e=>document.querySelectorAll('.row-menu[open]').forEach(menu=>{if(!menu.contains(e.target))menu.open=false;}));
  });
})();
