# Worker process provenance for Cluster Tasks

This selective restoration starts from personal `4ae52eea8cfe`. The deployed
Cluster Tasks consumer at `3631dd287043` additionally requires matching
`harmony_task.attempt_session` and `harmony_machines.process_session`.
An owner or a `do_entry` timestamp without this provenance is deliberately
Unknown. This is not evidence that native calculation is running or stalled.

## Restored boundary

- Generate a process-registration UUID once in `resources.RegisterWithResources`.
  Register resources and install the UUID in the same transaction, after the
  existing resource-update reset trigger. Engine construction does not write it
  again. Heartbeats update only `last_contact`.
- Capture this UUID with the existing immutable acquisition-generation map.
  Preparation writes it only under the existing owner/generation/token/start
  predicates, additionally checking the current machine session. An empty
  session used by legacy adapters remains NULL, not invented provenance.
- Keep the existing bounded, asynchronous Do-entry timestamp writer, History
  start time, start-reservation boundary and cleanup CAS unchanged.
  A cancelled/failed timestamp write cannot fabricate Running.
- Preserve the deployed UI's extra SDR reference validity checks. Current
  process provenance does not make failed/completed/unreferenced SDR work valid.

The original provenance commit is `cd4da92377ecc3d97230a6177967f378c6628a7c`.
Its SDR retirement, execution-boundary, cleanup and UI changes are NOT restored.
The original separate late engine-registration write is replaced by atomic
machine registration so delayed engine initialization cannot overwrite a newer
registration. This does not redesign machine identity or allow multiple live
engines to share one host/port registration safely.

## Schema and version compatibility

New migration `20260925-task-process-telemetry.sql` installs only nullable
session/queue-age columns and telemetry triggers. `created_at`/`queued_at` are
dependencies of the deployed UI SELECT, not new scheduling clocks.
Existing rows are never backfilled. `posted_time`, scheduling order, attempts,
in-flight timestamps and previous migration ledger entries are preserved.

On an existing task-lifetime schema, keep its retirement table/reference guard,
`sdr_execution` and lifetime trigger. The same machine-reset trigger name and
semantics are retained. Owner/generation changes invalidate attempt provenance.
Unexpected column types/nullability cause migration failure, not coercion.
Replaying the new SQL after an interrupted ledger write is idempotent.

Supported startup cases: fresh restored worker/schema; pre-restore schema;
schema with the exact historical `20260917-task-lifetime.sql` already applied.
Old workers still run their original SQL, but their new registration clears
process provenance and the newer UI intentionally shows Unknown.

Important sequencing limitation: an untouched old `3631dd28` binary must NOT
initialize/reinitialize a *fresh restored-only* database. Its older non-idempotent
20260917 migration would try to create already-added columns. This restoration
does not fabricate its ledger entry or install retirement policy to bypass that
problem. The currently deployed UI's already-migrated database is the intended
mixed-version case. Preserve its real ledger; do not drop database objects.

## What changes in the UI

| Observed task | Deployed UI classification |
|---|---|
| Unowned | Pending |
| Current session, prepared, no Do-entry timestamp | Awaiting start |
| Current session/token and confirmed Do entry; valid SDR reference if SDR | Running |
| Missing process/attempt session | Unknown |
| Different process session | previous-process (Unknown section) |
| Invalid/missing SDR reference | invalid-reference/unreferenced (Unknown section) |

The repository's older UI remains unchanged and continues consuming its existing
fields. Pipeline Overview is also unchanged: its Preparing/Running counters have
a different classifier. Owned totals are not Running totals. No inference of
native progress, liveness or completion follows from these telemetry states.

Existing unknown attempts gain provenance only through normal real preparation
and Do-entry events (including normal recovery). Do not backfill, reset tasks,
force retry or change ownership to improve display counts. This change does not
promise to normalize a previously observed Preparing backlog.

## Build and operator verification

Build the exact reviewed candidate in a clean Linux checkout using the role's
previously successful options. The ordinary repository target is `make all`;
`make curio` builds Curio alone. The ordinary amd64 target uses **GOAMD64=v3**.
Do not silently substitute `curio-native`, change CUDA/SupraSeal/FFI flags, copy
Darwin native artifacts, or change profiles/slots/pacing. The existing personal
PC1 wrappers under `scripts/personal/` accept explicit `--head`, `--tree`,
`--output` and a verified native receipt; they are unchanged.

Record `git rev-parse HEAD HEAD^{tree}`, `git status --porcelain`,
`git submodule status`, the exact build command and `go version -m ./curio`.
This source change is shared worker code: every worker role whose task telemetry
is to be restored needs the reviewed worker binary, not just SDR. It requires no
new Web UI code for the already deployed 3631 consumer. Building alone is not
authorization to install/restart workers; retain each role's service settings.

After a separately approved rollout, read the existing Cluster Tasks response:
check current `AttemptID`, `TookState`, `TookSeconds`, `ExecutionReason` and the
Running/Awaiting start/Unknown/Pending sections for newly admitted work. Observe
normal completion, History and next-work acceptance; do not use heartbeat alone.
Missing provenance stays Unknown. Do not inject failures on production.

For an operator-run read-only database spot check (no fallback to public):

```sh
ysql_curio <<'SQL'
\set ON_ERROR_STOP on
BEGIN READ ONLY;
SELECT entry,count(*) FROM curio.base
WHERE entry IN ('20260917','20260925') GROUP BY entry ORDER BY entry;
SELECT t.id,t.name,t.owner_id,t.owner_generation,t.attempt_id,
       t.attempt_start_source,t.attempt_started_at,
       t.attempt_session=m.process_session AS same_process
FROM curio.harmony_task t
LEFT JOIN curio.harmony_machines m ON m.id=t.owner_id
ORDER BY t.id DESC LIMIT 30;
COMMIT;
SQL
```

The sample is provenance inspection, not the UI's full SDR-reference classifier
and not proof of native execution. No SQL here changes state.

## Validation scope

Integration tests use explicit disposable loopback targets and the pinned real
HarmonyQuery migration runner. They cover fresh/old/lifetime schemas, restart,
incompatible-column rejection, unchanged in-flight rows/ledger, legacy SQL,
registration/session replacement, owner/generation/token fencing and the exact
deployed UI classification SQL. The Do body is substituted, not native SDR.
The optional `CURIO_TELEMETRY_BASELINE_RED=1` test executes the exact baseline
preparation SQL and intentionally fails the same Running assertion as the fix.
Frozen sources in testdata are not production migrations or duplicate runtime.

PostgreSQL execution, race tests, frontend unit tests, native builds and Yugabyte
execution are separate evidence classes. See the accompanying review package for
actual commands/results and environment limitations; no production test is implied.
