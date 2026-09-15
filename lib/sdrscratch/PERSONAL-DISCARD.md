# Personal-only SDR discard policy

Not an upstream candidate. Default is **off**. The single public opt-in is
`CURIO_SDR_DISCARD_INTERRUPTED`, whose value is an absolute, root-owned managed
configuration JSON file. `sdr-scratch run` supplies it to the worker together
with an inherited domain lease. Do not set the internal lease FD by hand.
`CURIO_PERSONAL_STORAGE_PROFILE` remains capacity accounting, not permission
to delete. No config schema, defaults, pacing, slots, task rows, retry/CAS,
receipt inputs or FFI/backend selection are changed.

## Lifetime and filesystem contract

The current writer may discard its own private cache/key attempt after a
synchronous native error or successful return followed by a publication
failure. A completion receipt in scratch is not publication. Return xattr or
Sync failure is still reported independently and does not erase local return
authority. The directory FD and lock remain held. Path identity is rechecked
before own discard. A renamed canonical directory cannot be reclaimed through
that authority. FTKey publishes the last layer first and discards the private
remaining layers. Existing canonical receipt/input validation and successful
output retry remain unchanged (including the existing ensureOneCopy path).

For crash recovery each new version-2 record includes a domain UUID, host
machine-id, kernel boot UUID, unique cgroup path and cgroup device/inode. A
Linux launcher places the entire worker directly into that cgroup with
`clone3(CLONE_INTO_CGROUP)` through Go's `UseCgroupFD`, **before it runs**.
There is no start-first/move-later fallback. Backend children inherit the
cgroup. Cgroup UUIDs must never be reused or re-entered.

Requirements for this first Linux implementation:

- Native host PID/cgroup/mount namespaces, root, local ext4/XFS, xattrs and
  `STATX_MNT_ID`, cgroup v2 and permitted `CLONE_INTO_CGROUP` (Linux 5.7+).
- Every local storage accessor must be inventoried in one config. No remote
  writer/shared storage, unlisted service, manual worker launch, cross-group
  process migration, FD transfer to an external process, or storage namespace
  replacement is permitted. This cooperative host contract is not an
  adversarial security boundary against privileged administrators.
- Each service has `Delegate=yes`, `KillMode=control-group`, `SendSIGKILL=yes`;
  the launcher verifies these actual properties. Preserve the existing role's
  environment, arguments, backend, resource limits and shutdown timeout.
- Root-owned non-group/world-writable config/state ancestry. Keep state on a
  filesystem with free metadata space, **outside** scratch (preferably a
  different filesystem). Keep the same config/domain through restarts.

The scanner trusts `returned` only for the same enrolled domain. Otherwise it
requires the recorded whole cgroup subtree to be empty, its empty group to
have been removed by the kernel/service manager, or a different boot on the
same host. A free directory flock alone is never termination evidence. A
live process or SIGSTOP remains populated; a child outliving its parent yields
`termination_required` and blocks fresh SDR on that storage path. No age-based
kill or individual native watchdog is added. Stop is managed by the existing
service manager. If stopping a process kills several in-process SDRs, **all
those interrupted attempts will be recomputed**. Cancellation, task absence,
owner/heartbeat changes and scheduler graceful shutdown are not proof.

Startup and pre-admission use the existing `PrepareSDRScratch` and `ReserveSDR`
paths. A live locked *managed or old* writer is protected; an unlocked unknown
or legacy remnant requires maintenance before more SDR on that path. Failed
cleanup blocks the selected path, not the whole cluster or every healthy disk.
Base-directory locks serialize scans/new attempt creation, never native work.

Only flat private regular files are unlinked through pinned directory FDs;
symlinks, hardlinks, subdirectories, unsupported/cross-mount paths or identity
changes fail closed. Empty directories are retained, avoiding pathname rmdir
races. Legacy maintenance leaves an inode-bound empty marker so an empty
unknown directory is not mistaken for a live writer that may still write.

## Unlink is not immediate space recovery

Logs and journals separate removed-file count, allocated bytes (`st_blocks`),
and filesystem free bytes before/after. Linux additionally checks open inode
references through root-visible procfs. A durable per-attempt space witness
outside scratch blocks admission while a handle remains, evidence cannot be
read, or free space is below the pre-unlink-plus-allocation watermark. Later
scans retry remaining files and recheck the witness; never manually delete a
witness to force admission. A torn/unwritable journal fails closed. Return
diagnostic I/O and this independent accounting journal are different stores.
Loss of the latter may prevent cleanup rather than lose the admission guard.

This watermark is conservative and **not attributable freed-byte accounting**:
unrelated filesystem writes, snapshots and delayed accounting can require
maintenance even after successful unlink. No GC marks/approval/sweep, storage
delete API, queue changes or automatic resume/restart are introduced.

## Build and first transition (operator-only; not executed by this change)

1. Review the exact candidate commit/tree. In a normal Linux clone, build Curio
   using the already successful role-specific command. The existing
   `scripts/personal/build-sdisk-4slot.sh` and `build-sdisk-sm.sh` are unchanged;
   inspect their requested/effective flags rather than substituting Darwin
   test flags. Preserve custom worker/backend builds. Build the helper with
   `CGO_ENABLED=0 go build -o /absolute/review-output/sdr-scratch ./cmd/sdr-scratch`.
