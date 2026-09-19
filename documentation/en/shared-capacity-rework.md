# Shared capacity review rework — NOT a runtime replacement

## Boundary

Base checkpoint: `1efd33598c26cb62ad6a59203929995aacd8bc5c`, on personal
`3631dd287043e1bd622c0c44dc23a301970c6cab`. This remains an **incomplete development
candidate**, not an ENOSPC fix ready for installation. No existing Curio caller
uses the authority. Profile arithmetic, slots, pacing, scratch reclamation and
the UI remain byte-identical to the baseline.

The review's sampler bug is fixed. Recoverable ledger requests and continuous
models are implemented. Linux XFS measurement primitives are added, but a fresh
measurement primitive is **not** a complete production sampler. Quota scopes,
all-writer mutation fencing, caller recovery/unwind and activation are still
implementation gaps, not merely missing operator validation.

## Sampler error regression

The supplied review described but did not include `review_regressions_test.go`.
The same two input cases and assertions were reconstructed under that filename;
they are not claimed byte-identical to the reviewer's file. On repository-pinned
dependencies both direct and wrapped `ErrCapacity` callbacks issued a token and
mutated the resident entry before the fix. The saved red log is an assertion
failure, not a compiler/environment failure.

Only an allocator-owned disposition, set after *all* sample validation and
arithmetic, can permit an existing zero-additional-budget follow-up under
physical pressure. Callback errors cannot manufacture this disposition. Tests
cover both the historical model API and the new recoverable API. Validated
pressure still permits already-promised drainage. New requests remain denied.

## Publication and recovery contract

The ledger format is version 2; version 1 is rejected, not silently migrated or
reset. No production ledger exists for either experimental version. Whole-cohort
compatibility must be reviewed before eventual activation.

Use `Apply` and `Resolve`, with a persisted `Request` containing a unique client
identity and monotonically increasing sequence. A client issues only one request
at a time and must resolve it before advancing. The authority retains one latest
receipt per client, including a digest of the complete request and returned
token. Reusing a sequence with a different payload, skipping a sequence, or
replaying an older sequence is rejected. Do not choose a new client identity on
each operation: receipt storage is bounded per client, not globally garbage
collected. The existing 8-MiB ledger limit continues to fail closed.

- `not-applied`: under the lock the identified next request was absent, and this
  call failed before publication. After an uncertain earlier call, resolve first.
- `applied`: a receipt and its state were persisted, or a matching receipt was
  reread and the entire current ledger was republished and synced.
- `unknown`: publication/directory sync/response was uncertain, or the ledger
  could not be locked/read. Cancellation before acquiring the lock does not prove
  that an earlier invocation failed to publish.

Resolution must run after the issuer's outstanding call has finished. It does not
cancel another asynchronous invocation. Replays return the same receipt without
reapplying a transition or rerunning reconciliation proof. A failed resolution
does not erase a grant. There is no SQL repair dependency.

Reserve, Start, Returned, Cancel and Reconcile are tested before write, before
file sync, before rename, after rename, after directory sync and with a lost
response. Separate real OS processes are SIGKILLed after publication; a new
handle resolves the receipt. These are process-failure tests, not power-loss or
XFS durability tests. The lock timeout does not bound kernel sync latency.

An applied Start means **the budget entered running**, not that native execution
started/ended. A caller must retain its exclusive execution identity/lease and
must not launch a second writer merely because Start can be replayed. Cancellation
only refunds tentative, never running, state. Returned requires synchronous end
of all writes. Reconcile requires independent termination and footprint evidence.
Current Curio does not yet implement this client journal/lease integration.

The unkeyed Reserve/Start/etc. methods now exist only in `legacy_model_test.go`
to preserve earlier fixtures and the historical uncertain-outcome observation.
They are not an alternative production API or an unsafe fallback.

## Native source evidence and continuous model

The official `merkletree 0.23.0` crate was obtained with approval; SHA-256 is
`4a0ed8c0ce1e281870da29266398541a0dbab168f5fb5fd36d7ef2bbdbf808a3`, matching the
pinned FFI Cargo.lock. No dependency was upgraded.

Pinned source basis:

- `filecoin-proofs 19.1.0/src/constants.rs`: 32-GiB shape has eight arity-8 base trees.
- `storage-proofs-porep 19.1.0/src/stacked/vanilla/proof.rs`,
  `generate_tree_r_last_cpu`: constructs base trees sequentially.
- `merkletree 0.23.0/src/store/level_cache.rs`, `new_with_config`, `build`,
  `front_truncate`: initially sizes one base-tree file; compaction copies within
  that same file and truncates it, not a second full-sized temporary file.
- GPU R-last writes only cached nodes; the CPU payload bound covers that output.
- Eleven labels, full TreeD, sealed data and TreeC still coexist. Standard type 8
  does not gain synthetic early deletion.

