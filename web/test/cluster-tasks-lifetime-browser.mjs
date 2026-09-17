// Actual product modal/CSS and actual SQL fixture response, offline routes only.
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {fileURLToPath} from 'node:url';
import path from 'node:path';
const root=fileURLToPath(new URL('../static/',import.meta.url));
const snapshot=JSON.parse(readFileSync(process.env.CLUSTER_LIFETIME_SNAPSHOT,'utf8'));
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_EXECUTABLE});
try {
  const page=await browser.newPage({viewport:{width:1280,height:1100}});
  const errors=[],calls=[];
  page.on('pageerror',e=>errors.push(e.message));
  await page.routeWebSocket('**/*',ws=>{
    assert.equal(new URL(ws.url()).host,'cluster.invalid');
    ws.onMessage(raw=>{const req=JSON.parse(raw);const data={'CurioWeb.Version':'offline fixture','CurioWeb.UIVariant':'curio','CurioWeb.ClusterMachines':[],'CurioWeb.AlertTotalCount':0};assert.ok(Object.hasOwn(data,req.method));ws.send(JSON.stringify({jsonrpc:'2.0',id:req.id,result:data[req.method]}));});
  });
  await page.route('**/*',async route=>{
    const url=new URL(route.request().url());
    if(url.href.includes('/lit/dist@3/'))return route.fulfill({contentType:'text/javascript',body:readFileSync(process.env.LIT3_FIXTURE,'utf8')});
    if(url.origin!=='http://cluster.invalid')return route.abort();
    if(url.pathname==='/'){
      let html=readFileSync(path.join(root,'pages/tasks/index.html'),'utf8').replace(/<script\b[^>]*>[\s\S]*?<\/script>/g,'');
      html=html.replace('</head>','<link rel="stylesheet" href="/ux/vendor/bootstrap.min.css"><link rel="stylesheet" href="/ux/main.css"></head>');
      html=html.replace('</body>',`<script type="module">import '/cluster-tasks.mjs';import '/ux/components/Drawer.mjs';import '/ux/curio-ux.mjs';</script></body>`);
      return route.fulfill({contentType:'text/html',body:html});
    }
    if(url.pathname==='/api/webrpc/v0'){
      const req=route.request().postDataJSON();calls.push(req.method);assert.equal(req.method,'CurioWeb.ClusterTaskSummaryLimited');
      return route.fulfill({contentType:'application/json',body:JSON.stringify({jsonrpc:'2.0',id:req.id,result:snapshot})});
    }
    try{return route.fulfill({contentType:url.pathname.endsWith('.css')?'text/css':url.pathname.endsWith('.woff2')?'font/woff2':'text/javascript',body:readFileSync(path.join(root,url.pathname))});}catch{return route.abort();}
  });
  await page.goto('http://cluster.invalid');
  await page.waitForFunction(()=>document.querySelector('ui-drawer cluster-tasks')?.viewState.hasSuccessfulLoad);
  const component=page.locator('ui-drawer cluster-tasks');
  const ids=section=>component.locator(`section[aria-labelledby="cluster-tasks-${section}"] tbody tr`).evaluateAll(rows=>rows.map(r=>Number(r.cells[2].textContent.trim())));
  assert.deepEqual(await ids('running'),[4]);assert.deepEqual(await ids('awaiting-start'),[5]);assert.deepEqual(await ids('unknown'),[1,2,3]);assert.deepEqual(await ids('pending'),[6,7]);
  const unknown=component.locator('section[aria-labelledby="cluster-tasks-unknown"] tbody');
  assert.match(await unknown.innerText(),/Previous process/);assert.match(await unknown.innerText(),/Unreferenced/);
  assert.deepEqual(await unknown.locator('.age-column').allTextContents(),['unknown','unknown','unknown']);
  const pending=component.locator('section[aria-labelledby="cluster-tasks-pending"] tbody .age-column');
  assert.equal((await pending.first().innerText()).trim(),'unknown');
  assert.match(await pending.first().getAttribute('title'),/priority-before-sector/);
  assert.doesNotMatch(await component.innerText(),/58836h|43h/);
  const owner=component.locator('section[aria-labelledby="cluster-tasks-running"] tbody td:last-child a');
  assert.equal((await owner.innerText()).trim(),'worker.example:12300');
  assert.ok(await owner.evaluate(a=>a.getBoundingClientRect().right<=a.closest('.table-wrap').getBoundingClientRect().right),'Owner remains visible');
  await page.screenshot({path:process.env.CLUSTER_SCREENSHOT,fullPage:true});
  await page.getByRole('button',{name:'Pause',exact:true}).click();
  const before=await component.locator('.age-column').allTextContents();await page.waitForTimeout(1100);assert.deepEqual(await component.locator('.age-column').allTextContents(),before);
  assert.deepEqual(errors,[]);console.log(JSON.stringify({result:'PASS',browser:browser.version(),calls,snapshot:'actual disposable PostgreSQL response; native termination fixture is mocked; no production connection'},null,2));
}finally{await browser.close();}
