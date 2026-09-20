# Shared-capacity caller rework 3 — partial development checkpoint

## Disposition

**NOT a deployment candidate. The production sampler and supported writer
closure are still incomplete.** The R3 caller defects were reproduced and
repaired locally, with a durable cleanup queue and actual caller regressions.
This does not complete the requested physical-filesystem admission replacement.
No startup setting installs the private capacity adapter.

Base: `d75d77843ee4c57427983f0ca8a0b7db29bc4f5c`, tree
`d1828b0e7cfa27bc43f6ed9e4aec5762b201f7c2`. Previous Decision, sampler rejection,
Apply/Resolve, accounting policy, envelope and continuous model files are
unchanged. The final source identities and commands are in the review manifest.

The Review3 evidence ZIP was referenced by the supplied documents but was not
available at the supplied local locations. The user was asked for its location.
The three tests here were reconstructed from the stated interleavings, **not**
represented as the original drop-in files. All three reached assertion FAIL on
the pinned full Go/FFI graph before repair. Initial fixture compilation mistakes
are separately retained and do not count as red evidence.

## Caller contracts and evidence

| Issue | Before | Repair | Evidence / limit |
|---|---|---|---|
| R3-01 | Rejected second Begin changed a still-live writer to resident | Begin issues an invocation-specific execution capability. The wrapper pins its reservation and only that capability can report its synchronous return. Unsupported type, replacement, rejected/unknown Start has no such capability. | Actual Claim/Acquire/generateSDR, real ledger and temporary filesystem; native body/index/free-space are substitutes. A blocked first native substitute remains running after the rejected second invocation. |
| R3-02 | Ready cancellation synchronously waited in storage release and sync.Once | releaseLocal is non-storage cleanup. Claim and pre-entry cleanup run in preparation workers; entered cleanup runs in the task worker. Quarantine recovery is capped at four workers per task type. | Actual scheduler drain, registry/preempt/shutdown calls; cleanup barrier stays blocked while another actual task dispatch enters. Nil-backend legacy Storage.Claim/release is covered. No claim that arbitrary kernel I/O is interruptible. |
| R3-03 | Storage error was log-only; admission and reservation were removed | Preserve release errors, exact reservation/execution identity, post-entry as well as pre-entry quarantine, and automatic bounded retry. Cancel/Returned intents can be handed to an exclusive durable cleanup journal. | Actual reservation retention and scheduler recovery tests; real journal rename/fsync, restart and independent-process crash tests. The optional journal is exercised through the test-installed caller backend, not installed on a production storage root. |

An entered admission remains observable until storage cleanup finishes or is
durably handed off. Active Do records do not consume the pending-admission cap;
failed cleanup records do. Local CPU/RAM/slot/pacing permits are released once,
without claiming a durable budget refund. Task completion persistence still runs
outside the scheduler and retains the existing completion CAS policy.

Normal scheduler duplicate suppression is a **different claim** from the direct
duplicate wrapper test: considerWork filters both running handles and pending
admissions; the existing batch/rediscovery test exercises this filter. The normal
SDR and SDRKeyRegen Do paths each call GenerateSDR once. These observations do not
prove that a duplicate invocation is reachable through the normal scheduler,
nor that two native writers passed scratch guards. The direct public caller
contract must nevertheless not certify another invocation's termination.

Repeated Acquire within one private invocation reuses its capability. Acquire
after that invocation returned is rejected. A replaced task reservation cannot
redirect scratch bookkeeping or be removed by an old release callback. Duplicate
TaskStorage.Claim no longer overwrites a live local reservation.

Returned occurs after generateSDRBody and its defers, including the existing
scratch writer close / native execution lease, have returned. No defer fabricates
Returned on panic/Goexit. Cancellation or task/owner loss is not termination.
An unknown Start retains running responsibility and cannot launch a replacement
native body from Resolve. Actual native and parent-death lease behavior was not
executed here; its existing implementation was not changed.

## Durable cleanup ownership

`CleanupJournal` is intentionally limited to Cancel/Returned, not a second
allocator. It binds host/filesystem identity, stable client, lock device/inode,
sequence and original sector/token/op payload in a checksum-protected file.
Intent is atomically renamed and synced before acknowledgement. One OS-locked
issuer and one background worker serialize Apply/Resolve. An unresolved request
keeps the same client/sequence and cannot be bypassed by inventing a new issuer.
Pending diagnostics distinguish not-issued from issued/unknown operations.

Recovery first Resolves the exact request. It applies a still-missing cleanup
only when appropriate; it never reserves, starts native work, performs a file
deletion, or proves a native process dead. Running cancellation is a no-op, not
Returned. Published cleanup with a lost acknowledgement is recovered across a
real child-process exit, without reapplying it under a new sequence.

`Durable=true` means accepted recovery responsibility, **not** an already-applied
refund. Journal write failure is not a handoff: caller quarantine and the exact
execution capability remain for retry. If all recording fails and the process
then dies, the running ledger commitment is retained; an unrecorded native
return cannot be reconstructed from a timeout or lack of a task. This requires
the still-unimplemented production execution/reconciliation contract, not a
relaxation of cleanup proof.

The queue is bounded at 1,024 outstanding intents and fails closed if full.
That is a recovery bound, not an admission quota or a new configured SDR limit.
Per-client lookup/cleanup metadata is not a native-liveness authority.
Opening missing/corrupt state never resets an issuer. A mismatched token remains
an error, not authority to modify a new execution. Closing a journal may wait
for its I/O worker; it must never run in the scheduler event loop.

