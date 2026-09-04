import assert from 'node:assert/strict';
import test from 'node:test';

import {groupConsecutiveTasks} from '../static/cluster-tasks-grouping.mjs';

test('coalescing preserves chronological order and only groups consecutive tasks', () => {
  const tasks = [
    {ID: 1, SpID: '1000', Name: 'SDR', OwnerID: null, Age: '27m'},
    {ID: 2, SpID: '1000', Name: 'SDR', OwnerID: null, Age: '26m'},
    {ID: 3, SpID: '1000', Name: 'SDR', OwnerID: '7', Age: '25m'},
    {ID: 4, SpID: '1000', Name: 'SDR', OwnerID: null, Age: '24m'},
    {ID: 5, SpID: '1000', Name: 'SDR', OwnerID: null, Age: '23m'},
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

test('all grouping identity fields form group boundaries', () => {
  const tasks = [
    {ID: 1, SpID: '1000', Name: 'SDR', OwnerID: null},
    {ID: 2, SpID: '1001', Name: 'SDR', OwnerID: null},
    {ID: 3, SpID: '1001', Name: 'TreeD', OwnerID: null},
    {ID: 4, SpID: '1001', Name: 'TreeD', OwnerID: '7'},
  ];

  assert.deepEqual(
      groupConsecutiveTasks(tasks).map((group) => group.map((task) => task.ID)),
      [[1], [2], [3], [4]],
  );
});
