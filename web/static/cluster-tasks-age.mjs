import {clusterTaskExecutionSection, interpolateClusterTaskAgeSeconds as interpolate} from './cluster-tasks-model.mjs';

export function clusterTaskAge(entry, clock) {
  const section = clusterTaskExecutionSection(entry);
  const pending = section === 'pending';
  if (pending) {
    const age = entry.WaitingState === 'queue-entry' ? interpolate(entry.WaitingSeconds, clock) : null;
    const reason = entry.WaitingState || 'unknown-provenance';
    return {seconds: age, text: age === null ? 'unknown' : null, title: age === null
      ? `Waiting is unconfirmed (${reason}). Posted is a queue-order key, not proof of elapsed waiting.`
      : 'Waiting since the recorded entry into the unowned queue. This can be a retry; previous execution time is excluded.'};
  }
  const age = section === 'running' ? interpolate(entry.TookSeconds, clock) : null;
  if (age !== null) return {seconds:age, text:null, title:'Took since entry into the current task Do attempt; the same start is used by new History records.'};
  if (section === 'awaiting-start') return {seconds:null, text:'—', title:'No current attempt execution start has been confirmed; ownership alone is not execution.'};
  return {seconds:null, text:'unknown', title:entry.ExecutionReason || (entry.TookState === 'future-start' ? 'The recorded start is ahead of the server snapshot; check clock synchronization.' : 'The current attempt start is unknown. Posted, claim and migration times are not used as Took.')};
}
