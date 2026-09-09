# Personal task monitoring policy

This layer adds the historical sealing/proof-first display preference to the
bounded task endpoint. It is presentation order, never task scheduling priority.
The 500-row hard bound remains; the default Pending preview is 30 rows and
consecutive coalescing is initially off. Generic ancestry retains neutral age/ID
ordering and its own defaults.

This file describes a personal integration, not a claim of upstream acceptance.

## Live Took

Running rows display `TookSeconds` from the current task's Do-entry timestamp.
The runner captures that timestamp after candidate acceptance, ownership claim,
storage acquisition, goroutine dispatch and sector metadata lookup. The same
timestamp becomes `work_start` in that attempt's new History record. The existing
History end/rounding behavior remains; this is task-attempt duration, not an FFI
kernel benchmark. Existing History rows are never rewritten. The run registry's
earlier dispatch timestamp remains unchanged for preemption accounting.

Each accepted task gets a fresh attempt identity before storage acquisition,
including same-owner recovery. Ownership transitions clear the telemetry. A
conditional write requires the exact task/owner/attempt identity, prepared state,
and NULL start. No backfill, claim timestamp, posted time or old History row is
used as execution start. Pending rows retain a separate Waiting column; claimed
but unconfirmed rows show a dash. Missing provenance or a future timestamp shows
unknown, not fabricated runtime. Existing API age fields retain their meaning.
Worker and database wall clocks must be synchronized: future starts are rejected,
but an arbitrary past worker-clock skew cannot be detected from these fields.

At the one final Do-entry gate, cancellation is checked before an optional start
reservation hook. After a successful hook, the start is captured and Do is invoked
without another cancellation exit. Start persistence runs asynchronously with a
five-second context so DB latency does not delay the start being timed. The writer
is canceled and joined on normal return or panic. A write error leaves live Took
unconfirmed; it does not falsify the execution outcome. Preparation failure releases
the unstarted ownership with the existing CAS rather than invoking Do with stale
telemetry. Preparation and failure-release each have one five-second batch budget.

Between successful snapshots, the existing monotonic display clock estimates Took.
It freezes on pause, hidden/disconnected views or refresh errors, and remains frozen
until a fresh response succeeds. A new attempt may legitimately reduce Took. The
view does not reorder rows on clock ticks or poll every second.

All participating runners must support attempt instrumentation. Older binaries do
not report Do entry, and a same-owner recovery by an old binary without an ownership
write cannot identify a new attempt. Mixed-version recovery is not an instrumentation
guarantee. This change is not authorization to migrate or restart running systems.

Database-free tests exercise the production preparation/entry helpers, deterministic
blocked-writer cleanup, cancellation/reservation ordering, stale identities, timestamp
provenance and the actual response builder. SQL guards and migrations still require
isolated PostgreSQL/Yugabyte execution; test doubles do not establish DB concurrency.
