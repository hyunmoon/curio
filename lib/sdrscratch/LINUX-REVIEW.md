# Disposable Linux review (not executed on Darwin)

Run only on an owned disposable **native Linux systemd host**, root PID/mount/
cgroup namespace, cgroup v2, permitted CLONE_INTO_CGROUP, local ext4/XFS under
`/var/lib`. No production config, storage paths or service names are inputs.
The connected fixture creates exactly two fresh UUID-named services, verifies
their actual properties and uses the real helper/config/lease/scanner/accounting.
It stops/removes only those fixture units on exit. A failed/interrupted test
must be inspected before another run; no script sweeps arbitrary old units.
The driver retains its uniquely owned fixture and logs. Do not use a container
whose namespaces/proc visibility prevent the host-wide descriptor check.

From the reviewed restored source, using the existing Go toolchain/dependencies:

```sh
mkdir -p /tmp/sdr-review-build
CGO_ENABLED=0 go build -o /tmp/sdr-review-build/sdr-scratch ./cmd/sdr-scratch
CGO_ENABLED=0 go test -c -o /tmp/sdr-review-build/sdrscratch.test ./lib/sdrscratch
# Check BOTH build exit statuses before proceeding. No native FFI is linked.
sudo env CURIO_SDR_DISCARD_TEST_TARGET=disposable bash \
  scripts/personal/validate-sdr-discard-linux.sh \
  /tmp/sdr-review-build/sdr-scratch /tmp/sdr-review-build/sdrscratch.test
```

The finite test set is:

- `TestManagedLinuxReviewAccounting`: actual root-owned witnesses and procfs,
  residual/open-inode rejection, close/unlink completion, independent filesystem
  writer, old high-watermark compatibility and corrupt A / healthy B isolation.
- `TestManagedLinuxReviewMounts`: same-filesystem bind and different tmpfs
  mounts at cache. Preview and apply must refuse with zero removals. Same-mount
  execution is a positive control. This test injects only the maintenance guard;
  the following test uses the real guard.
- `TestManagedLinuxReviewConnected`: real `sdr-scratch run`, two accessors,
  parent death with a surviving child, blocked second accessor, entire-subtree
  exit, production scanner/accounting, next Begin and immediate own discard.
  The launcher is temporarily SIGSTOPped **only in the fixture** so systemd
  cannot kill the child before that boundary is observed. Real masked/inactive
  maintenance guard, cancellation and exact-list apply preserve canonical data.

These use tiny replacement worker/native bodies, not SDR/CUDA. Environment
forwarding is checked with representative slot/interval sentinels; that does
not prove production Curio configuration loading or every real unit's flags.
Existing role commands, `CURIO_PERSONAL_STORAGE_PROFILE`, slot counts and pacing
must remain unchanged. Unit `Type=notify`/NotifyAccess and actual role/backend
compatibility require later operator review. This does not authorize service
changes, the separate legacy list, deployment or production deletion.

On Darwin, run the deterministic `TestAccounting*`, `TestStorageRootBaseMountEdge`
and existing scanner/FFI tests. They substitute only OS trust/open-reference or
mount evidence where noted. Their PASS is not Linux execution. Linux cross-
compilation is COMPILED, never Linux PASS. No new policy is enabled by binary
replacement alone; all accessors require the same domain and managed launcher.
