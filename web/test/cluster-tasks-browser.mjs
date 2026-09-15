// Real Chromium/Lit; every route is intercepted locally. No Curio connection.
// Supply cached PLAYWRIGHT_MODULE, CHROMIUM_EXECUTABLE, LIT3_FIXTURE.
// CLUSTER_BASELINE optionally serves a preserved Git revision for before images.
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {execFileSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import path from 'node:path';
const root=fileURLToPath(new URL('../../',import.meta.url));
const baseline=process.env.CLUSTER_BASELINE;
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
function source(name) {
  return baseline ? execFileSync('git',['show',`${baseline}:web/static${name}`],{cwd:root,encoding:'utf8'})
    : readFileSync(path.join(root,'web/static',name),'utf8');
}
function fixture() {
  const row=(ID,TookSeconds,extra={})=>({ID,Name:'SDR',SpID:'1000',Miners:['f01000'],OwnerID:7,
    Owner:'worker.example:12300',State:'running',TookState:'running',TookSeconds,AgeSeconds:14400,
    AttemptID:`a-${ID}`,OwnershipStartedAt:'2026-09-16T00:00:00Z',...extra});
  return {Running:[row(1,939),row(2,11499),row(3,10925,{Name:'Indexing',OwnerID:8,Owner:'other.example:12300'}),
    row(4,null,{TookState:'awaiting-start'}),row(5,null,{TookState:'future-start'}),
    row(6,39),row(7,2),row(8,0),row(9,1)],
  Pending:[row(10,null,{OwnerID:null,Owner:null,State:'pending',TookState:'pending',AgeSeconds:86400})],
  Applied:{MaxTasks:500,MaxPending:30,IncludeBackground:false,TaskName:null},
  RunningTotal:650,PendingTotal:32108,TotalsAvailable:true,
  SectionTotals:{Running:600,AwaitingStart:30,Unknown:20,Pending:32108},
  ObservedAt:'2026-09-16T04:00:00Z',TaskTypes:[{Name:'SDR'},{Name:'Indexing'}],TaskTypesAvailable:true,Warnings:[]};
}
const browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_EXECUTABLE});
try {
  const page=await browser.newPage({viewport:{width:1000,height:1160}});
  const errors=[],calls=[],held=[];
  page.on('pageerror',e=>errors.push(e.message));
  await page.clock.install({time:new Date('2026-09-16T04:00:00Z')});
  let hold=false;
  await page.route('**/*',async route=>{
    const u=new URL(route.request().url());
    if(u.href.includes('/lit/dist@3/')) return route.fulfill({contentType:'text/javascript',body:readFileSync(process.env.LIT3_FIXTURE,'utf8')});
    if(u.origin!=='http://cluster.invalid') return route.abort();
    if(u.pathname==='/') return route.fulfill({contentType:'text/html',body:`<!doctype html><meta charset="utf-8"><body style="background:#171c24;color:#e6edf3;font:14px sans-serif;padding:16px"><h2>Cluster Tasks</h2><p>Offline synthetic snapshot — no production connection</p><cluster-tasks></cluster-tasks><script type="module" src="/cluster-tasks.mjs"></script></body>`});
    if(u.pathname==='/api/webrpc/v0') {
      const req=route.request().postDataJSON();calls.push(req);
      assert.equal(req.method,'CurioWeb.ClusterTaskSummaryLimited');
      const finish=async(result,error)=>{
        try { await route.fulfill({contentType:'application/json',body:JSON.stringify({jsonrpc:'2.0',id:req.id,...(error?{error:{code:-1,message:error}}:{result})})}); } catch { /* deliberately aborted late response */ }
      };
      if(hold) {held.push(finish);return;}
      return finish(fixture());
    }
    try {return route.fulfill({contentType:u.pathname.endsWith('.css')?'text/css':'text/javascript',body:source(u.pathname)});} catch {return route.abort();}
  });
  await page.goto('http://cluster.invalid');
  await page.waitForFunction(()=>document.querySelector('cluster-tasks')?.viewState.hasSuccessfulLoad);
  const sections=page.locator('cluster-tasks .task-section');
  const ids=key=>page.locator(`cluster-tasks section[aria-labelledby="cluster-tasks-${key}"] tbody tr:not(.similar-row)`).evaluateAll(rows=>rows.map(r=>Number(r.cells[2].textContent.trim())));
  if(baseline) {
    assert.equal(await sections.count(),2);
    assert.deepEqual(await ids('running'),[1,2,3,4,5,6,7,8,9]);
    await page.screenshot({path:process.env.CLUSTER_SCREENSHOT,fullPage:true});
  } else {
    assert.equal(await sections.count(),4);
    assert.deepEqual(await ids('running'),[2,3,1,6,7,9,8]);
    assert.deepEqual(await ids('awaiting-start'),[4]);assert.deepEqual(await ids('unknown'),[5]);
    assert.deepEqual(await sections.locator('.section-summary').allTextContents(),['7 of 600','1 of 30','1 of 20','1 of 32108']);
    const ageCells=page.locator('cluster-tasks section[aria-labelledby="cluster-tasks-running"] tbody .age-column');
    assert.deepEqual(await ageCells.allTextContents(),['3h11m39s','3h2m5s','15m39s','39s','2s','1s','0s']);
    assert.equal(await ageCells.first().evaluate(el=>getComputedStyle(el).textAlign),'start');
    const awaiting=page.locator('cluster-tasks section[aria-labelledby="cluster-tasks-awaiting-start"] tbody td:nth-child(4)');
    assert.equal((await awaiting.innerText()).trim(),'Awaiting start');
    assert.ok(await awaiting.evaluate(el=>el.scrollWidth<=el.clientWidth),'state is not clipped');
    await page.screenshot({path:process.env.CLUSTER_SCREENSHOT,fullPage:true});
    await page.getByText('Coalesce Entries',{exact:true}).click();
    await page.waitForFunction(()=>document.querySelector('cluster-tasks').coalesceEntries);
    assert.deepEqual(await ids('running'),[2,3,1,8],'only adjacent same-owner/type rows coalesce');
    await page.screenshot({path:process.env.CLUSTER_COALESCED_SCREENSHOT,fullPage:true});
    const countBefore=calls.length;
    await page.getByRole('button',{name:'Pause',exact:true}).click();
    const paused=await ageCells.allTextContents();
    await page.clock.runFor(10000);
    assert.deepEqual(await ageCells.allTextContents(),paused);assert.equal(calls.length,countBefore);
    hold=true;
    await page.getByRole('button',{name:'Resume',exact:true}).click();
    await page.waitForFunction(()=>document.querySelector('cluster-tasks').viewState.refreshing);
    await page.waitForTimeout(40);
    await held.shift()(null,'offline failure');
    await page.waitForFunction(()=>document.querySelector('cluster-tasks').viewState.stale);
    assert.deepEqual(await ids('running'),[2,3,1,8]);
    await page.getByRole('button',{name:'Refresh now',exact:true}).click();await page.waitForTimeout(40);
    const retry=fixture();retry.Running.find(r=>r.ID===2).TookSeconds=0;retry.Running.find(r=>r.ID===2).AttemptID='retry';
    await held.shift()(retry);
    await page.waitForFunction(()=>!document.querySelector('cluster-tasks').viewState.refreshing);
    await page.getByText('Coalesce Entries',{exact:true}).click();
    assert.deepEqual(await ids('running'),[3,1,6,7,9,2,8]);
    await page.getByRole('button',{name:'Refresh now',exact:true}).click();await page.waitForTimeout(40);
    const old=fixture();delete old.SectionTotals;await held.shift()(old);
    await page.waitForFunction(()=>!document.querySelector('cluster-tasks').viewState.refreshing);
    assert.match((await sections.locator('.section-summary').allTextContents())[0],/7 shown; total unavailable/);
    await page.getByRole('button',{name:'Refresh now',exact:true}).click();await page.waitForTimeout(40);
    const late=held.shift();await page.getByRole('button',{name:'Pause',exact:true}).click();await late(retry);
    assert.deepEqual(await ids('running'),[2,3,1,6,7,9,8],'late paused response is ignored');
  }
  assert.deepEqual(errors,[]);
  console.log(JSON.stringify({result:'PASS',browser:browser.version(),baseline:baseline||null,calls:calls.map(c=>c.method),screenshot:process.env.CLUSTER_SCREENSHOT,scope:'real Chromium, intercepted offline fixtures; not database or production evidence'},null,2));
} finally {await browser.close();}
