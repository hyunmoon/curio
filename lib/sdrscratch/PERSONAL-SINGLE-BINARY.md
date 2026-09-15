# Personal SDR cleanup in Curio

This is personal-only code, not an upstream proposal or deployment approval.
It adds a new interface; the preceding release did not support this variable.

```sh
export CURIO_PERSONAL_SDR_CLEANUP=1
# Keep the existing curio (or curio-0) run command and arguments unchanged.
```

Unset, `0`, and `false` preserve the preceding behavior. `1` and `true` enable
the new mode. Other values fail explicitly. Enabled mode conflicts with
`CURIO_SDR_DISCARD_INTERRUPTED` or an inherited manual domain-lease FD; the old
variable still means a JSON file, not a boolean. Do not combine the modes.
Help/version/config/read-only commands do not create a managed worker lifetime.

## Required service and host conditions

**The currently reported `Delegate=no` configuration is not supported by this
mode. An environment variable alone is NOT sufficient on that configuration.**
The minimum additional service change for separate review is `Delegate=yes`.
The code never writes a unit file, calls set-property, or changes this setting.
It requires `Type=simple`, `KillMode=control-group`, `SendSIGKILL=yes`, root,
systemd/cgroup v2, and a local supported filesystem. Other service types fail
closed. A successful test with delegation does not validate `Delegate=no`.

At the start of the real `run` action, before DB, tasks, native self-test or RPC
initialization, Curio creates a fresh child cgroup under its delegated service
and moves its own entire thread group there via `cgroup.procs`. Existing
processes outside that new lifetime cause rejection. No helper, supervisor,
re-exec, second daemon or secondary Curio process is introduced. MainPID,
executable identity, argv, environment, working directory, UID/GID, inherited
resource limits, signal handling and exit codes retain their existing paths.
No new CPU/memory/pids controller settings are written.

Native SDR is called synchronously through `GenerateSDR -> cgo.GenerateSDR ->
generate_sdr -> seal::sdr`. The wrapper does not launch a helper. Threads and
any descendant processes remain in the managed subtree. Synchronous return
and crash termination remain distinct proofs: reclamation after a crash needs
the original subtree empty/removed, or the same host in a later boot, under
the existing no-process-migration/no-external-FD-transfer contract. Cordon,
PID absence and free flock are not termination proofs. External root processes
or unregistered accessors must not enter/reuse the private scratch namespace.
Automatic enrollment is not proof that no such accessor exists.

The cgroup single-writer/delegation contract is documented by
[systemd](https://systemd.io/CGROUP_DELEGATION/). Kernel movement/empty-state
semantics are in the [cgroup v2 documentation](https://www.kernel.org/doc/html/latest/admin-guide/cgroup-v2.html).
Neither documentation nor compilation is actual Linux execution evidence.

## Registered storage, not disk names

`Local.openPath` registers only the actual `CanSeal` local storage root and
storage ID from `sectorstore.json`. Preparation retries registration for those
same opened roots. Store-only and unregistered paths are not scanned by this
mode. No `/sdisk` or `/sdisk_sm` name matching, disk search or guessed child path
is used. One root, the other root, both roots in one instance and roots shared
by multiple participating instances use the same mechanism.

Internal persistent state lives at `/var/lib/curio/sdr-cleanup`, outside the
registered storage trees. Scratch must be on a different filesystem from this
state so a full scratch filesystem does not consume the reclamation journal.
A same-filesystem or unsupported root is blocked for SDR, not silently accepted.
Operators do not supply another target-list variable or manually author domain
JSON. State-directory ancestry and records must be root-owned/non-writable.

The domain is stable for this host. Each physical root has a separate registry
record keyed by device/inode; storage ID and exact path are rechecked. A second
instance extends the valid membership under a short nonblocking lock and atomic
write/sync/rename. Bad records are not overwritten. Aliases/changed identities
are rejected, not counted as additional storage. Different roots may be on
different filesystems. Mount checks run root -> cache/key -> private file within
each root, not across disks. A missing empty cache/key directory is allowed only
after its registered root/metadata identity has been validated.

Reclamation witnesses and open-inode checks are storage-base scoped. A corrupt
root record, unresolved scratch or reservation error blocks that root's SDR
entry while preparation continues for other roots. Shared management-state
failure necessarily blocks the managed mode; it is not reported as a healthy
scratch root. No free-space high-watermark rule is added. Existing reservation,
virtual capacity, per-role slot counts, pacing and FFI/backend choices are
unchanged. This mode does not select a personal capacity profile for the user.

## First maintenance and later automatic reclamation

Old files do not gain termination evidence merely by enabling this option.
Canonical cache/key, sealed/unsealed and published outputs remain protected.
No sector/market/GC/DB operation is performed. A failed or unrecognized old
private attempt blocks new SDR entry on that root until explicit maintenance.

The same Curio executable provides exact-list maintenance:

```sh
curio --repo-path /path/to/existing/repo sdr-cleanup preview \
  --unit example.service --unit example-second.service \
  --paths /path/outside/storage/exact-targets.json \
  --plan /path/outside/storage/new-plan.json
curio --repo-path /path/to/existing/repo sdr-cleanup apply \
  --plan /path/outside/storage/new-plan.json \
  --journal /path/outside/storage/new-journal.jsonl
```

These are command forms, **not permission to execute maintenance**. Unit names
must be the operator's complete accessor inventory. Auto-observed membership
alone cannot authorize maintenance: all recorded members must be included and
each explicit unit must have a preserved enrollment. Unknown old accessors or
changed cgroup identities need review, not a fabricated historical record.
The target file is a finite JSON array of exact `{ "Root": "...", "Relative":
"cache/...tmp" }` entries, not a glob or automatically discovered sector list.
Root must be registered. Preview prints full identities/files and writes a new
plan; apply rechecks the current registry and uses the existing masked/inactive
unit guard, kernel subtree check, exclusive domain lease and exact confirmation.
The existing global domain maintenance lease intentionally excludes all current
managed processes on this host during legacy apply; this is not a live-worker
maintenance feature. No broad sweep of old records/units is performed.

No separate helper or hand-authored management JSON is required. New internal
records are preserved, not imported into or used to erase prior manual-mode
records. Old unrecognized attempts stay protected. The previous 19-item legacy
inventory is a separate unexecuted operation.

Once execution is properly managed, later crashes do not require manually
rebuilding a target list for the new managed scratch: the existing scanner uses
the writer's preserved run identity and real subtree termination evidence.
Self-observed synchronous failure/publication-failure cleanup remains immediate,
including diagnostic write/sync failure. Unknown, live, foreign and published
data remain protected; errors/partial results keep their existing journaling.

## Validation boundary

Offline tests cover option handling, real CLI help/version parsing, registered
path selection, per-root registry damage, concurrent membership, immutable old
metadata, capacity profiles, scanner/mount invariants and the FFI error paths
with replacement native functions. See the review archive for executed results.

`TestPersonalLinuxReviewSameBinary` supplies real systemd/cgroup/self-entry,
two small separate ext4 loop filesystems, a no-delegation rejection, preservation
of argv/env/PID and own-discard/canonical controls. Its test executable replaces
Curio/native computation; only the persistent state location is relocated.
`TestManagedLinuxReviewAccounting` and `TestManagedLinuxReviewMounts` cover the
existing actual procfs/accounting and mount checks. These require an owned
disposable host. Compilation is not execution. Existing parent/child tests and
mock boundaries do not prove that new systemd service path passed on Linux.
Actual Linux, XFS, native SDR/CUDA and production validation remain separate
gates. No service setting should be changed merely to make an unreviewed test pass.