For S=32 GiB and the **default** two discarded rows, the closed tree/label payload
model yields 525,280,252,384 peak bytes and 520,381,305,312 resident bytes.
Six peak payloads plus a fixture margin of 32 GiB are 3,186,041,252,672 bytes.
The continuous fixture adds an explicitly assumed 64-MiB small-file allowance per
sector. These are source-derived **payload** calculations plus named model inputs,
not a verified whole-filesystem/native bound. Metadata, non-default discarded
rows, protected failed attempts, duplicate fetch/copy/COW and all caller behavior
must be included before a production envelope can be enabled.

`TestContinuousRefillModel` uses a 15-second virtual clock for 24 virtual hours.
One completion returns one SDR execution slot; previous sectors independently
remain in TreeD/TreeRC/PreCommit/WaitSeed/PoRep/Finalize/MoveStorage. There is no
batch-drained barrier. The source determines file phases, **not their elapsed
durations**: the fixture explicitly assumes SDR=(slots*cadence)-30s, 2 minutes
each for TreeD/TreeRC/PreCommit/PoRep, 75 minutes WaitSeed, and 1 minute each for
Finalize/MoveStorage. The 44m and 25m45s cadences are model inputs, not config edits.

The model checks 4 and 6+6 occupancy, overlapping refills, multiple completions,
per-event materialized bytes, retained residents, and per-stage/combined stalls
between virtual hour 10 and 17.5. It uses an exact serialized byte oracle, not
FIEMAP. Alternating poll order is an explicit fairness assumption, not a proven
production starvation guarantee. One-sided worker failure/fairness under real
scheduling remains unverified. The earlier 16*S sensitivity still correctly
admits only 3/4 when two residents are present.

The existing two-/four-process role models and last-slot tests are retained as
real subprocess accounting tests. They remain batch-shaped and are not relabelled
as continuous Curio worker execution. Continuous virtual tests and OS subprocess
tests are two separate evidence classes.

## Linux measurements: implemented primitives, not connected sampler

`OpenPhysicalFilesystem` pins an XFS root FD, reads UUID with the read-only XFS
geometry ioctl, and pairs it with host machine identity. Storage ID/path is not
the key. Fresh `Fstatfs` returns Bavail; device, fsid and root inode replacement
are checked. Non-XFS and realtime XFS are unsupported. Host identity cloning,
mount-session fencing, alias-to-one-registry enforcement and reboot recovery are
not completed by this primitive alone.

`ExclusiveFileBytes` reads bounded FIEMAP pages on a pinned regular single-link
file. Sparse logical length is not credit. Shared, delayed, unknown, encoded and
unrecognized extent flags receive zero credit. Changes in size/blocks/timestamps
during observation cause rejection. The filter is used by the Linux syscall
path and tested on the host; Linux-only tests cover real sparse/reflink/root
replacement behavior when explicitly given a disposable XFS root.

