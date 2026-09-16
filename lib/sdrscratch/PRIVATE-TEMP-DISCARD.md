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

After **all existing** registered-root, live DB stage, participant/termination,
sector gate, pinned-directory lock and identity checks pass, automatic cleanup
discards every safe regular file in the unpublished temporary directory. It
does not require any final layer to exist, nor a filename or layer-number
allowlist. Even full-looking layers or an unpublished completion receipt do not
make a private interrupted attempt reusable under this personal policy.

This is whole-directory disposal, not recursive `RemoveAll`: symlinks,
hardlinks, subdirectories, filesystem/mount crossings, oversized files and
identity replacements still fail closed. Those violate the existing flat
private-file contract, independently of their names. Every entry is inspected
before the first unlink. Per-file identity is rechecked at unlink; partial
failures retain witnesses and retry only the remaining files. Namespace names
are not authentication: unregistered/external accessors must not enter them.
An unconverted/live participant still blocks disposal. The existing cgroup and
exclusive-access contract, not `after_sdr=false`, supplies execution safety.

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
