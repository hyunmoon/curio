# Shared-filesystem capacity: implementation checkpoint, NOT deployable

Historical checkpoint for `1efd33598c26cb62ad6a59203929995aacd8bc5c`.
For the subsequent review fixes and updated source availability, see
[shared-capacity-rework.md](shared-capacity-rework.md). Claims below about the
missing crate and the original API/test coverage describe that earlier tree,
not the subsequent rework. Neither checkpoint enables runtime admission.

## Status and source

Baseline: personal `3631dd287043e1bd622c0c44dc23a301970c6cab`, tree
`6e9d7b63462aee77135a50392bcdae19908b3084`.

**The requested Curio runtime replacement is NOT COMPLETE.** This checkpoint
implements and tests the durable cross-process accounting component. It is not
imported by any Curio runtime path. The existing virtual profiles, scheduler,
scratch cleanup, native calls and UI are unchanged. Do not deploy this checkpoint
as an ENOSPC fix or describe its model tests as successful Curio admission.

Two prerequisites for a safe runtime connection remain unsatisfied:

1. A filesystem sampler/writer protocol must establish exclusive materialized
   credit across native overwrite/truncate, reflink/COW, copies, retries and
   cleanup. The current cached `Stat`/`DiskUsage` pair cannot do that. Merely
   wrapping `Local.Reserve` with a flock is unsafe; executed negative controls
   demonstrate this limitation of the new accounting component too.
2. The full pinned native peak (particularly the CPU level-cache construction
   and compaction inside `merkletree 0.23.0`) has not been established. That crate
   is absent locally. Its download was blocked by automatic permission review;
   no bypass was attempted. Linux/XFS/native execution is unavailable locally.
   A guessed peak has not been wired into production admission.

Source availability is not the only remaining work. Even with the crate, the
writer adapter, identity discovery, quota/MaxStorage enforcement, automatic
reconciliation and actual caller regressions must be implemented before this is
a usable capacity change. This document is a resumable checkpoint, not a claim
that only an operator test remains.

## Findings in the existing personal code

The intentional virtual-capacity workaround was introduced in
`d3d6e94fc91f153999532bc2680c0842b220093f`. Its four-file diff and the relevant
diffs of `cb4803b73657f9674124343c1e3523cde938045d`,
`2da9907c28e1334f74f138addc192cd1948a6d4e` and
`51d38a04057b7b941d95d3ca8ca35a5280de0251` were inspected. It must not be
recharacterized as an accidental configuration error. Its former virtual
arithmetic requirement is superseded by the shared-capacity request, but has
not yet been removed in this checkpoint.

Source locations below refer to the fixed baseline (existing files unchanged).

