# Optional per-instance SDR start pacing

This topic is retained only on the personal branch. It is structured for an
independent review, not claimed to be accepted or deployment-approved upstream.

## Contract

`Subsystems.SealSDRMinStartInterval` retains its duration key and zero default.
Zero preserves the existing unpaced batch path, even when jitter is enabled.
Negative intervals reject SDR task construction instead of becoming unlimited.
`Subsystems.SealSDRStartJitter` retains its boolean key and false default.
These fields are read when tasks are constructed; changing them requires restart.

The paced unit is an **SDR task's entry into `Do` on one Curio process**, after
ordinary scheduler eligibility, resource capacity, authoritative SQL ownership,
and storage claim. It is not the start of the native SDR computation, which
occurs later after task-local preparation. It is not a global rate limiter,
sector quota, sealing concurrency limit, or substitute for database ownership.
UnsealSDR and SupraSeal batch tasks are not changed. CPU accounting, FFI backend,
existing minimum-queue and maximum-task settings remain independent.

With a positive interval, at most one provisional start is reserved per instance.
Reservation happens outside speculative/cached `CanAccept`; candidate checks do
not consume the interval. Claim loss/error, storage failure, or cancellation
before `Do` releases only that reservation's token. At `Do` entry the token is
committed and the minimum interval begins. Task errors, panic, or retries after
that entry do not refund it. Stale or repeated cancellation cannot clear another
attempt's reservation. No lock is held for SQL, storage calls, task execution,
logging, or waiting; no scheduler sleep is added.

## Phase and lifecycle

When jitter is enabled, a first start or a start after more than twice the
interval of inactivity waits for the next stable phase in that interval. Phase
is the existing SHA-256 construction, now keyed by `CURIO_NODE_NAME`, a separator,
and the instance's advertised listen identity. The listen identity is required
and distinguishes instances sharing the same host or node name. Keep it stable
across restarts. Changing this identity changes phase; hashes can still collide.
Different phases do not guarantee collision-free starts across the cluster.

Wall time selects the phase once. The resulting wait is latched and measured
with process-local monotonic elapsed time. Repeated polls cannot keep moving
the deadline; wall-clock jumps cannot extend that latched wait or bypass the
minimum interval. Phase waiting holds neither task ownership nor storage.
After phase expiry, insufficient resources or lost claims may delay actual
start past the nominal phase; there is no catch-up burst.

| Lifecycle | Behavior |
| --- | --- |
| First start, jitter off | Immediately eligible after ordinary prerequisites. |
| First start, jitter on | Wait at most one interval for the stable phase. |
| Continuous work / retry | Minimum interval measured from the previous `Do` entry. |
| Long idle | Re-latch one phase wait; do not accumulate missed slots. |
| Claim or storage failure | Cancel provisional reservation; no interval charge. |
| Cancellation before `Do` | No interval charge; existing task retry semantics remain. |
| Failure after `Do` starts | Keep interval charge, including pre-native-computation failures. |
| Recovery after restart | Apply the same pacing hook. Existing recovery code disowns work it cannot currently accept, making it discoverable by normal scheduling; no separate bypass or sleep is added. |
| Config change/restart | Construct a fresh pacer with the new interval and stable identity. In-memory previous-start history is not persisted. |
| Graceful shutdown | Already dispatched task contexts retain the existing shutdown contract. No new starts are scheduled once the engine stops scheduling. |

Restart resets the minimum-interval history. Jitter re-phases the first start but
does not promise minimum spacing across two process lifetimes. Fleet-wide or
durable pacing would require a different policy and is outside this topic.

## Evidence and limits

Database-free tests execute the production pacer and scheduler reservation/run
helpers with injected elapsed/wall clocks, cancellation, and a simultaneous
reservation barrier. They include 43m45s and 25m20s as regression inputs, not
defaults or recommendations. The historical implementation's recursive logging
mutex and speculative `CanAccept` consumption are removed: one locked state
transition replaces the double-check/logging path.

The actual scheduler admission body is also exercised with injected ownership
outcomes and storage-claim failures: lost claims, claim errors, cancellation,
storage errors, failed ownership release, and restart recovery all cancel the
reservation without starting work. The production wrapper supplies its existing
SQL methods; call-site assertions additionally protect reserve/claim/storage/Do
ordering. These unit tests do not execute PostgreSQL/Yugabyte or prove distributed
claim behavior. Real scheduler/database integration and role-specific native
builds remain separate validation gates.

The available original source contains the two config keys and their disabled
defaults, but no committed active instance profiles establishing the currently
deployed interval or identity. Those values are a source gap, not inferred from
screenshots or task durations. Existing personal profile values must be supplied
and reviewed before any deployment; no operator values are selected here.
