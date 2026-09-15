# SDR scratch return certificates

Scope: new SDR cache/key attempts created by `Begin`, on local APFS (Darwin),
ext4 or XFS (Linux). This is not a native execution tracker or sector GC.

## Contract

`owned-v1-<uuid>` is a name, not proof. The writer locks the **directory inode**
from creation through synchronous native execution, publication and cleanup.
The xattr record binds version, name, root name/inode and device/inode. There is
no PID, age, task/owner or heartbeat test. A missing, changed or unknown record
never grants deletion authority.

The only automatic restart-recovery certificate is `returned`, written and
synced after a synchronous native **error return**, a pre-native failure, or
successful publication of the last layer for FTKey. Context cancellation does
not write this certificate. A panic during native execution does not write it.
The pinned FFI call synchronously invokes Rust `seal::sdr`; a free process lock
alone is never used to infer whether that call or a child has terminated.

| State when scanned | Action |
| --- | --- |
| Directory lock held | `live`; leave files alone, allow other attempts |
| Valid `returned`, incomplete output, flat private regular files | Unlink files using pinned directory FD, sync, mark `reclaimed` |
| `active` with free lock | `needs_review`, even after process death |
| Missing/old/unknown/identity-mismatched metadata | `needs_review` |
| Completed output or native success before publication | Preserve; never infer failure from an API/publication error |
| Reclaimed, empty tombstone | `already_reclaimed` |
| Reclamation error/partial unlink | Error with actual file count; later scan retries remaining files |

Native success with invalid/missing output is also retained for review. It is
not newly eligible for reuse: the existing receipt/input/ticket validation is
unchanged. Successfully published canonical cache/key retries continue to use
that validation, without repeating native work.

### Crash boundary (important)

The new format can reclaim after a crash **after the return certificate was
persisted**, including a prior cleanup error. A kill/power failure while native
is active or before the certificate is durable remains `needs_review`.
Supporting that earlier crash window would require a separately verified
native-process termination contract. Neither a new lock nor a software upgrade
retroactively provides such a contract for old `attempt-*` or legacy `.tmp`.
An old/nonparticipating process must not be pointed at new private attempts.
Mixed old-writer online cleanup is not claimed.

## Filesystem and admission

`Local.openPath` scans cache/key scratch before taking `localLk`. SDR
`TaskStorage.Claim` also prepares cleanup before sector locks and the existing
global `ReservationCtxLock`. `ReserveSDR` checks the selected storage/type
without unlinking; pending/failed reclamation denies that reservation, while
another healthy storage path remains usable. No deletion occurs under those
global locks; normal byte accounting and selection remain unchanged.
`Begin` scans again under the cache/key directory's nonblocking advisory lock
before creating new scratch. Concurrent startups/creation return a retryable
error instead of waiting while holding scheduler resources. The directory lock
is released before native starts. Unknown/live remnants are not treated as free
space or as a reason to stop every worker; the existing capacity policy still
applies. Scans are linear in scratch entries, not a filesystem-capacity quota.

The collector pins directories, refuses symlink ancestors, checks local FS and
same mount/device (Linux requires `STATX_MNT_ID`), and does not recurse. Symlinks,
directories, hard links and unexpected nonregular entries deny reclamation.
Unsupported filesystems, missing mount-ID support or I/O errors fail closed.
The native path interface still requires the storage namespace not to be
administratively replaced while native is using it. This is not protection
against privileged/nonparticipating writers that falsify metadata or mutate
files inside a certified private directory.

There is intentionally **no pathname-based RemoveAll or rmdir** in recovery.
Empty inode-bound tombstones remain; an FD-relative unlink cannot traverse a
new directory installed at the old pathname. This removes file directory
entries, not a guarantee of immediate `statvfs` space gain: open external FDs,
snapshots and filesystem accounting can delay physical recovery. No GC marks,
GC approval, sweep, canonical removal, DB writes or scheduler changes occur.
The existing unrelated successful-output `ensureOneCopy` behavior is unchanged.

## Validation boundaries

`TestStartup*` covers real directory locks and separate OS processes, return-
certificate crash boundaries, new creation, stale identity, replacement during
unlink, unknown metadata, nonregular files, partial failure and persistent-write
failure. `TestSDRStartup*` uses production GenerateSDR with a synchronous **fake
native body**, real files/xattrs/no-replace publication and real reclamation.
`TestSDRStartupAdmissionFailureAndRetry` exercises Local admission and its startup
scan. Existing receipt/retry, foreign-byte accounting and selection tests remain.
Tests retain empty directories instead of asserting directory disappearance;
they still assert all failed layer files were removed before capacity release.

Linux execution must be performed on disposable ext4/XFS paths with xattr and
mount-ID support. Do not run these tests against storage used by a Curio worker.
With an already working native test environment and inherited DB settings
removed, the focused command is:

```sh
go test -tags=cgo,fvm,nosupraseal ./lib/sdrscratch ./lib/paths ./lib/ffi \
  -run '^(TestStartup|TestSDR|TestLocalAcquireSector)' -count=1 -timeout=3m
go test -race -tags=cgo,fvm,nosupraseal ./lib/sdrscratch ./lib/paths ./lib/ffi \
  -run '^(TestStartup|TestSDR|TestLocalAcquireSector)' -count=1 -timeout=3m
```

This does not execute native SDR. Preserve the existing role/backend build
settings for any later operator build. No deployment or existing-file deletion
is authorized by this document.
