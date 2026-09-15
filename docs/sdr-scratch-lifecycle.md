# SDR attempt scratch lifecycle

`GenerateSDR` owns one UUID-named `attempt-*` directory beneath
`<output>.sdr.tmp`. Creation is filesystem-exclusive across processes, not
based on a task ID, owner lookup, or a process-local mutex. Legacy
`<output>.tmp` directories are not recycled or swept.

The synchronous native call must return before cleanup starts. The pinned
filecoin-ffi SDR path calls filecoin-proofs-api/filecoin-proofs 19.1.0 and
storage-proofs-porep 19.1.0. The multicore label loop joins its scoped
producers before `write_layer`; layer writes/renames are synchronous. The
single-core path is synchronous too. This is a source-level contract, not a
CUDA/native execution test or proof that cancellation stops native work.

After any ordinary return, the attempt's unpublished scratch is removed
before its storage reservation is released and before `GenerateSDR` returns
to its task caller. An error before creation leaves existing files alone.
Cleanup failure is returned alongside the original error and logged once;
remaining data is not reported reclaimed. There is no infinite cleanup retry.
The shared empty root is retained to avoid racing another directory creator.
Repeated failures of the same sector do not create more roots.

Publication remains an atomic no-replace rename (`RENAME_NOREPLACE` on Linux,
`RENAME_EXCL` on Darwin). Known destination conflicts are rejected before
native entry; a destination created during computation is rejected by the
final rename. Legacy/unmarked output, including an empty directory, is never
overwritten or automatically accepted. Cache permissions remain 0755. Key
generation publishes only the final layer and removes its other temporary layers.

After successful synchronous native generation, a versioned completion receipt
is attached to the output inode as `user.curio.sdr-completion-v1`. It travels
atomically with the renamed cache directory or FTKey file. Unlike a separate
sidecar, the receipt cannot be published without its key file. User-xattr support
is probed in private scratch before native entry; unsupported filesystems fail
closed, without changing backend or capacity policy. A receipt records sector,
file type, full proof type, CommD, ticket, replica ID, and the generated layer
layout/sizes. Only such known outputs can be reused. Input mismatch, missing or
changed layers, unknown receipts, and symlinks fail before native computation.
This is trusted-local-output provenance, not cryptographic integrity verification
of arbitrary files or a proof that another process has stopped writing them.

A post-publication `ensureOneCopy` or caller database failure can now retry the
same output without native recomputation. For new sealing, the receipt also
records ticket epoch. `SDRTask.Do` reads it instead of selecting a new ticket,
then rechecks original randomness against the current chain head and the existing
PreCommit randomness window. A stale/reorganized ticket is rejected, not relabeled
or silently replaced. PreCommit still performs its own later validity check.
SDRKeyRegen continues using the historical ticket/CommD from sector metadata;
new-sealing ticket age rules do not apply to key regeneration.

Receipts survive worker process restart after publication on the same local
path. Crashes before publication leave an unowned scratch directory, not a
reusable completion. This does not promise recovery from storage corruption or
power-loss ordering of every native-written layer. A transfer that strips xattrs
leaves unrecognized output requiring review. No remote-search/fetch recovery,
new database authority, migration, blanket retry exemption, or output replacement
is introduced.

Only published output is declared through the existing `AcquireSector`
release helper. No index declaration is dropped on failure. Published
output survives subsequent `ensureOneCopy` errors. The existing successful
cross-storage `ensureOneCopy` reconciliation is otherwise unchanged; this
patch is not general sector/attempt fencing across storage locations.

SDR storage claims explicitly select attempt-owned reservation accounting.
The unique scratch name is allocated at claim time and created exclusively at
Do entry. Only that claim's scratch, or a validated published output that Do
must reuse or reject, can reduce its outstanding reservation. Legacy scratch,
abandoned attempts and other active attempts still count in total disk usage
but cannot fund a new native computation. A credited published output that
disappears causes rejection, requiring a new full-budget claim. Duplicate local
claims have distinct reservation entries and idempotent release; one cannot
release the other's reservation. Published-path credit is not duplicated between
SDR claims in one Local registry. Normal TreeD/TreeRC existing-cache/fetch credit
is unchanged; their reservation does not include unrelated SDR scratch roots.

Reservation accounting remains process-local and uses the existing disk-usage
cache. Separate processes see actual foreign disk bytes, not each other's future
reservation budgets. This does not create a distributed quota or make mixed
native execution safe. FTCache and FTKey retain their actual overheads, 141/10
and 111/10 respectively. The `.tmp` suffix keeps scratch out of declaration.
Canonical filenames, file type parsing and normal GC destinations do not
change. There is no background/startup cleanup or sector GC approval.

## Compatibility and limits

- Existing capacity profiles, four-slot limits, pacing, retry budgets,
  pipeline state transitions, scheduler and database schema are unchanged.
- New cleanup cannot remove another new attempt's scratch or published
  cache on the same storage path. Old workers still use destructive shared
  paths and can remove canonical output; this does not make concurrent
  mixed-version execution of one sector safe.
- SIGKILL, OOM kill, process abort, host loss and filesystem failure can leave
  scratch. Context cancellation alone never triggers cleanup while native
  work has not returned. Historical leftovers require separate review.
- Initial physical-space exhaustion can still occur with retained downstream
  data or virtual-capacity overcommit. Reclaiming failed scratch prevents
  that particular accumulation; it is not a new capacity/admission policy.

## Regression coverage

`lib/ffi/sdr_cleanup_test.go` exercises production acquisition, SDR lifecycle,
atomic publication and release ordering with a substituted synchronous native
body and an explicit cleanup-error injector. It uses allocated local files,
barriers and two independent `SealCalls` instances. It does not run native
proofs or write storage declarations to a database. The incident-source
negative control preserves the old lifecycle and substitutes only the native
call; it fails both scratch-removal and no-declaration assertions.

`lib/paths/sdr_reservation_test.go` and `sdr_fresh_test.go` exercise actual
`Local.Reserve`/`ReserveSDR` and profile arithmetic with an injected disk-usage
backend: fresh progress, foreign/legacy bytes, multiple claims, separate-process
accounting models, four slots, actual overhead constants and existing-cache credit.
`lib/ffi/sdr_retry_test.go` checks both output types, retry, mismatched inputs,
early/late conflicts and reuse across independent test processes.
Existing scheduler/pacing tests separately check
admission, completion/refill and cancellation lifetime. These are local
unit/model results, not physical-disk or native-throughput validation.
