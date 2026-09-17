# Personal automatic SDR temporary-directory disposal

This personal-only policy applies to the existing `CURIO_PERSONAL_SDR_CLEANUP`
mode. It does not change enrollment, termination, storage profiles, reservation,
slots, pacing, native backend, or the separate manual-maintenance interface.

## Temporary namespaces and publication boundary

The historical `SealCalls.GenerateSDR` passed `intoPath + ".tmp"` exclusively to
native SDR. Cache success renamed that directory to canonical cache; key success
moved the final layer to the canonical key file. Current `generateSDR` passes a
unique `owned-v1-*` reservation within `<sector>.sdr.tmp` to `BeginWithOptions`
and native SDR. Older `attempt-*` directories use the same private purpose.
No-replace cache/key publication leaves successfully published output outside
these temporary targets. TreeRC consumes canonical cache, not the private paths.
Remote transfer staging uses the separate `fetching` directory.

The pinned filecoin-ffi Cargo.lock uses storage-proofs-porep 19.1.0. Its
`create_label::write_layer` writes `data_path.with_extension(".tmp")` then
renames to the final layer name: a `.dat` path therefore has a `..tmp` sibling.
The SDR label writer uses flat regular files, not a nested directory tree.

After **all existing** registered-root, live DB read, participant/termination,
sector gate, pinned-directory lock and identity checks pass, automatic cleanup
discards every safe regular file in the unpublished temporary directory. It
does not require any final layer to exist, nor a filename or layer-number
allowlist. Even full-looking layers or an unpublished completion receipt do not
make a private interrupted attempt reusable under this personal policy.

This is whole-directory disposal, not recursive `RemoveAll`: symlinks,
hardlinks, subdirectories, filesystem/mount crossings and
identity replacements still fail closed. Those violate the existing flat
private-file contract, independently of their names. Every entry is inspected
before the first unlink. Per-file identity is rechecked at unlink; partial
failures retain witnesses and retry only the remaining files. Namespace names
are not authentication: unregistered/external accessors must not enter them.
An unconverted/live participant still blocks disposal. The existing cgroup and
exclusive-access contract, not `after_sdr=false`, supplies execution safety.

## Pipeline lifetime is not private-directory lifetime

For these private namespaces only, an absent pipeline row, sector metadata or
another attempt's SDR/later completion does not prohibit disposal. Normal
`PipelineGC.cleanupSealed` removes completed pipeline rows after metadata and
piece checks; the legacy `SectorRemove` path can also remove a row. Absence does
not identify which happened and does not prove execution ended. A real DB query
error is still refused. Cleanup does not create/remove tasks or pipeline rows.

Private discard does not require a disappeared row's proof type, layer names
or per-layer size limit. It never guesses a proof or sector size. Flat private
files are validated by their pinned identity, regular-file type, link count
and mount, not by whether their size matches a now-unavailable proof. Canonical
cache keeps its existing proof-derived size limit and completion checks.

An absent-row `SELECT FOR UPDATE` is **not** a lock on a future INSERT. The
existing physical sector access gate serializes cleanup with SDR Claim,
native entry/reuse/publication and participating remote access. The exclusive
gate is acquired before the DB read and held through actual unlink and space
verification. Claim takes shared access before creating/reserving scratch;
the real I/O caller also takes shared access and holds it until return. DB
row recreation can succeed during cleanup but cannot authorize bypassing
that gate. Managed participant/process-subtree evidence still rejects live
or unconverted accessors; a live writer's directory lock also rejects disposal.
Namespace spelling alone, missing DB ownership or a heartbeat timeout is not
termination evidence. Unmanaged external filesystem operations remain outside
this contract and must not access these private namespaces.

Cached completed-stage denials apply only to canonical cache. Protected reasons
are logged at startup and at the existing 15-minute per-root diagnostic interval;
ordinary 30-second retry passes do not repeat all unchanged protected targets.
Successful/partial unlinks continue reporting their actual removal counts. The
independent timer, root isolation and capacity-refresh paths are unchanged.

## Canonical data stays separate

Canonical cache keeps the stricter receipt, proof-layout and known-layer-name
checks. A complete layer set, completion receipt, unexpected file, or an unknown
state preserves it, including a full layer set accompanied by `..tmp` files.
Live `after_sdr`/later-stage and metadata checks also protect TreeRC input.
`after_sdr=false` alone does not authorize deleting canonical cache. Canonical
key is not an automatic directory-disposal target. Normal published-output
reuse and receipt validation are unchanged.

## Evidence boundaries

Regression tests cover arbitrary private filenames, zero/partial/full-size
temporary files, tmp-only directories, old and managed namespaces, actual
32-GiB proof layout using sparse files, canonical preservation, live writers,
replacement, unsafe entries and partial-delete retry. PostgreSQL integration
fixtures exercise Local startup/timer, reservation/Claim, both FTCache and FTKey,
and publication/reuse with a replacement native function. Test-only overlays
replace Linux root/cgroup/process-census checks on Darwin and accelerate the
retry timer; they do not replace the SQL/migration/lock/unlink paths. These are
not native SDR, Linux/XFS or production execution evidence. The existing Linux
auto-discard fixture includes `..tmp` and backend-private names for separate
execution in an owned disposable Linux environment.

Orphan regression fixtures execute the actual GC SQL (checked against source),
real migrated PostgreSQL schema and Local startup/timer paths for all three
private forms under both cache and key. They preserve coexisting canonical
bytes, distinguish DB errors from absence, inject a partial unlink failure,
check capacity refresh without admission, and allow a concurrent DB INSERT
while proving real Claim cannot enter until cleanup releases the gate. These
fixtures do not establish how any particular production pipeline disappeared.
