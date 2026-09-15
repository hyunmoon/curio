// Real product Drawer, call-site markup, Lit and local CSS; RPC is fixture-only.
// No network is allowed past this router. CLUSTER_BASELINE serves an old tree.
import assert from 'node:assert/strict';
import {readFileSync, mkdirSync, writeFileSync} from 'node:fs';
import {execFileSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import path from 'node:path';

const root = fileURLToPath(new URL('../../', import.meta.url));
const baseline = process.env.CLUSTER_BASELINE;
const output = process.env.CLUSTER_OWNER_OUTPUT;
assert.ok(output, 'CLUSTER_OWNER_OUTPUT is required');
mkdirSync(output, {recursive: true});
const {chromium} = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const source = (name, binary = false) => baseline
  ? execFileSync('git', ['show', `${baseline}:web/static${name}`], {cwd: root, encoding: binary ? undefined : 'utf8'})
  : readFileSync(path.join(root, 'web/static', name), binary ? undefined : 'utf8');
const ipv4 = ['10.10.35.121:12300', '10.10.35.5:12311', '255.255.255.255:65535'];
const extended = [...ipv4, 'a-very-long-worker-instance-name.in-a-long-test-domain.example:65535', '[2001:db8:1234:5678:9abc:def0:1234:5678]:12300'];
function snapshot(owners) {
  let id = 1;
  const row = (owner, extra = {}) => ({ID: id++, Name: 'SDR', SpID: '1000', Miners: ['f01000'],
    OwnerID: 7 + extended.indexOf(owner), Owner: owner, State: 'running', TookState: 'running', TookSeconds: 3600,
    AgeSeconds: 5000, AttemptID: 'fixture-attempt', ...extra});
  const Running = owners.flatMap(owner => Array.from({length: 4}, () => row(owner)));
  Running.push(...owners.map(owner => row(owner, {TookState: 'awaiting-start', TookSeconds: null})),
    ...owners.map(owner => row(owner, {TookState: 'future-start', TookSeconds: null})));
  return {Running, Pending: [row(null, {OwnerID: null, State: 'pending', TookState: 'pending', TookSeconds: null})],
    Applied: {MaxTasks: 500, MaxPending: 30, IncludeBackground: false, TaskName: null},
    RunningTotal: Running.length, PendingTotal: 1, TotalsAvailable: true,
    SectionTotals: {Running: owners.length * 4, AwaitingStart: owners.length, Unknown: owners.length, Pending: 1},
    ObservedAt: '2026-09-16T04:00:00Z', TaskTypes: [{Name: 'SDR'}], TaskTypesAvailable: true, Warnings: []};
}

const browser = await chromium.launch({headless: true, executablePath: process.env.CHROMIUM_EXECUTABLE});
const measurements = [];
try {
  for (const layout of ['overlay', 'push']) {
    for (const width of [1280, 640, 390]) {
      const page = await browser.newPage({viewport: {width, height: 1050}});
      await page.clock.install({time: new Date('2026-09-16T04:00:00Z')});
      const errors = [], served = new Set(), calls = [];
      page.on('pageerror', e => errors.push(e.message));
      let owners = ipv4;
      await page.routeWebSocket('**/*', ws => {
        assert.equal(new URL(ws.url()).host, 'cluster.invalid');
        ws.onMessage(raw => {
          const req = JSON.parse(raw);
          const results = {'CurioWeb.Version': 'offline fixture', 'CurioWeb.UIVariant': 'curio',
            'CurioWeb.ClusterMachines': [], 'CurioWeb.AlertTotalCount': 0};
          assert.ok(Object.hasOwn(results, req.method), `unexpected shell RPC: ${req.method}`);
          ws.send(JSON.stringify({jsonrpc: '2.0', id: req.id, result: results[req.method]}));
        });
      });
      await page.route('**/*', async route => {
        const url = new URL(route.request().url());
        if (url.href.includes('/lit/dist@3/')) return route.fulfill({contentType: 'text/javascript', body: readFileSync(process.env.LIT3_FIXTURE, 'utf8')});
        if (url.origin !== 'http://cluster.invalid') return route.abort();
        if (url.pathname === '/') {
          // Preserve actual drawer/parent markup and page styles, not a standalone table.
          // Unrelated page components/boot are omitted; the real navigation shell is loaded.
          let html = source(layout === 'push' ? '/index.html' : '/pages/tasks/index.html');
          html = html.replace(/<script\b[^>]*>[\s\S]*?<\/script>/g, '');
          html = html.replace('</head>', `<link rel="stylesheet" href="/ux/vendor/bootstrap.min.css"><link rel="stylesheet" href="/ux/main.css"></head>`);
          html = html.replace('</body>', `<script type="module">import '/cluster-tasks.mjs'; import '/ux/components/Drawer.mjs'; import '/ux/curio-ux.mjs';</script></body>`);
          return route.fulfill({contentType: 'text/html', body: html});
        }
        if (url.pathname === '/api/webrpc/v0') {
          const req = route.request().postDataJSON();
          assert.equal(req.method, 'CurioWeb.ClusterTaskSummaryLimited');
          calls.push(req.method);
          return route.fulfill({contentType: 'application/json', body: JSON.stringify({jsonrpc: '2.0', id: req.id, result: snapshot(owners)})});
        }
        try {
          const body = source(url.pathname, url.pathname.endsWith('.woff2'));
          served.add(url.pathname);
          return route.fulfill({contentType: url.pathname.endsWith('.css') ? 'text/css' : url.pathname.endsWith('.woff2') ? 'font/woff2' : url.pathname.endsWith('.svg') ? 'image/svg+xml' : 'text/javascript', body});
        } catch (e) { errors.push(`asset failed ${url.pathname}: ${e.message}`); return route.abort(); }
      });
      await page.goto('http://cluster.invalid');
      const component = page.locator('ui-drawer cluster-tasks');
      await component.evaluate(async el => { await el.updateComplete; });
      await page.waitForFunction(() => document.querySelector('ui-drawer cluster-tasks')?.viewState.hasSuccessfulLoad);
      await component.locator('link[rel=stylesheet]').evaluateAll(async links => {
        await Promise.all(links.map(l => l.sheet ? Promise.resolve() : new Promise((resolve, reject) => {
          l.addEventListener('load', resolve, {once: true}); l.addEventListener('error', reject, {once: true});
        })));
      });
      assert.ok(served.has('/ux/vendor/bootstrap.min.css') && served.has('/ux/main.css'));
      await page.evaluate(async () => { await document.fonts.ready; });
      assert.ok(await page.evaluate(() => [...document.fonts].every(font => font.status === 'loaded')), 'real local fonts loaded');
      const geometry = async phase => {
        const info = await component.evaluate(el => {
          const rect = e => {const r = e.getBoundingClientRect(); return {left: r.left, right: r.right, top: r.top, bottom: r.bottom, width: r.width, height: r.height};};
          const style = e => {const s = getComputedStyle(e); return {width: s.width, minWidth: s.minWidth, maxWidth: s.maxWidth, overflowX: s.overflowX, tableLayout: s.tableLayout, whiteSpace: s.whiteSpace, boxSizing: s.boxSizing};};
          const dialog = el.closest('ui-drawer').shadowRoot.querySelector('dialog');
          return {viewport: innerWidth, pageWidth: document.documentElement.scrollWidth, dialog: rect(dialog), dialogStyle: style(dialog),
            close: rect(dialog.querySelector('.close-btn')), controls: [...el.shadowRoot.querySelectorAll('.controls input, .controls select, .controls button')].map(rect), sections: [...el.shadowRoot.querySelectorAll('.task-section')].map(section => {
              const wrap = section.querySelector('.table-wrap'), table = section.querySelector('table');
              return {title: section.querySelector('h3').textContent, wrap: rect(wrap), scrollWidth: wrap.scrollWidth, clientWidth: wrap.clientWidth, table: rect(table), tableStyle: style(table),
                owners: [...section.querySelectorAll('tbody tr:not(.similar-row) td:last-child')].map(td => ({cell: rect(td), style: style(td), text: td.textContent.trim(),
                  link: td.querySelector('a') ? rect(td.querySelector('a')) : null, href: td.querySelector('a')?.getAttribute('href')}))};
            })};
        });
        measurements.push({layout, width, phase, ...info});
        return info;
      };
      const info = await geometry('ipv4');
      await page.screenshot({path: path.join(output, `${layout}-${width}-ipv4.png`)});
      if (baseline) {
        // Same concrete acceptance assertion as below, deliberately expected to fail.
        assert.throws(() => assert.ok(info.sections.every(s => s.owners.every(o => !o.link || o.link.right <= s.wrap.right)), 'all IPv4 owners visible without horizontal scroll'), /IPv4 owners/);
        await page.close();
        continue;
      }
      assert.equal(info.sections.length, 4);
      assert.ok(info.dialog.left >= 0 && info.dialog.right <= width + 1, 'dialog fits viewport');
      assert.ok(info.close.right <= width && info.close.left >= 0, 'close control remains visible');
      assert.ok(info.pageWidth <= width, 'no page-level horizontal overflow');
      assert.ok(info.controls.every(r => r.left >= info.dialog.left && r.right <= info.dialog.right), 'controls stay inside dialog');
      if (width >= 1280) assert.ok(info.sections.every(s => s.owners.every(o => !o.link || o.link.right <= s.wrap.right)), 'all IPv4 owners visible without horizontal scroll');
      for (const section of info.sections) for (const owner of section.owners) {
        if (!owner.link) {assert.equal(owner.text, ''); continue;}
        assert.ok(ipv4.includes(owner.text));
        assert.equal(owner.href, `/pages/node_info/?id=${7 + extended.indexOf(owner.text)}`);
        assert.ok(owner.link.left >= owner.cell.left && owner.link.right <= owner.cell.right + 1, 'link fits its own cell');
        assert.equal(owner.style.whiteSpace, 'nowrap');
      }
      for (const wrap of await component.locator('.table-wrap').all()) {
        await wrap.evaluate(e => { e.scrollLeft = e.scrollWidth; });
        const visible = await wrap.evaluate(e => {
          const area = e.getBoundingClientRect();
          return [...e.querySelectorAll('tbody td:last-child a')].every(a => {
            const r = a.getBoundingClientRect(); return r.left >= area.left && r.right <= area.right + 1;
          });
        });
        assert.ok(visible, 'every IPv4 owner is fully visible after table scrolling');
      }
      await page.screenshot({path: path.join(output, `${layout}-${width}-ipv4-scrolled.png`)});
      await component.locator('.table-wrap').evaluateAll(wraps => wraps.forEach(e => { e.scrollLeft = 0; }));
      if (width === 1280) {
        for (const section of await component.locator('.task-section').all()) {
          await section.scrollIntoViewIfNeeded();
          const name = await section.getAttribute('aria-labelledby');
          await page.screenshot({path: path.join(output, `${layout}-${width}-${name}.png`)});
        }
        await page.locator('ui-drawer dialog').evaluate(el => { el.scrollTop = 0; });
      }
      owners = extended;
      await component.getByRole('button', {name: 'Refresh now', exact: true}).click();
      await page.waitForFunction(owner => document.querySelector('ui-drawer cluster-tasks')?.viewState.response.Running.some(r => r.Owner === owner), extended.at(-1));
      await component.getByText('Coalesce Entries', {exact: true}).click();
      await component.evaluate(async el => { await el.updateComplete; });
      assert.ok(await component.locator('.similar-row td[colspan="6"]').count() > 0);
      const long = await geometry('long-coalesced');
      assert.ok(long.pageWidth <= width);
      for (const section of long.sections) for (const owner of section.owners) {
        if (owner.link) {
          assert.ok(extended.includes(owner.text));
          assert.equal(owner.href, `/pages/node_info/?id=${7 + extended.indexOf(owner.text)}`);
          assert.ok(owner.link.right <= owner.cell.right + 1, 'long address fits cell without overlap');
        }
      }
      for (const wrap of await component.locator('.table-wrap').all()) {
        await wrap.evaluate(e => { e.scrollLeft = e.scrollWidth; });
        const bounds = await wrap.evaluate(e => ({right: e.getBoundingClientRect().right,
          ends: [...e.querySelectorAll('tbody td:last-child a')].map(a => a.getBoundingClientRect().right)}));
        assert.ok(bounds.ends.every(end => end <= bounds.right + 1), 'scroll reaches full link end');
      }
      await page.screenshot({path: path.join(output, `${layout}-${width}-long-scrolled.png`)});
      await geometry('long-scrolled');
      assert.deepEqual(errors, []);
      assert.ok(calls.length > 0);
      await page.close();
    }
  }
  console.log(JSON.stringify({result: baseline ? 'EXPECTED_ASSERTION_FAILURE_OBSERVED' : 'PASS', baseline: baseline || null, browser: browser.version(), cases: measurements.length, output}, null, 2));
} finally {
  writeFileSync(path.join(output, 'geometry.json'), JSON.stringify(measurements, null, 2));
  await browser.close();
}