| Source | Finding / required integration boundary |
| --- | --- |
| `lib/paths/personal_capacity.go:20`, `:46` | Profile parsing selects 5.6e12/16.8e12 virtual bytes; lexical path matching is not filesystem identity; stat replaces physical free. |
| `lib/paths/local.go:67`, `:114`, `:142`, `:242` | ReservationCtxLock, maps and reserved accounting are process-local. Matching profile overrides even MaxStorage. |
| `lib/paths/local.go:820`, `:958` | Reservation and candidate path selection are distinct; active reservation count includes SDR, but not other processes. |
| `lib/paths/localstorage.go:45`, `:49` | Basic stat and allocated-block DU exist, but neither alone certifies attribution/shared extents. |
| `lib/paths/localstorage_cached.go:17`, `:91`, `:111` | Five-second stat cache; slow DU returns last/zero value, and errors may become a logged value. Not a hard-admission snapshot. |
| `lib/paths/db_index.go:309`, `:837` | Multiple reporters overwrite one storage health row. StorageBestAlloc filters by advertised availability before a local claim. Neither is a shared atomic grant. |
| `lib/ffi/task_storage.go:105`, `:146`, `:204`, `:280` | HasCapacity -> selection -> storage Claim -> task reservation. Completion frees process-local reservations while sector files survive. |
| `harmony/harmonytask/task_type_handler.go:175`, `:221`, `:236` | Capacity/readiness and candidate acceptance precede start reservation and ownership claim; preparation is asynchronous. |
| `harmony/harmonytask/start_reservation.go:11`, `:44`, `:63` | Pacing commitment is short/non-I/O at Do entry. A durable filesystem write must NOT be inserted into that callback under its start lock. |
| `lib/paths/sdr_reservation.go:17`, `:30`, `:48` | Attempt-specific scratch and verified published credit are valid protections, but accounting remains process-local. |
| `lib/ffi/sdr_receipt.go:28`, `:152`, `:225` | Published output identity is checked; a receipt is neither allocated-block accounting nor native liveness evidence. |
| `lib/ffi/sdr_funcs.go:76`, `:122`, `:127` | Non-task storage acquisition can fetch first and reserves only `allocate`; wrapping TaskStorage alone misses this path. |
| `lib/ffi/sdr_funcs.go:169`, `:356`, `:651`, `:768`, `:805`, `:888` | SDR; TreeRC reflink/copy and native write; unsealed conversion; Finalize; MoveStorage; TreeD must participate in accounting transitions. |
| `lib/proof/treed_build.go:58` | Creates/truncates/fallocates TreeD, then writes it; error cleanup can delete it. |
| `lib/paths/remote.go:111`, `:195`, `:252`, `:348` | Fetch staging, acquire-into reuse and cross-role movement; the fetch map is local to a process. Copies must not inherit source credit. |
| `lib/paths/local.go:1149`, `:1181`, `:1243`; `lib/paths/util_unix.go` | Removal/copy removal and external `mv`; same storage ID is not the same test as same filesystem, and a copy cannot be treated as a rename. |
| `lib/ffi/piece_funcs.go:13`, `:25` | Piece/upload entry points also reach storage; a managed root cannot silently allow unsupported writers. Writer inventory is not closed yet. |
| `tasks/seal/task_sdr.go:127` | SQL completion is not file eviction; residency must survive task ID removal. |
| `tasks/seal/task_synth_proofs.go:72` | Standard type 8 skips synthetic calculation and records completion. No early label cleanup may be inferred. |
| `tasks/seal/task_finalize.go:156`; `tasks/seal/task_movestorage.go:202` | Finalize is not covered by a normal TaskStorage claim in the same way as MoveStorage. A grant must span its physical writes/deletions. |
| `tasks/seal/poller.go:480`, `:496`, `:541`, `:557` | Pipeline flag transitions schedule later work; they do not prove native exit or return disk space. |
| `market/backpressure/backpressure.go:96`, `:133` | Queue pressure controls waiting workload, not shared local filesystem commitments. Do not repurpose it into a disk allocator. |
| `lib/paths/sdr_startup.go:22`, `:84`; `lib/paths/sdr_auto_state.go:23`, `:120` | Independent cleanup retry and separate filesystem/native evidence must remain independent of new admission. |
| `lib/sdrscratch/managed.go:123`, `:161`, `:338`, `:374`, `:454`; `lib/sdrscratch/auto_boundary_linux.go:59` | Existing pinned directory, participant, termination and space-debt protections. Do not equate flock release, row absence or heartbeat age with termination. |
| `lib/proofpaths/cachefiles.go:36`, `:40`; `lib/storiface/filetype.go:101` | Standard layout and historical `141/10` coefficient; the coefficient is not a lifetime peak proof. |

These explain missing safety guarantees, not a unique proven cause for every
reported ENOSPC. Virtual overcommit, unshared future promises, post-SDR residency,
failed scratch and asynchronous measurements are distinct contributing paths.
No production diagnosis or filesystem measurement was performed here.

## Type 8 footprint evidence, not a finished production envelope

Let S = 32 * 2^30 bytes. FFI gitlink is
`de13e6489d1b54ceda04b13574ef01f57bda1875`; the available Rust sources are
filecoin-proofs/storage-proofs-porep 19.1.0. Source excerpts are archived with
their original crate checksums. No native calculation was executed.

