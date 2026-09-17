# Cluster task lifetime and display provenance

## Scope

This personal candidate distinguishes a recorded Do entry from an old worker
process's attempt and separates waiting time from the `posted_time` priority
key. It does not change queue ordering, pacing, resource capacity, scratch
deletion policy, or completion callbacks. It does not prove native progress.

### Display

The engine publishes a fresh process session on registration. Preparation
stores that session alongside the existing acquisition generation and attempt
token. Resource registration (including an old binary registering again)
invalidates the previous session; periodic heartbeat does not manufacture one.

Running requires current session identity, token, `do_entry`, a non-future
start, and, for SDR, exactly one not-failed/not-after-SDR pipeline reference.
Previous-process, unavailable-provenance, missing-reference and contradictory
SDR rows remain in Unknown with reasons. Neither Unknown nor a missing SpID is
permission to retire a task. An existing process session is registration/entry
evidence, not a heartbeat-independent proof that native work is progressing.

Classification, counts and selection use one SQL snapshot before LIMIT.
Running still sorts by descending Took across owners and task kinds. The
legacy API `Running` array and `RunningTotal` continue to mean all owned tasks.
`SectionTotals` supplies the explicit execution split. SpID enrichment failure
remains partial/error information, not missing-reference proof.

`WaitingSeconds` uses recorded entry into the unowned queue, including retry.
The migration does not backfill existing rows. New inserts record immutable
creation time; ownership transitions record queue entry. Legacy rows lacking
creation provenance stay unknown, even after a later queue transition. A
priority timestamp before its sector, missing/future timestamps and unknown
or relinked provenance have explicit reasons. No year or maximum age is
hard-coded. The older API `AgeSeconds` is preserved, but the new UI never uses
it as a Waiting fallback. Pause/stale/failed-refresh interpolation is unchanged.

## Narrow automatic SDR retirement

An independent bounded worker loop runs without waiting for admission,
uncordon or a pacing slot. Only a task owned by this machine, from a different
recorded process session, with a complete acquisition/token/execution identity,
can be considered. The current pending/running registry must have no handle.
The Linux personal managed-process boundary must prove the recorded subtree
has ended: same enrolled host/domain/service and different kernel boot, or the
same managed UUID cgroup removed/empty. Replaced inodes, foreign identity,
inspection errors and live descendants refuse retirement. This relies on the
existing managed contract: no reuse/re-entry of UUID groups and no migration
of descendants out of the subtree. Missing cgroup/process metadata is not
reconstructed from timestamps, task history, or a service name.

Retirement locks the exact task and rechecks name, owner, generation, attempt,
session and execution identity, then checks the registry and SDR references.
Any reference, including failed/completed/ambiguous references, protects the
task. Only an unreferenced stopped task is deleted, together with a diagnostic
tombstone in the same transaction. No normal history success, callback or
peer-success notification is generated.

A pipeline insert/relink trigger takes the same task lock and rejects retired
IDs. Thus a reconnect that wins the lock protects the task; retirement that
wins prohibits a future reconnect. This is not a bare NOT EXISTS assumption.
Normal AddTask creates its task and pipeline connection in one transaction.
No unrelated task classes, stage-result writes, files or GC marks are changed.
Serialization/deadlock/inspection failures defer to a later bounded pass.

## Rollout and limits

Requires the additive `20260917-task-lifetime.sql` migration, new workers for
process/queue provenance, and new WebRPC/static UI. It is not a UI-only update.
Existing rows are preserved, not swept in migration. Old workers can retain
their existing SQL paths but provide no new session proof; such attempts are
unknown to the new UI and ineligible for automatic retirement. Keep additive
schema/tombstones if reverting binaries; do not remove them while new workers
or the new API still depend on them. Never reset sequences/reuse retired IDs.

Stage the rollout, rather than treating this as a static-page replacement:

1. An approved new process must finish the ordinary startup migration runner
   before new workers or WebRPC query the additional columns. Preserve the
   migration ledger and existing task rows; do not backfill their provenance.
2. Replace selected workers under the operator's normal drain/restart policy,
   not an unconditional fleet restart. Only attempts prepared by a new worker
   carry its session. An old worker may execute normally while showing Unknown.
   Automatic retirement additionally requires the supported managed Linux
   execution boundary on that worker; a WebRPC upgrade cannot supply it.
3. Deploy the matching WebRPC and static UI together after schema readiness.
   Existing pending rows remain Waiting=unknown without verified creation/queue
   provenance. No fallback to posted_time or fabricated historical timestamps
   is permitted. Unknown is a provenance classification, not proof of failure.
4. If returning to an earlier binary, retain the additive columns, triggers,
   ledger and tombstones. Old registration clears process_session; it must not
   inherit a newer process's identity. SQL compatibility does not establish
   concurrent mixed-version native safety or prove prior executions ended.

In particular, pre-protocol orphan tasks lacking recorded execution identity
remain protected. The observed old tasks cannot be declared ended solely from
restart time, owner absence, age, `n/a`, or a missing pipeline. This candidate
does not retroactively authorize deleting them. Managed cleanup disabled or a
non-Linux worker yields no automatic task-retirement evidence.

## Verification

The integration-tag tests use an explicit disposable loopback database and
the actual migration runner. They cover real AddTask/claim/preparation/SDR
success SQL, startup while cordoned/pacing-blocked, valid recovery, registry
protection, independent-connection reclaim/reconnect races in both orders,
rollback and no invented success. Native termination is a fixture boundary,
not native execution. Existing completion CAS and storage Claim race tests
remain included. The SQL backlog fixture has 32,108 task-linked pending rows;
its plan/size observations are not production or Yugabyte benchmarks.

The Linux kernel fixture additionally exercises the task boundary with live
parent/descendant, subtree exit, replaced inode and foreign host. Cross-building
that fixture is not executing it. Linux/native and Yugabyte execution remain
separate validation gates; consult the external review archive for actual run
results, commands, negative controls and offline browser screenshots.

The follow-up migration fixture runs the real loader from the pre-lifetime
embedded migration set through current startup, restart and return to the old
startup set. It checks legacy row/ledger preservation, NULL provenance and
retained tombstones. The old-worker fixture executes the frozen registration
and preparation SQL plus the unchanged claim/completion functions. This is
sequential SQL compatibility, not an old/new native scheduler fleet test.
Lock observers use PostgreSQL backend dependencies or Yugabyte transaction
UUID dependencies, with a real row-lock barrier rather than advisory locks.
An observer or setup failure is not a successful contention test.
