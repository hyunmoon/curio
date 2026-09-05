import assert from 'node:assert/strict';
import test from 'node:test';

import {
  CLUSTER_TASK_DEFAULTS,
  CLUSTER_TASK_ORDER_POLICY,
  PENDING_AGE_TOOLTIP,
  RUNNING_AGE_TOOLTIP,
  UNKNOWN_RUNNING_AGE_TOOLTIP,
  beginClusterTaskRefresh,
  buildClusterTaskRequest,
  buildClusterTaskSections,
  buildClusterTaskTypeOptions,
  completeClusterTaskRefresh,
  createClusterTaskViewState,
  failClusterTaskRefresh,
  formatClusterTaskFreshness,
  formatClusterTaskSectionSummary,
  formatTaskAgeSeconds,
  normalizeClusterTaskResponse,
  parseBoundedInteger,
  shouldRunClusterTaskFreshness,
} from '../static/cluster-tasks-model.mjs';

test('controls use bounded defaults and the limited-RPC request shape', () => {
  assert.deepEqual(CLUSTER_TASK_DEFAULTS, {
    maxTasks: 500,
    maxPending: 30,
    includeBackground: false,
    taskName: '',
    coalesceEntries: false,
  });

  assert.deepEqual(buildClusterTaskRequest(CLUSTER_TASK_DEFAULTS), {
    MaxTasks: 500,
    MaxPending: 30,
    IncludeBackground: false,
    TaskName: null,
  });
  assert.equal(buildClusterTaskRequest({
    ...CLUSTER_TASK_DEFAULTS,
    taskName: 'SDR',
  }).TaskName, 'SDR');
  assert.deepEqual(buildClusterTaskRequest({
    maxTasks: 17,
    maxPending: 0,
    includeBackground: true,
    taskName: 'TreeRC',
  }), {
    MaxTasks: 17,
    MaxPending: 0,
    IncludeBackground: true,
    TaskName: 'TreeRC',
  });

  assert.equal(parseBoundedInteger('1', 1, 500), 1);
  assert.equal(parseBoundedInteger('500', 1, 500), 500);
  assert.equal(parseBoundedInteger('0', 0, 500), 0);
  assert.equal(parseBoundedInteger('', 1, 500), null);
  assert.equal(parseBoundedInteger('1.5', 1, 500), null);
  assert.equal(parseBoundedInteger('0', 1, 500), 1);
  assert.equal(parseBoundedInteger('501', 1, 500), 500);
  assert.equal(parseBoundedInteger('-1', 0, 30), 0);
});

test('task ages preserve second precision and distinguish unknown runtime', () => {
  assert.equal(formatTaskAgeSeconds(0), '0s');
  assert.equal(formatTaskAgeSeconds(42), '42s');
  assert.equal(formatTaskAgeSeconds(5 * 60 + 1), '5m1s');
  assert.equal(formatTaskAgeSeconds(18 * 60 + 39), '18m39s');
  assert.equal(formatTaskAgeSeconds(25 * 60 * 60 + 61), '25h1m1s');
  assert.equal(formatTaskAgeSeconds(null), 'unknown');
  assert.equal(formatTaskAgeSeconds(undefined), 'unknown');
  assert.equal(formatTaskAgeSeconds(-1), 'unknown');
  assert.match(UNKNOWN_RUNNING_AGE_TOOLTIP, /work_start/);
});

test('freshness is relative to the last successful client update', () => {
  assert.equal(formatClusterTaskFreshness(10_000, 10_000), 'just now');
  assert.equal(formatClusterTaskFreshness(15_001, 10_000), '5s ago');
  assert.equal(formatClusterTaskFreshness(80_000, 10_000), '1m10s ago');
  assert.equal(formatClusterTaskFreshness(9_000, 10_000), 'just now');
  assert.equal(formatClusterTaskFreshness(Date.now(), null), '');
});

test('freshness ticks only for a loaded, active, unpaused view', () => {
  const active = {
    isConnected: true,
    viewVisible: true,
    documentVisible: true,
    paused: false,
    hasSuccessfulLoad: true,
  };
  assert.equal(shouldRunClusterTaskFreshness(active), true);

  for (const field of ['isConnected', 'viewVisible', 'documentVisible', 'hasSuccessfulLoad']) {
    assert.equal(shouldRunClusterTaskFreshness({...active, [field]: false}), false, field);
  }
  assert.equal(shouldRunClusterTaskFreshness({...active, paused: true}), false, 'paused');
});

test('section summaries expose shown and total counts', () => {
  assert.equal(formatClusterTaskSectionSummary(12, 40_000, true), '12 of 40000');
  assert.equal(
      formatClusterTaskSectionSummary(12, 0, false),
      '12 shown; total unavailable',
  );
});

test('task-type options preserve an active filter while metadata changes', () => {
  assert.deepEqual(buildClusterTaskTypeOptions('SDR', []), [{Name: 'SDR'}]);
  assert.deepEqual(
      buildClusterTaskTypeOptions('SDR', [
        {Name: 'TreeD'},
        {Name: 'SDR'},
        {Name: 'TreeD'},
      ]).map((entry) => entry.Name),
      ['TreeD', 'SDR'],
  );
  assert.deepEqual(
      buildClusterTaskTypeOptions('', [{Name: 'TreeD'}]),
      [{Name: 'TreeD'}],
  );
});