| Phase | Retained / growing files | Bound established from source | Return boundary / unresolved part |
| --- | --- | --- | --- |
| SDR | Eleven labels in a unique attempt directory; current layer written to `.tmp`, then renamed | 11*S logical label payload, not complete sector peak; `create_label/mod.rs:58` | Return does not release residency. Failed scratch must actually pass existing termination/cleanup protection. Allocation overhead still needs margin. |
| TreeD | Labels plus full binary tree D | (2*S - 32) payload; `treed_build.go:58`; Rust `proof.rs:1628` | Fallocate/overwrite and error unlink are accounting events. Sparse length is not physical credit. |
| TreeRC | Labels, D, full arity-8 C, sealed, R-last | Sealed S; C < 8*S/7 for base-tree payload; Go `sdr_funcs.go:386`, Rust `proof.rs:1013`, `:1628` | Reflink followed by encoding incurs COW; failed reflink uses copy. R-last CPU construction/compaction temporary footprint NOT established without merkletree source. |
| PreCommit / WaitSeed / PoRep | Standard-PoRep labels and trees retained | No timeout-based reduction; `task_synth_proofs.go:72` | `after_synth` is not eviction. PoRep helper/vanilla cache writes need completion of the writer inventory. |
| Finalize / keep-unsealed | D renamed and truncated to S, possibly copied across filesystem; ClearCache removes D/C/labels | Go `sdr_funcs.go:651`, `:744`, `:768`; Rust `clear_files.rs:24` | Only after actual deletion and safe writer return; keep sealed/R-last/p_aux/unsealed. Copy destination has its own budget; unlink with open FD does not mean free. |
| MoveStorage / fetch | Source retained while destination staging/copy grows | `remote.go:195`, `:252`, `:348`; `local.go:1243` | Destination before copy, source only after proven release. Same-filesystem rename creates no free. Retry staging is not interchangeable attempt credit. |

The `16*S` envelope in accounting tests is explicitly a **model input**, not a
source-proven upper bound. The 32 GiB margin in those tests is likewise a fixture
parameter, not a newly chosen operational default. No numeric physical or quota
limit is inferred from a disk model/name. Actual XFS usable free and applicable
quota/MaxStorage must bound a production sample.

## Implemented component

`lib/sharedcapacity` implements:

- one checksummed, versioned ledger under a private directory, fsynced temp file,
  atomic rename and directory sync before grant;
- real cross-process flock, bounded lock acquisition, persisted lock inode
  identity, rejection of missing/replaced/symlinked/hardlinked control files;
- tentative execution -> running -> resident state with random attempt token;
- pre-entry cancellation restoring an older residency, not removing it;
- old-token rejection and no timeout/owner-death automatic budget refund;
- explicit proof-requiring reconciliation; corrupt/version/policy mismatch
  fails closed without resetting metadata;
- domain-key construction from verified host/filesystem identity, excluding
  storage ID, root path, bind mount ID and boot ID;
- structured decision values, with physical free, credit, future commitment,
  additional request, margin and admissible headroom kept separate.

For a valid sample: R = sum(envelope - exclusively attributed materialized
credit), and new requests require F >= R + additional + margin. An existing
resident follow-up within its previous promise does not reserve the whole
envelope again. A capacity alarm does not transfer its promise to a new sector.
Sampler errors still deny a grant. `Returned` retains the commitment. `Cancel`
is allowed only before *any* write, including fetch, not just before native SDR.

The sampler receives an isolated state copy. It must provide fresh free and
certified credit in one serialized writer protocol; the component cannot detect
a caller that supplies a lying/stale observation. Tests deliberately show this.
Likewise a callback returning nil is not itself evidence of native termination.

The lock is not held across modeled execution. Only metadata operations and the
small fixture sampler execute under it. The two-second deadline bounds waiting
for flock, not arbitrary kernel fsync latency or a misbehaving sampler. There is
no separate daemon, DB schema, dependency upgrade or new operator configuration.

## Validation scope

The handoff includes exact commands/logs. Real OS subprocesses exercise a
two-process SDR/Trees role model and a four-process two-pair model, with one
older resident sector, three cycles of admission/handoff/drain/refill, and the
3.2e12/12.8e12 capacities. Both Trees processes perform transitions; they are not
empty placeholder processes. Byte arithmetic uses 32-GiB-sized sectors. Native
time, real pacing and physical 32-GiB files are NOT simulated as execution proof.