ABI sources: [Linux v6.8 FIEMAP](https://github.com/torvalds/linux/blob/v6.8/include/uapi/linux/fiemap.h),
[ioctl constants](https://github.com/torvalds/linux/blob/v6.8/include/uapi/linux/fs.h),
[XFS geometry](https://github.com/torvalds/linux/blob/v6.8/fs/xfs/libxfs/xfs_fs.h).

**An FD, FIEMAP result or unchanged stat is not an immutable-file fence.** A writer
can truncate/unlink/reflink after measurement. A real sampler must prevent that
through grant publication, read credit before fresh free, detect duplicate inode
credit, and enforce quota/MaxStorage separately per scope. No callback returning
nil substitutes for this proof. This rework does not supply that all-writer fence
and therefore does not expose these primitives as a ready admission Sampler.

Linux/amd64 compilation and vet are distinct from Linux execution. XFS syscalls,
bind/remount behavior, native type-8 computation and production are NOT RUN.

## Caller closure inventory — activation remains blocked

All entries below remain **unconnected** in this candidate. This is an explicit
implementation gap, not a list of runtime paths claimed protected. No global
always-deny gate or reduced slot count was substituted for implementation.

| Actual source path / operation | Required capacity boundary |
| --- | --- |
| `lib/ffi/task_storage.go` HasCapacity/Claim/markComplete | Advisory path discovery must not exclude a valid shared grant using old inflated/duplicate accounting. Acquire durable grant before preparation writes, without pacing lock or global scheduler mutex. Pre-entry failures cancel tentative; task release cannot release residency. |
| `lib/paths/local.go` AcquireSector/reserve/stat/reportHealth | One filesystem key across roots/storage IDs; atomic grant at selection; per-root MaxStorage and quota, not borrowed headroom. Health reports remain advisory. |
| `lib/ffi/sdr_funcs.go` storageProvider.AcquireSector | Existing-task and non-task callers both need coverage. Non-task currently fetches before reserve; Finalize allocates no filetype but writes. |
| `lib/ffi/sdr_funcs.go` generateSDR; `lib/paths/sdr_reservation.go` | Unique scratch and published receipt reuse must retain identity, cgroup/FD/termination protections; grant before native/first preparation write; freeze or monotonic-proof for any active label credit. |
| `lib/proof/treed_build.go`; TreeD/TreeRC in `sdr_funcs.go` | Create/truncate/fallocate, reflink->COW, copy fallback, error deletion and C1 checks are writer phases, not merely reservation hints. Follow-up inherits prior promise. |
| `lib/paths/local_prove.go`, remote_prove.go | Standard PoRep reaches FFI directly, not TaskStorage. Native read lifetimes and cache side effects must be checked before releasing/deleting residency. No native termination claim from a canceled HTTP context. |
| `lib/ffi/sdr_funcs.go` Finalize/GenerateUnsealedSector/TruncateAndMoveUnsealed | Move TreeD, truncate, possible cross-filesystem copy, ClearCache, ensureOneCopy. These need source mutation and destination commitments; no early refund at SQL completion. |
| `lib/paths/remote.go` AcquireSector/acquireFromRemote/AcquireInto | Into assumes an existing reservation. Fetch staging, existing destination removal and remote deletion must not bypass grant/mutation rules. |
| `lib/paths/fetch.go` fetch/FetchWithTemp; `lib/tarutil` extraction | Exported path can write without TaskStorage; bounded content and staging/copy/retry residency need coverage before files are created. |
| `lib/paths/local.go` MoveStorage; `lib/paths/util_unix.go` Move | Same storage ID is not filesystem identity. External mv may copy; preserve source until completion, budget destination before first copy. |
| `lib/paths/local.go` Remove/RemoveCopies/openPath stash removal | Invalidate/re-measure credited materialization before deletion. Open-unlinked bytes do not equal returned disk space. Existing deletion authorization stays separate. |
| `lib/paths/local_stash.go` StashCreate/ServeAndRemove/StashRemove | Additional writer missed by the original short inventory: selects a sealing root from advisory stats and writes directly. Must budget or reject that managed root before writing; not silently allow it. |
| `lib/piecestore/store.go`, io.go; `lib/ffi/piece_funcs.go` | Piece/upload temporary writes and rename reach storage independently. Use separate bounded grant or explicitly reject on managed roots, retaining other roots. |
| `lib/ffi/unseal_funcs.go`, snap_funcs.go, FTKey callers | Unseal/Snap/update/vanilla-cache writers require their own proven budgets or managed-root rejection. Do not assume a standard type-8 SDR envelope covers them. |
| `lib/sdrscratch`, `lib/paths/sdr_startup.go` and sdr_auto_state.go | Cleanup stays independent of new SDR admission. Preserve termination/gate/cgroup/space-debt checks; coordinate credit invalidation and reconcile only after observed cleanup. |
| `harmony/harmonytask` Claim/admission/start reservation | WAIT_CAPACITY must unwind CPU/RAM/slot/tentative grant without charging SDR retry or pacing. No fsync or directory walk under pacing start mutex. No integration tests for this new protocol yet. |

There is currently no supported-writer activation set. In particular, enabling
only Local.Reserve would miss Finalize, stash and direct fetch. Implementing such
an incomplete hook would create a false safety boundary, so no partial runtime
activation was added.

## Required next implementation and rollout gates

1. Establish the generation/frozen-file protocol at the actual mutation sites
   above, including cleanup and same-filesystem copies. Resolve active-label
   credit without counting open/delalloc/shared bytes as safe materialization.
2. Connect one registry/session per filesystem, quota and MaxStorage scopes,
   persisted request client recovery and before-write gates. Explicitly reject
   unsupported managed-root writers before I/O, without disabling other roots.
3. Prove actual Claim/Acquire/Do/fetch/Finalize/Move cancellation and handoff using
   caller tests; close full native bounds including retries/copies/metadata.
4. Execute Linux/XFS/native tests in a disposable environment. Existing absence
   of such an environment has not been probed repeatedly or replaced by access
   to operational storage.
5. Only after implementation review, prepare whole-filesystem cohort bootstrap:
   all two/four participants upgraded together, no legacy fake-cap writer, real
   writer drain and complete residency census before initialization. Keep the
   original profiles/slots/pacing unless separately approved. A partial-member
   rollout or rollback is unsafe. On rollback drain the entire cohort and preserve
   ledger/resident files; never delete ledger to manufacture capacity.

The saved review archive contains exact source identities, test commands/results,
the red regression, new validation logs, source checksums and a restorable bundle.
Package/model PASS is not Curio runtime replacement PASS.