test('limited response normalization keeps server metadata and safe arrays', () => {
  const normalized = normalizeClusterTaskResponse({
    Running: null,
    Pending: [{ID: 1}],
    Applied: {
      MaxTasks: 50,
      MaxPending: 10,
      IncludeBackground: true,
      TaskName: 'SDR',
    },
    RunningTotal: 7,
    PendingTotal: 100,
    TotalsAvailable: true,
    Partial: true,
    ObservedAt: '2026-09-05T12:00:00Z',
    TaskTypes: [{Name: 'SDR', Total: 107}, null, {Name: 12}],
    TaskTypesAvailable: true,
    Warnings: [{Code: 'partial', Message: 'Partial result'}, {Code: 'bad'}],
  });

  assert.deepEqual(normalized.Running, []);
  assert.deepEqual(normalized.Pending, [{ID: 1}]);
  assert.deepEqual(normalized.Applied, {
    MaxTasks: 50,
    MaxPending: 10,
    IncludeBackground: true,
    TaskName: 'SDR',
  });
  assert.equal(normalized.PendingTotal, 100);
  assert.equal(normalized.TotalsAvailable, true);
  assert.equal(normalized.Partial, true);
  assert.deepEqual(normalized.TaskTypes, [{Name: 'SDR', Total: 107}]);
  assert.deepEqual(normalized.Warnings, [{Code: 'partial', Message: 'Partial result'}]);
});

test('failed refresh preserves the last rows, ages, and last-success timestamp', () => {
  const firstResponse = {
    Running: [{ID: 1, State: 'running', AgeSeconds: 720}],
    Pending: [{ID: 2, State: 'pending', AgeSeconds: 240}],
  };

  let state = createClusterTaskViewState();
  state = beginClusterTaskRefresh(state);
  state = completeClusterTaskRefresh(state, firstResponse, 1234);
  const acceptedResponse = state.response;

  state = beginClusterTaskRefresh(state);
  state = failClusterTaskRefresh(state, new Error('database unavailable'));

  assert.equal(state.response, acceptedResponse);
  assert.equal(state.response.Running[0].AgeSeconds, 720);
  assert.equal(state.response.Pending[0].AgeSeconds, 240);
  assert.equal(state.lastSuccessAt, 1234);
  assert.equal(state.hasSuccessfulLoad, true);
  assert.equal(state.refreshing, false);
  assert.equal(state.stale, true);
  assert.equal(state.error, 'database unavailable');
});

test('first refresh failure remains a first-state error, not stale data', () => {
  let state = createClusterTaskViewState();
  state = beginClusterTaskRefresh(state);
  state = failClusterTaskRefresh(state, new Error('offline'));

  assert.equal(state.response, null);
  assert.equal(state.hasSuccessfulLoad, false);
  assert.equal(state.stale, false);
  assert.equal(state.error, 'offline');
});

test('sections are Running then Pending and coalesce only inside each section', () => {
  const response = {
    Running: [
      {ID: 1, State: 'running', SpID: '1000', Name: 'SDR', OwnerID: '7'},
      {ID: 2, State: 'running', SpID: '1000', Name: 'SDR', OwnerID: '7'},
    ],
    Pending: [
      {ID: 3, State: 'pending', SpID: '1000', Name: 'SDR', OwnerID: null},
      {ID: 4, State: 'pending', SpID: '1000', Name: 'SDR', OwnerID: null},
    ],
  };

  const expanded = buildClusterTaskSections(response, false);
  assert.deepEqual(expanded.map((section) => section.key), ['running', 'pending']);
  assert.deepEqual(expanded[0].groups.map((group) => group.map((entry) => entry.ID)), [[1], [2]]);
  assert.deepEqual(expanded[1].groups.map((group) => group.map((entry) => entry.ID)), [[3], [4]]);

  const coalesced = buildClusterTaskSections(response, true);
  assert.deepEqual(coalesced[0].groups.map((group) => group.map((entry) => entry.ID)), [[1, 2]]);
  assert.deepEqual(coalesced[1].groups.map((group) => group.map((entry) => entry.ID)), [[3, 4]]);
  assert.equal(coalesced[0].ageLabel, 'Runtime');
  assert.equal(coalesced[1].ageLabel, 'Waiting');
  assert.match(RUNNING_AGE_TOOLTIP, /work_start/);
  assert.match(PENDING_AGE_TOOLTIP, /posted/);
  assert.equal(
      CLUSTER_TASK_ORDER_POLICY,
      'Sealing/proof first; longest-running first within each group',
  );
});

test('coalescing preserves the selected-record bound', () => {
  const selected = Array.from({length: 500}, (_, index) => ({
    ID: index + 1,
    State: 'running',
    SpID: '1000',
    Name: 'SDR',
    OwnerID: 7,
  }));

  const sections = buildClusterTaskSections({Running: selected, Pending: []}, true);
  assert.equal(sections[0].groups.length, 1);
  assert.equal(
      sections[0].groups.reduce((count, group) => count + group.length, 0),
      500,
  );
  assert.equal(sections[1].groups.length, 0);
});