Other tests cover last-slot races, simultaneous sufficient-space admission,
token mismatch, cross-role retention, startup/corruption, SIGKILL at publication
boundaries, independent domains, denied reconciliation, cached-file loss,
open-unlinked/unknown occupancy as model inputs and assertion-failing unsafe
samplers. Stage-labelled stall tests are accounting state models, **not** actual
TreeRC/chain/PoRep/NAS failure injection. Alias tests check identity-to-key
mapping, not Linux bind mounts or filesystem UUID discovery.

Self-review caught and corrected: (1) SIGKILL test injection continuing before
signal delivery; (2) cancellation/reconciliation retaining an invalid prior
envelope; (3) missing lock-file recreation risking two lock domains. These are
new component/fixture corrections, not claimed pre-existing Curio regressions.

Still NOT RUN / NOT IMPLEMENTED: actual Curio Claim/Acquire/Do integration,
fresh XFS sampler, reflink-exclusive extent accounting, quota/MaxStorage,
all-writer mutation fencing, actual process/native crash reconciliation,
native type-8 peak, bind/remount tests, production, and integration pacing
unwind. Race detector and Linux cross-compilation do not replace these.

## Required continuation (no activation in this checkpoint)

1. Obtain checksum-matching merkletree 0.23.0 and close the native footprint and
   all-writer inventory. Keep source bounds distinct from native measurements.
2. Choose and implement the credit protocol: stable file identity/extent
   attribution with mutations serialized by generation, or a proven equivalent.
   Do not certify sparse sizes, overlapping reflink extents or arbitrary scratch.
   Do not perform a full filesystem DU under the global lock.
3. Add Linux XFS identity discovery (verified filesystem UUID/device and host),
   one trusted registry for the cohort, fresh/quota-bounded samples, and explicit
   whole-cohort bootstrap. A path-derived identifier is insufficient.
4. Integrate before preparation writes and pacing commit. Cover every fetch,
   TreeD/TreeRC, Finalize/unsealed, move/copy and supported other writer; preserve
   existing termination and cleanup checks. Close bypasses before switching
   profile semantics; do not keep virtual admission as a fallback.
5. Test actual caller cancellation/unwind, publication and handoff failures,
   mixed versions, reboot, open-unlinked and reflink on disposable Linux/XFS.
   Add real standard-PoRep measurement without touching operational storage.
6. Review the complete diff and cohort rollout gates below. Only then is a
   personal runtime candidate eligible for operator review.

## Whole-cohort rollout / rollback plan — NOT executable yet

- Inventory every writer/root/storage ID on one filesystem, including Trees,
  fetch, Finalize, MoveStorage, piece/unseal/Snap if configured. Resolve aliases
  to one filesystem authority. Never create a separate budget for each SDR pair.
- Choose one single-topology canary (two processes) and one dual-topology canary
  (four processes). Preserve slot/pacing/resource/backend settings. The doc's
  44m/25m45s observations are not permission to rewrite configuration.
- Stop *new* admission through an operator-approved maintenance procedure;
  do not interrupt existing native work. Cordon alone is not native termination.
  Wait for real writer exit and complete a resident/scratch/copy census. Prefer
  a fully drained cohort for first bootstrap. If unresolved residency remains,
  retain it physically and do not create an empty ledger as if it vanished.
- Verify all cohort binaries/protocol versions and a consistent registry/policy,
  XFS identity, boot evidence, actual free, quotas/MaxStorage, backend and proof
  layout. No member may continue legacy fake-cap admission. Missing/corrupt
  ledger is an investigation condition, not permission to initialize over it.
- Preserve ledger/control metadata backups while quiescent. Bring up the entire
  cohort without admission first; verify samples/promises, then admit under an
  independently approved canary procedure. Exercise healthy 4 and 6+6 acceptance
  only when arithmetic permits; observe backpressure and downstream drain.
- On rollback, block new work for the *whole filesystem*, let admitted writers
  finish, and reconcile resident/future promises. Preserve ledger and files.
  Legacy binaries ignore these promises: switching just one participant back is
  unsafe. Do not delete state or fabricate free space to make rollback proceed.
  Re-enable legacy admission only after a separate reviewed capacity assessment.
- No service commands, queue SQL, automatic GC or file deletion are provided or
  executed by this checkpoint. Do not change production based on model PASS.
