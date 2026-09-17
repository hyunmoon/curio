import assert from 'node:assert/strict';
import test from 'node:test';

import {groupConsecutiveTasks} from '../static/cluster-tasks-grouping.mjs';

test('coalescing preserves chronological order and only groups consecutive tasks', () => {
  const tasks = [
    {ID: 1, State: 'pending', SpID: '1000', Name: 'SDR', OwnerID: null, Age: '27m'},
    {ID: 2, State: 'pending', SpID: '1000', Name: 'SDR', OwnerID: null, Age: '26m'},
    {ID: 3, State: 'running', SpID: '1000', Name: 'SDR', OwnerID: '7', Age: '25m'},
    {ID: 4, State: 'pending', SpID: '1000', Name: 'SDR', OwnerID: null, Age: '24m'},
    {ID: 5, State: 'pending', SpID: '1000', Name: 'SDR', OwnerID: null, Age: '23m'},
  ];

  const groups = groupConsecutiveTasks(tasks);

  assert.deepEqual(groups.map((group) => group.map((task) => task.ID)), [[1, 2], [3], [4, 5]]);
  assert.deepEqual(groups.flat(), tasks);
});

test('coalescing handles empty and single-entry input', () => {
  assert.deepEqual(groupConsecutiveTasks([]), []);

  const task = {ID: 1, SpID: '', Name: 'bg:Example', OwnerID: null};
  assert.deepEqual(groupConsecutiveTasks([task]), [[task]]);
});

test('coalescing does not hide distinct execution or waiting provenance', () => {
  const base = {State: 'running', Name: 'SDR', OwnerID: 7, SpID: ''};
  const rows = ['previous-process', 'unreferenced', 'unknown'].map((TookState, ID) => ({...base, ID, TookState}));
  assert.equal(groupConsecutiveTasks(rows).length, 3);
  assert.equal(groupConsecutiveTasks([
    {...base, State: 'pending', WaitingState: 'queue-entry'},
    {...base, State: 'pending', WaitingState: 'unknown-provenance'},
  ]).length, 2);
});

test('all grouping identity fields form group boundaries', () => {
  const tasks = [
    {ID: 1, State: 'pending', SpID: '1000', Name: 'SDR', OwnerID: null},
    {ID: 2, State: 'pending', SpID: '1001', Name: 'SDR', OwnerID: null},
    {ID: 3, State: 'pending', SpID: '1001', Name: 'TreeD', OwnerID: null},
    {ID: 4, State: 'running', SpID: '1001', Name: 'TreeD', OwnerID: null},
    {ID: 5, State: 'running', SpID: '1001', Name: 'TreeD', OwnerID: '7'},
  ];

  assert.deepEqual(
      groupConsecutiveTasks(tasks).map((group) => group.map((task) => task.ID)),
      [[1], [2], [3], [4], [5]],
  );
});
