# SDR attempt scratch lifecycle

`GenerateSDR` owns one randomly created `attempt-*` directory beneath
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

Publication is an atomic no-replace rename (`RENAME_NOREPLACE` on Linux,
`RENAME_EXCL` on Darwin). A previously published cache/key is never removed
to make room for a retry. An existing destination or unsupported filesystem
operation is an explicit error, requiring review; the code does not fall back
to a racy existence check or overwrite. In particular, a successful publish
followed by failed pipeline persistence may leave a valid cache that this
change intentionally refuses to replace automatically. Recovery of that
case is not added here. Cache permissions remain 0755. Key generation
publishes only the final layer and removes its other temporary layers.

Only published output is declared through the existing `AcquireSector`
release helper. No index declaration is dropped on failure. Published
output survives subsequent `ensureOneCopy` errors. The existing successful
cross-storage `ensureOneCopy` reconciliation is otherwise unchanged; this
patch is not general sector/attempt fencing across storage locations.

Reservation accounting includes `<output>.sdr.tmp` for cache/key, alongside
the existing canonical/legacy/fetch accounting, with the existing per-sector
reservation cap. The `.tmp` suffix keeps the root out of sector declaration.
Canonical filenames, file type parsing and normal GC destinations do not
change. There is no background/startup cleanup or sector GC approval.

## Compatibility and limits

- Existing capacity profiles, four-slot limits, pacing, retry budgets,
  pipeline state, scheduler and database schema are unchanged.
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

`lib/paths/sdr_reservation_test.go` covers cache/key materialization, four
reservations with the unchanged personal capacity profile, bounded credit
and declaration exclusion. Existing scheduler/pacing tests separately check
admission, completion/refill and cancellation lifetime. These are local
unit/model results, not physical-disk or native-throughput validation.
