import {groupConsecutiveTasks} from './cluster-tasks-grouping.mjs';

export const CLUSTER_TASK_DEFAULTS = Object.freeze({
  maxTasks: 500,
  maxPending: 30,
  includeBackground: false,
  taskName: '',
  coalesceEntries: false,
});

export const CLUSTER_TASK_ORDER_POLICY =
  'Sealing/proof first; longest-running first within each group';
export const RUNNING_AGE_TOOLTIP =
  'Runtime starts at work_start for the current ownership interval.';
export const PENDING_AGE_TOOLTIP =
  'Waiting time starts when the task was posted.';
export const UNKNOWN_RUNNING_AGE_TOOLTIP =
  'Runtime is unknown because work_start is unavailable for this ownership interval.';

export function parseBoundedInteger(value, min, max) {
  const text = String(value).trim();
  if (!/^-?\d+$/.test(text)) {
    return null;
  }

  const parsed = Number(text);
  if (!Number.isSafeInteger(parsed)) {
    return null;
  }
  return Math.min(max, Math.max(min, parsed));
}

export function buildClusterTaskRequest(controls) {
  return {
    MaxTasks: controls.maxTasks,
    MaxPending: controls.maxPending,
    IncludeBackground: controls.includeBackground,
    TaskName: controls.taskName || null,
  };
}

export function formatTaskAgeSeconds(value) {
  if (!Number.isFinite(value) || value < 0) {
    return 'unknown';
  }

  let seconds = Math.floor(value);
  const hours = Math.floor(seconds / 3600);
  seconds %= 3600;
  const minutes = Math.floor(seconds / 60);
  seconds %= 60;

  if (hours > 0) {
    return `${hours}h${minutes}m${seconds}s`;
  }
  if (minutes > 0) {
    return `${minutes}m${seconds}s`;
  }
  return `${seconds}s`;
}

export function formatClusterTaskFreshness(now, updatedAt) {
  if (!Number.isFinite(now) || !Number.isFinite(updatedAt)) {
    return '';
  }

  const elapsedSeconds = Math.max(0, Math.floor((now - updatedAt) / 1000));
  return elapsedSeconds === 0
    ? 'just now'
    : `${formatTaskAgeSeconds(elapsedSeconds)} ago`;
}

export function formatClusterTaskSectionSummary(shown, total, totalsAvailable) {
  return totalsAvailable
    ? `${shown} of ${total}`
    : `${shown} shown; total unavailable`;
}

export function shouldRunClusterTaskFreshness({
  isConnected,
  viewVisible,
  documentVisible,
  paused,
  hasSuccessfulLoad,
}) {
  return isConnected && viewVisible && documentVisible && !paused && hasSuccessfulLoad;
}

export function buildClusterTaskTypeOptions(selectedTaskName, taskTypes) {
  const available = Array.isArray(taskTypes) ? taskTypes : [];
  const seen = new Set();
  const options = [];

  if (selectedTaskName && !available.some((entry) => entry?.Name === selectedTaskName)) {
    options.push({Name: selectedTaskName});
    seen.add(selectedTaskName);
  }

  for (const taskType of available) {
    if (taskType && typeof taskType.Name === 'string' && !seen.has(taskType.Name)) {
      options.push(taskType);
      seen.add(taskType.Name);
    }
  }
  return options;
}

function nonNegativeInteger(value, fallback = 0) {
  return Number.isSafeInteger(value) && value >= 0 ? value : fallback;
}

export function normalizeClusterTaskResponse(value) {
  const response = value && typeof value === 'object' ? value : {};
  const applied = response.Applied && typeof response.Applied === 'object'
    ? response.Applied
    : {};

  return {
    Running: Array.isArray(response.Running) ? response.Running : [],
    Pending: Array.isArray(response.Pending) ? response.Pending : [],
    Applied: {
      MaxTasks: nonNegativeInteger(applied.MaxTasks, CLUSTER_TASK_DEFAULTS.maxTasks),
      MaxPending: nonNegativeInteger(applied.MaxPending, CLUSTER_TASK_DEFAULTS.maxPending),
      IncludeBackground: Boolean(applied.IncludeBackground),
      TaskName: typeof applied.TaskName === 'string' ? applied.TaskName : '',
    },
    RunningTotal: nonNegativeInteger(response.RunningTotal),
    PendingTotal: nonNegativeInteger(response.PendingTotal),
    TotalsAvailable: Boolean(response.TotalsAvailable),
    Partial: Boolean(response.Partial),
    ObservedAt: typeof response.ObservedAt === 'string' ? response.ObservedAt : '',
    TaskTypes: Array.isArray(response.TaskTypes)
      ? response.TaskTypes.filter((entry) => entry && typeof entry.Name === 'string')
      : [],
    TaskTypesAvailable: Boolean(response.TaskTypesAvailable),
    Warnings: Array.isArray(response.Warnings)
      ? response.Warnings.filter((entry) => entry && typeof entry.Message === 'string')
      : [],
  };
}

export function createClusterTaskViewState() {
  return {
    response: null,
    hasSuccessfulLoad: false,
    refreshing: false,
    stale: false,
    error: '',
    lastSuccessAt: null,
  };
}

export function beginClusterTaskRefresh(state) {
  return {
    ...state,
    refreshing: true,
    error: '',
  };
}

export function completeClusterTaskRefresh(state, value, completedAt) {
  return {
    ...state,
    response: normalizeClusterTaskResponse(value),
    hasSuccessfulLoad: true,
    refreshing: false,
    stale: false,
    error: '',
    lastSuccessAt: completedAt,
  };
}

export function failClusterTaskRefresh(state, error) {
  return {
    ...state,
    refreshing: false,
    stale: state.hasSuccessfulLoad,
    error: error?.message || String(error || 'Cluster task refresh failed'),
  };
}

export function cancelClusterTaskRefresh(state) {
  if (!state.refreshing) {
    return state;
  }
  return {
    ...state,
    refreshing: false,
  };
}

function groupsFor(entries, coalesceEntries) {
  return coalesceEntries
    ? groupConsecutiveTasks(entries)
    : entries.map((entry) => [entry]);
}

export function buildClusterTaskSections(response, coalesceEntries) {
  const normalized = normalizeClusterTaskResponse(response);
  return [
    {
      key: 'running',
      title: 'Running',
      ageLabel: 'Runtime',
      ageTooltip: RUNNING_AGE_TOOLTIP,
      entries: normalized.Running,
      groups: groupsFor(normalized.Running, coalesceEntries),
      total: normalized.RunningTotal,
    },
    {
      key: 'pending',
      title: 'Pending preview',
      ageLabel: 'Waiting',
      ageTooltip: PENDING_AGE_TOOLTIP,
      entries: normalized.Pending,
      groups: groupsFor(normalized.Pending, coalesceEntries),
      total: normalized.PendingTotal,
    },
  ];
}