## Actual production writer matrix — not closed

These paths were checked again against the candidate. There is no claim that
adding a journal makes physical materialization credit safe.

| Actual path | Current coverage | Missing implementation |
|---|---|---|
| TaskStorage.claim / storageProvider.AcquireSector / GenerateSDR | Invocation ownership, asynchronous admission, retention/retry tested with the private adapter | Installable backend, production Reserve/Start intent journal, durable preparation before pacing commit |
| paths.path.stat / Local.AcquireSector / ReserveSDR | Existing profile and selection behavior preserved | Physical quota/MaxStorage-aware admission and one verified authority across aliases |
| sharedcapacity.OpenPhysicalFilesystem / ExclusiveFileBytes | Existing XFS UUID/Bavail/FIEMAP primitives unchanged | Pinned root/boot/cohort lifecycle and actual mutation-fenced production Sample |
| SealCalls.TreeD / TreeRC | Existing code unchanged | Credit fence around Go TreeD writes, reflink/truncate/copy fallback, native PC2/C1, and error tree removal |
| Local.GeneratePoRepVanillaProof | Direct standard SealCommitPhase1 verified in source | Native writer lease; it bypasses SealCalls' storage provider |
| SealCalls.FinalizeSector / GenerateUnsealedSector | Required drain path preserved | Unsealed generation, ClearCache and copy removal under the shared mutation/credit protocol |
| SealCalls.MoveStorage / Local.MoveStorage / Remote.AcquireSector | Required drain path preserved | Destination-first commitment and source/copy reconciliation; task return is not physical reclamation |
| Local.Remove/RemoveCopies, remote HTTP removal, sdrscratch cleanup | Existing deletion authorization preserved | Common mutation generation/fence with sampling; do not substitute the ledger lock for this |
| ffiselect child execution | Existing ExtraFiles/cmd.Wait and scratch/native boundaries preserved | Capacity lease inheritance and actual return reconciliation across parent death |
| Stash, Snap/unseal/PDP/piece and other root writers | Unchanged | Before-I/O root-scoped routing/rejection without disabling standard Finalize/Move drains |

Concrete unsafe enabling interleaving: sample exclusive extents; an unfenced
truncate/unlink frees them; sample Bavail; admit using both new free space and
stale credit. A pinned FD, an after-stat comparison, or the accounting flock does
not close the cross-call mutation window. Merely connecting TaskStorage, blocking
every non-task Acquire, or assigning zero credit to every active writer would
not satisfy the requested useful/safe replacement. None is enabled as a solution.

Actual two/four-process Claim→SDR→Tree→Finalize→Move cohorts, and healthy-WIP
4 / 6+6 continuous caller refill, are **not implemented**. The existing model's
TreeRC 15/28 minute waits of 190/450 are retained as model limitations; no timing,
envelope, margin, slots or pacing were lowered to manufacture a throughput PASS.
The present work is therefore a partial caller repair, not closure of Rework3.

## Validation boundaries

The archive distinguishes intentional assertion red, fixture/compiler failures,
normal/race regression, source review, and NOT RUN. Commands use Go 1.26.1 and the
pinned FFI artifacts with `cgo,fvm,nosupraseal`, a sanitized environment, no DB
credentials/integration opt-ins, and finite test timeouts. Darwin linker warnings
about the cached archive's newer macOS build target are preserved.

Existing Apply/Resolve/Decision/sampler regressions and source are preserved.
Cleanup tests use actual local filesystem persistence and independent OS child
processes but a synthetic free-space oracle, not XFS/native measurements. Caller
tests use actual Go caller/scheduler code with a fake DB/index/native body.

The prior component-wide race timeout remains prior failed evidence; subsequent
passes cannot establish its root cause. Full logs and run conditions, including
any new full-suite timeout, are retained. No test timeout was raised internally
or failed test removed to hide this issue.

Linux/XFS/native, full writer-cohort throughput and production validation remain
NOT RUN; the first three are not equivalent to cross-compilation. No schema,
configuration model, generator inputs, public generated interfaces or generated
outputs changed. Targeted canonical fiximports and scoped build/vet/pinned lint
are recorded; full generation is not represented as PASS.

## Future cohort transition (design only)

1. Close the matrix and validate physical bounds, quota/MaxStorage and every root
   alias first. Do not deploy this checkpoint.
2. Inventory the full filesystem cohort (two processes on a single host, four on
   a dual host), including non-SDR writers. Establish independently verified
   drained/bootstrap or reconciled resident commitments before creating authority.
3. Move the entire cohort to one protocol. Never mix legacy fake-cap writers
   with new physical admission on the same filesystem. Preserve profile names,
   physical/virtual distinction, four and six-plus-six slots, and configured pacing.
4. Restart recovery must retain running/unknown commitments, replay only owned
   cleanup intents, and never use heartbeat/task absence as native termination.
5. Rollback is likewise cohort-wide after stopping new admissions and reconciling
   in-flight/resident responsibility. Do not delete/reinitialize a ledger to make
   capacity appear available. No rollout/rollback command is authorized here.

Personal, pr-candidates, previous candidates and archives are preserved. No
production/server/DB access, remote write, push, PR, deployment or file cleanup
outside owned test fixtures was performed.
