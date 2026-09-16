# Personal automatic interrupted-SDR discard

This is personal-only behavior behind the existing
`CURIO_PERSONAL_SDR_CLEANUP=1`. It uses the same Curio executable, registered
local `CanSeal` roots and the existing delegated process lifetime. It does not
alter capacity profiles, slots, pacing, task/pipeline rows, GC or sealed files.
Without that option the previous cleanup/admission behavior remains.

## What is removed

| Observation | Action |
|---|---|
| Pre-SDR pipeline, terminated prior execution, exclusive sector access, private layer-only legacy `.tmp` or attempt directory | Unlink the validated files, confirm no open deleted inode, remove the empty directory |
| Same evidence for canonical cache with an incomplete layer layout | Same removal; drop only that local storage ID's cache declaration |
| Current writer observed synchronous native error | Existing immediate cleanup, including when diagnostic xattr/Sync fails |
| Published completion receipt, full legacy layer layout, after-SDR/later completion, metadata/Snap lifecycle | Preserve canonical data |
| Pipeline absent, DB error, unknown files/metadata, unreadable evidence, symlink/hardlink/mount mismatch | Defer that target; do not infer incompleteness |
| Live reader/writer, active recorded run, unconverted Curio accessor | Do not delete |

Canonical key files and sealed files are never adopted for deletion. FTKey
scratch uses `sectors_unseal_pipeline`, not the sealing pipeline. Missing
pipeline rows never authorize deletion. A complete-looking legacy layout is
ambiguous and retained: missing receipts are not failure evidence.

A receipt causes preservation here; its mere presence is **not** certified
success. `GenerateSDR` still validates the full receipt's inputs, proof,
identity and output layout before reusing it. This covers successful publication
followed by a failed or unobserved pipeline update. Successful canonical outputs
are not recomputed merely because their pipeline update has not committed.

## Boundaries and first transition

New workers publish a process-start/cgroup identity capability before opening
storage. Sector gates live on the existing external state filesystem and are
keyed by physical storage identity, storage ID and full miner/sector identity.
SDR and subsequent FFI calls hold shared gates until synchronous I/O returns;
remote fetch send/receive paths also participate. Cleanup takes a nonblocking
exclusive gate. Neither cancellation nor expired DB ownership releases a
native operation's gate. There is no scheduler sleep or host-wide exclusive
lease upgrade.

SDR storage Claim acquires its shared gate nonblockingly, before reservation
and Do-entry. Contention abandons that preparation through the existing
admission cancellation path: no Do failure/history, failure-budget charge or
pacing-start commit occurs. A successful Claim retains the gate through
prepared admission and synchronous SDR I/O; pre-entry cancellation releases
only its reservation. Cleanup therefore cannot take the exclusive gate in
the gap between Claim and native entry.

Other accessors (FFI acquisition, remote fetch, HTTP serving) retry only the
typed lock-contention error, at 25ms intervals for at most 30 seconds or the
caller's earlier deadline. Partial multi-path locks are released before each
wait. Timeout/cancellation returns a classified error, never permission to
ignore a gate; HTTP retains its existing 503 response. Identity, filesystem
and permission errors are not retried as contention. A timeout on these
non-admission accessors does not retroactively refund an SDR Do-entry. The
production SDR Claim gate prevents cleanup contention after that entry.
Once access succeeds, cancellation alone cannot release a live I/O gate.

For metadata-free legacy files, the scanner checks participating live parents,
unconverted Curio executables, registered service subtrees and all managed
subtrees (including children whose parent died before registration). The
existing `curio ffi` subprocess remains in its parent's cgroup. A populated
old/parentless subtree prevents legacy adoption. A recorded active writer must
independently have a stopped subtree; a free directory lock is insufficient.

The initial deployment must replace **all Curio accessors sharing a scratch
root**, including downstream readers/fetch servers, not just the SDR worker.
Do not concurrently start an old, nonparticipating binary or an external tool
against that root. Such a future uncooperative accessor cannot be fenced by
advisory locks or a process census. Curio does not stop services for this
transition. With participating services, ordinary simultaneous/sequential
starts and one-service restarts use the same gates; legacy cleanup retries on
normal preparation and independently during the Local lifecycle. No path list or manual preview/apply is required for
provably incomplete legacy data.

## Admission-independent retry

After storage opens successfully, the existing option also enables a 30-second
Local-context timer over currently registered `CanSeal` roots. No task, Claim,
available CPU/slot, uncordon, or pacing admission is required. Capacity checks
do not run cleanup. Startup, preparation and timer passes use one nonblocking
in-process root guard; different roots run independently. A root still busy at
a tick is skipped, not queued into unbounded concurrent scans.