2. Inventory **all** accessors for each local storage ID/root, including other
   worker roles on that storage. Confirm root/no namespaces/no external writer.
   Create a root-owned state directory outside scratch. Before stopping the
   units (so their cgroup names can be captured), run:

   ```sh
   sdr-scratch enroll --state /var/lib/curio-sdr-discard \
     --storage /absolute/local-storage \
     --unit worker-a.service --unit worker-b.service \
     --config /var/lib/curio-sdr-discard/domain.json
   ```

   `enroll` only reads source identities and writes a **new** JSON file. It
   never changes units. Review the storage IDs, root device/inode, host and
   complete unit/cgroup inventory. It refuses missing/inactive cgroup paths;
   do not guess them. Capture before the maintenance stop.
3. During a separately approved maintenance window, exclude all starts
   (including timers/manual supervisors), mask the listed services, stop them
   using the normal supported stop procedure and confirm actual subtree exit.
   `apply` independently requires each unit masked/inactive and every recorded
   subtree absent/empty. Inactive alone, cordon, lsof absence and an operator
   boolean are not sufficient. Do not enable the launcher before this initial
   legacy transition is complete.
4. Put **only exact reviewed targets** in `paths.json`, e.g.:

   ```json
   [{"Root":"/absolute/local-storage","Relative":"cache/s-t01000-42.tmp"}]
   ```

   Older `cache|key/s-t0MINER-SECTOR.sdr.tmp/attempt-UUID` or `owned-v1-UUID`
   require separately enumerated paths. There are no wildcards or recursive
   automatic legacy deletion. Canonical cache/key names never match.

   ```sh
   sdr-scratch preview --config /var/lib/curio-sdr-discard/domain.json \
     --paths /absolute/review/paths.json --plan /absolute/review/plan.json
   sdr-scratch apply --config /var/lib/curio-sdr-discard/domain.json \
     --plan /absolute/review/plan.json --journal /absolute/review/attempt.jsonl
   ```

   All paths/files are identity-pinned in the plan. `apply` holds an exclusive
   domain lease and prints every path before asking exactly `DISCARD LEGACY N`.
   EOF/wrong input/output failure produces zero removals. It rechecks the
   maintenance guard and exact file identities per target, logs/fsyncs attempt
   before unlink and result after. Any error stops the remaining targets.
   Existing plans/journals are not overwritten. After partial failure, preserve
   the journal, establish the same boundary again and create a **new preview of
   explicitly selected remaining targets**. No blind retry or automatic resume.
5. Only after maintenance and Linux tests pass, arrange each existing service's
   ExecStart as `sdr-scratch run --config CONFIG -- EXISTING-CURIO-COMMAND ARGS`.
   Add the three verified systemd properties above without changing role,
   User/backend/resource policy. For `Type=notify`, review `NotifyAccess=all`
   because the worker is a child; for unsupported service types do not guess.
   Unmask/start only under separate approval. Multiple enrolled services can
   hold shared leases; one service's startup cannot reclaim another's live
   private outputs. No code here executes systemctl stop/start/mask/kill.
6. Subsequent terminated version-2 remnants are reclaimed at startup/admission
   without further human approval. Pending tasks remain; a new scratch UUID is
   allocated by the existing retry path. A live child requires supported whole
   service termination, not deleting its files.

Rollback is service/config-boundary work: stop all affected accessors, confirm
exit, preserve config/witnesses and published outputs, then restore the old
ExecStart/binary under separate approval. Old code does not reclaim version-2
records; do not mix old unmanaged writers with online managed cleanup. Never
remove unknown scratch or accounting witnesses just to complete rollback.

## Tests and limitations

`TestSDRDiscard*` executes the production Go GenerateSDR/receipt/publication
path with small replacement native bodies. `TestManagedActualProcessKill`
uses an actual killed OS process and a test supervisor boundary, not Linux
kernel evidence. `TestManaged*` models child survival, return vs termination,
identity replacement, four slots/repeated crashes, partial cleanup and a
healthy alternative path. `TestLegacy*` exercises the real executor with a
deterministic injected maintenance guard, exact lists, output/confirmation,
re-entry, symlinks/hardlinks and partial errors. Production guard is never
selected by a user boolean or test environment flag.

Linux tests are compiled on Darwin but **NOT RUN** there:
`TestManagedLinuxKernelCrashAndChild` (actual cgroup entry, SIGSTOP, parent kill,
surviving child, child exit), `TestManagedLinuxOpenInodeAccounting` (unlinked
open FD blocks clearing witness). Both require
`CURIO_SDR_DISCARD_CGROUP_ITEST=1`, `CURIO_SDR_DISCARD_TEST_TARGET=disposable`.
The kernel test must run in `curio-sdr-discard-test.service`; the open-inode
test requires an existing root-owned `CURIO_SDR_DISCARD_TEST_STATE` whose
basename starts `curio-sdr-discard-test-`. Run only on disposable local ext4/XFS.
No production service/files are used. Actual launcher/service integration,
Linux XFS publication, real FFI/CUDA, OOM, host reboot and production space
recovery still need operator validation; cross-compilation is not a PASS.

References: [cgroup v2 lifecycle](https://www.kernel.org/doc/html/v6.4/admin-guide/cgroup-v2.html),
[clone3 cgroup placement](https://man7.org/linux/man-pages/man2/clone3.2.html),
[unlink and open descriptors](https://man7.org/linux/man-pages/man2/unlink.2.html).
