import test from 'node:test';
import assert from 'node:assert/strict';
import {personalTaskAge} from '../static/cluster-tasks-personal.mjs';
import {resetClusterTaskDisplayClock, advanceClusterTaskDisplayClock, freezeClusterTaskDisplayClock} from '../static/cluster-tasks-model.mjs';

test('Took uses only confirmed current-attempt seconds, never Posted or ownership age', () => {
  const task={ID:1,OwnerID:7,AgeSeconds:10800,TookSeconds:600,TookState:'running',AttemptID:'one'};
  const clock=advanceClusterTaskDisplayClock(resetClusterTaskDisplayClock(100),2600);
  assert.equal(personalTaskAge(task,clock).seconds,602);
  for(const state of ['unknown','awaiting-start','future-start']) {
    const value=personalTaskAge({...task,TookState:state},clock);
    assert.equal(value.seconds,null);
    assert.ok(value.text==='unknown'||value.text==='—');
  }
  assert.equal(personalTaskAge({...task,OwnerID:null},clock).seconds,10802);
});

test('new same-owner attempt replaces Took with a lower authoritative baseline', () => {
  const old={ID:1,OwnerID:7,TookState:'running',TookSeconds:600,AttemptID:'old'};
  const running=advanceClusterTaskDisplayClock(resetClusterTaskDisplayClock(100),2500);
  const frozen=freezeClusterTaskDisplayClock(running);
  assert.equal(personalTaskAge(old,advanceClusterTaskDisplayClock(frozen,1e9)).seconds,602);
  assert.equal(personalTaskAge({...old,TookSeconds:1,AttemptID:'new'},resetClusterTaskDisplayClock(1e9)).seconds,1);
});
