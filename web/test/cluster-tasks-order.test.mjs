import assert from 'node:assert/strict';
import test from 'node:test';
import {buildClusterTaskSections, clusterTaskSectionEmptyMessage, completeClusterTaskRefresh,
  createClusterTaskViewState, failClusterTaskRefresh} from '../static/cluster-tasks-model.mjs';
import {clusterTaskAge} from '../static/cluster-tasks-age.mjs';

const run = (ID, TookSeconds, extra = {}) => ({ID, OwnerID: 7, Name: 'SDR', State: 'running',
  TookState: 'running', TookSeconds, AttemptID: `a-${ID}`, ...extra});
test('Running uses global numeric Took order, not type, owner or old claim age', () => {
  const rows = [run(1,939,{AgeSeconds:14400}), run(2,11499,{AgeSeconds:12000}),
    run(3,10925,{Name:'Indexing',OwnerID:8}), run(9,0),run(8,2),run(7,39),run(6,39)];
  const before = structuredClone(rows);
  const sections = buildClusterTaskSections({Running:rows}, false);
  assert.deepEqual(sections[0].entries.map(r=>r.ID),[2,3,1,6,7,8,9]);
  assert.deepEqual(rows,before,'snapshot must not be mutated');
});

test('new totals are full-set counts; absent or malformed totals are not zero', () => {
  const base = {Running:[run(1,5),run(2,null,{TookState:'awaiting-start'}),run(3,null,{TookState:'unknown'})],
    Pending:[], RunningTotal:700,PendingTotal:32108,TotalsAvailable:true,
    Applied:{MaxTasks:3,MaxPending:30},SectionTotals:{Running:600,AwaitingStart:70,Unknown:30,Pending:32108}};
  const sections=buildClusterTaskSections(base,false);
  assert.deepEqual(sections.map(s=>s.total),[600,70,30,32108]);
  assert.deepEqual(sections.map(s=>s.entries.length),[1,1,1,0]);
  assert.match(clusterTaskSectionEmptyMessage('pending',base),/owned tasks use the display limit/);
  for (const SectionTotals of [undefined,null,{Running:600}, {...base.SectionTotals,Unknown:-1}]) {
    const old=buildClusterTaskSections({...base,SectionTotals},false);
    assert.deepEqual(old.map(s=>s.totalsAvailable),[false,false,false,true]);
    assert.equal(old[0].total,undefined);
  }
  const empty={Running:[],Pending:[],TotalsAvailable:true,PendingTotal:0,SectionTotals:{Running:0,AwaitingStart:0,Unknown:0,Pending:0}};
  for(const key of ['running','awaiting-start','unknown','pending']) assert.match(clusterTaskSectionEmptyMessage(key,empty),/No .* tasks match/);
  assert.match(clusterTaskSectionEmptyMessage('awaiting-start',{...empty,SectionTotals:{...empty.SectionTotals,AwaitingStart:9}}),/earlier sections use the display limit/);
});

test('retry resets sorting, while stale snapshots reuse their stable split and order', () => {
  const first={Running:[run(1,10800),run(2,900)],Partial:true};
  let state=completeClusterTaskRefresh(createClusterTaskViewState(),first,1);
  const sections=buildClusterTaskSections(state.response,false);
  assert.equal(buildClusterTaskSections(state.response,false),sections,'no per-second re-sort');
  state=failClusterTaskRefresh(state,new Error('offline'));
  assert.equal(buildClusterTaskSections(state.response,false),sections);
  state=completeClusterTaskRefresh(state,{Running:[run(1,0,{AttemptID:'retry'}),run(2,901)]},2);
  assert.deepEqual(buildClusterTaskSections(state.response,false)[0].entries.map(r=>r.ID),[2,1]);
});

test('awaiting/unknown sort by ownership timestamp then numeric ID, missing times last', () => {
  const awaiting=(ID,OwnershipStartedAt)=>run(ID,null,{TookState:'awaiting-start',OwnershipStartedAt});
  const sections=buildClusterTaskSections({Running:[awaiting(10,null),awaiting(9,'2026-09-16T00:00:00Z'),
    awaiting(3,'2026-09-15T23:59:59Z'),awaiting(2,'2026-09-15T23:59:59Z')]},false);
  assert.deepEqual(sections[1].entries.map(r=>r.ID),[2,3,9,10]);
  for(const row of sections[1].entries) assert.equal(clusterTaskAge(row,{}).text,'—');
  const precise=buildClusterTaskSections({Running:[awaiting(2,'2026-09-16T00:00:00.000200Z'),
    awaiting(10,'2026-09-16T09:00:00.000100+09:00')]},false);
  assert.deepEqual(precise[1].entries.map(r=>r.ID),[10,2],'SQL microsecond ordering must not collapse to millisecond ties');
});

test('invalid current attempt never has a Running label or numeric Took', () => {
  for(const row of [run(1,0,{AttemptID:''}),run(2,1,{AttemptID:null}),run(3,-1),run(4,5,{TookState:'awaiting-start'})]) {
    assert.equal(buildClusterTaskSections({Running:[row]},false)[2].entries[0],row);
    assert.equal(clusterTaskAge(row,{}).text,'unknown');
  }
});

test('coalescing cannot join nonadjacent same-owner rows across global Took order', () => {
  const sections=buildClusterTaskSections({Running:[run(1,30),run(2,10),run(3,20,{OwnerID:8,Name:'Indexing'})]},true);
  assert.deepEqual(sections[0].groups.map(g=>g.map(r=>r.ID)),[[1],[3],[2]]);
});
test('owned rows split by current execution state; invalid ages never become running', () => {
  const sections = buildClusterTaskSections({Running:[run(1,0),run(2,null,{TookState:'awaiting-start'}),
    run(3,null,{TookState:'unknown'}),run(4,null,{TookState:'future-start'}),run(5,-1),run(6,'39'),
    run(7,NaN),run(8,Infinity),run(9,3,{TookState:'awaiting-start'})],Pending:[{ID:10,OwnerID:null}]},false);
  assert.deepEqual(sections.map(s=>s.key),['running','awaiting-start','unknown','pending']);
  assert.deepEqual(sections.map(s=>s.entries.map(r=>r.ID)),[[1],[2],[3,4,5,6,7,8,9],[10]]);
});