Each pass has a two-minute context budget and each pipeline observation retains
its 15-second timeout. Shutdown stops new passes and new targets; an unlink
already in progress finishes the existing witness/accounting protocol. These
deadlines do not forcibly interrupt a blocked filesystem syscall. Partial or
transient failures are reevaluated next tick. Completed-stage denials alone are
cached for up to five minutes (bounded entries); cached state can only refuse
removal, never authorize it. Filesystem gates, live state and termination
evidence are required again before any removal. Routine deferral diagnostics
are rate-limited to once per root per 15 minutes; actual unlinks are logged.

After any unlink, including partial cleanup, the root's cached Stat/DU readings
are expired and fresh health is reported to the allocation index. Old in-flight
DU results cannot overwrite the refresh; a slow DU retains the previous usage
as a conservative fallback. Health reporting errors still use the normal
heartbeat retry. Deletion does not imply that all space is available: the
existing open-inode/accounting checks still gate reservation.

This contract requires host-visible procfs/cgroup v2, root, existing
`Type=simple`, `Delegate=yes`, `KillMode=control-group`, `SendSIGKILL=yes`, and
local supported ext4/XFS. It is not a shared-NFS or container-isolated-PID
cleanup protocol. Unknown external/native executables or transferred FDs are
outside the cooperative storage contract. Do not enable this on such a root.

## Stage and space accounting

The current pipeline row is read with `FOR UPDATE` while deciding/removing;
this serializes stage persistence, **not** native execution. Filesystem gates
and process-lifetime checks provide the separate execution boundary. These are
not a cross-resource atomic transaction: if SQL declaration reconciliation
fails after unlink, a stale local declaration can remain until startup's
drop-missing reconciliation. No task/deal/allocation is removed.

Incomplete managed canonical cache is not newly advertised by startup and is
not served to remote consumers without completed-stage evidence. This does
not affect in-place receipt-validated SDR retry reuse.

Every unlink is descriptor-relative with regular-file, link count,
device/mount and inode checks. A durable witness on the separate state
filesystem precedes deletion; scratch xattrs need not be writable. Partial
unlink or process exit is retried from that witness at startup. The empty
legacy/canonical directory is removed only after accounting confirmation, so
it cannot block the next no-replace publication.

`allocatedBytes` is observed allocation of unlinked files, not a promise about
the global free-space delta under concurrent writers. An open deleted inode
or unreadable accounting evidence is `partial_or_space_unconfirmed`, not
reclaimed space. That root's admission remains blocked on unresolved space
accounting. Other healthy roots remain independent. Ordinary unrelated
legacy uncertainty is sector-local and is not reservation credit.

## Validation and remaining gates

Portable tests exercise actual filesystem unlink, independent-process gates,
process death during unlink, open unlinked files, receipt preservation,
replacement, partial failure, roots/miners and ENOSPC witness failure. The
database caller fixture connects real Local startup/preparation/reservation,
real PostgreSQL migration/schema queries and `generateSDR` publication/reuse.
Its native body and Linux host-lifetime evidence are explicitly substituted.
That is not native proofs, Linux cgroup/XFS, or production execution.

The retry fixture opens a real Local with pinned directories, then releases the
startup blocker without invoking Claim. Real temporary file usage and the
existing MaxStorage quota keep HasCapacity false until removal and index-health
refresh. It then checks ordinary Claim/reservation separately. Short timer and
OS-evidence overlays are test-only. Lifecycle fixtures cover transient state
errors, per-root overlap/isolation, partial unlink, denial caching and shutdown.
TaskStorage.Claim now passes the timer goroutine an immutable context argument;
the existing timeout, lock and reservation semantics are unchanged. The
follow-up review reproduces the old race and restores the post-recovery Claim
case to connected race validation. Cleanup-gate barriers exercise actual SDR
acquisition and receipt reuse, cancellation/reservation release, and the
preparation gate lifetime. Native and Linux evidence remain substituted in
those tests; a connected fixture PASS is not native execution evidence.

Build-tagged caller fixtures require `sdr_auto_itest,sdr_retry_itest` and the
review archive's explicit Go overlay. They fail closed without it. Normal
unit/race tests require no database. Linux test binaries may be cross-compiled,
but cross-compilation is not Linux execution. `TestAutoLinuxWorker` is an
additional real Linux boundary fixture for an owned disposable delegated
service; its pipeline state remains synthetic. A real Linux connected
DB/FFI/cgroup test and Yugabyte execution remain separate validation gates.

This does not establish the original ENOSPC cause or guarantee that retained
successful/later-stage files will make a full disk usable. Unknown targets
remain protected rather than silently becoming failures.
