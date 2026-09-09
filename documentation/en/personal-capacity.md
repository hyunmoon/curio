# Personal virtual storage profiles

`CURIO_PERSONAL_STORAGE_PROFILE` is a local, startup-only environment selection,
not a shared Harmony config layer. `NewLocal` validates it before opening paths
and logs the selected profile name and virtual capacity. Unset preserves upstream
behavior. Unknown, case-altered or whitespace-padded values fail construction;
they never silently disable the profile. No source mutation is needed at build.

| Profile | Matching root | Virtual capacity, bytes |
| --- | --- | --- |
| `sdisk-4slot` | `/sdisk/sealworker` | 5600000000000 |
| `sdisk-sm` | `/sdisk_sm/sealworker` | 16800000000000 |

Matching retains the previous lexical rule: clean the local path and require
equality with the root or prefix `root + path separator`. It is not symlink
resolution. A sibling such as `sealworker-other` does not match. For a matching
path, use `LocalStorage.DiskUsage(p.Local)`, never the parent disk's usage.

After existing reservation accounting:

```
fsAvail = max(0, virtualCapacity - used)
available = max(0, fsAvail - stat.Reserved)
stat.Capacity = stat.Max = virtualCapacity
stat.Used = used
stat.FSAvailable = fsAvail
stat.Available = available
```

This deliberately overrides physical free space and `MaxStorage` for the matched
path. It is virtual/overcommit accounting, not proof of real capacity or enough
scratch space. Disk-usage errors and negative usage fail closed. Unmatched paths
keep physical accounting and the original `MaxStorage` behavior. Reservation
accounting/caches and actual filesystem operations are not otherwise changed.

Both profiles are supported by the same binary. Selecting one does not set task
limits or pacing: the separately supplied pc1/pc1-sm config layer still governs
those. The build receipt records the intended runtime profile, but the environment
must also be present when the operator later starts the process. No start,
deployment, migration or config-application command is provided here.
