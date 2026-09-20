# Shared capacity R4: bounded initial cleanup and retained acknowledgement

## Status and scope

**PARTIAL development checkpoint, not an operational replacement.** R4 caller
and journal regressions are repaired. Production physical sampling and the
supported writer cohort are not integrated. Do not install/enable the private
capacity adapter. No settings, margins, envelopes, slot limits, pacing, scratch
policy, dependency pins, migrations or UI were changed.

Base: `bf210cfa04f2f8154ce523ec79d97cbec416dd6e`, tree
`47be263c8e3003b712710dfaee451f467c276f02`. The external review manifest pins
the resulting commit/tree, commands, test source hashes and artifact checksums.

Both supplied R4 Markdown documents were read. The evidence ZIP mentioned in
them was not present in the accessible supplied directories; its location was
requested. The tests here are independently reconstructed full-repository
regressions, **not the original R3/R4 drop-in files**. Those original drop-ins
remain NOT RUN. Prior R3 tests already in the repository were executed again.

## R4-01: cleanup liability before resource return

The actual dispatch path marks `executionReturned` under the admission mutex
before returning Max/CPU/RAM/running-handle resources and before its first storage
I/O. The scheduler counts that liability even while `finished=false` and the
first cleanup has not returned. Active Do is distinct and still uses the original
resource/Max limits. No storage I/O was put back in the scheduler or cancellation
callback. Existing four-worker quarantine retries remain bounded and serialized
per admission. No extra goroutine queue was introduced.

Per task type, at most 100 pending/returned/quarantined admissions can be newly
budgeted. Already-active Do workers can transfer into this budget concurrently;
that overhang is bounded by the existing active Do resource/Max limits (four or
six in the specified SDR profiles). This is not a new global native-process cap.
Zero-cost/unlimited task configurations do not acquire a new finite active-Do
limit from this change. Completed storage handoff releases this local liability,
not the authority's durable capacity commitment. Completion DB persistence is
still outside the scheduler and uses unchanged completion CAS code.

Self-review also found headroom can become exhausted while CanAccept executes.
The second count is checked before claim or pacing reservation; a nonpositive
limit is never sent to claim. This is part of the same liability transfer fix.

Evidence: real `considerWork/dispatchAdmission/prepare/runregistry/release`
paths; DB store, storage callback and Do body are substitutes. Before: 105 Do
returns blocked in initial cleanup, pending=0, active=0. After: the 101st is
refused, pending=100, active=0; unrelated dispatch proceeds, releasing the
barrier delivers completion callbacks and permits refill. Four/twelve active Do
fixtures distinguish active execution from returned cleanup and cover drain.
Completion persistence in these fixtures uses the existing test recorder, not
a DB/native execution claim. Existing completion model tests are separate.

## R4-02: exact old success is not permission over the new token

The journal atomically moves an applied job into a bounded terminal evidence
set before advancing to another cleanup request. The record retains the exact
operation, sector, old token, journal client and authority request sequence.
Later requests may replace the authority's latest per-client receipt; terminal
journal evidence survives. No-op observations are recorded with sequence zero
and do not consume an authority sequence. Absence of a sector alone is no longer
accepted as proof of an old cancel. Unknown/mismatching tokens without an exact
retained record remain errors.

Submit of an exact retained request returns Durable=true, Completed=true after
directory sync, without mutating the authority. It does not replay the old
mutation, invent a new request or refund a new token. Completed means handled,
including a valid no-op; it does not always mean released bytes.

The total of pending plus unacknowledged terminal records is capped at 1,024.
Existing requests remain queryable when full; new requests fail with an explicit
caller-quarantine error. There is no timeout-based eviction.

The explicit acknowledgement protocol is:

1. Persist caller intent; Submit and retry the exact payload until durable.
2. Durably retire the caller's right to repeat Submit, retaining the recovery ID
   as an acknowledgement intent.
3. Call Acknowledge(ID); retry only Acknowledge after an ambiguous result.
4. Pending mutations remain journal-owned. Completed evidence can be collected.

Acknowledging a pending record marks it but cannot remove its mutation. Repeated
Acknowledge of a collected ID only syncs the directory; it proves no completion.
A caller crash before retirement keeps terminal evidence. A crash after retirement
must recover the acknowledgement intent. This is the bounded GC contract, not a
claim that existing production callers already implement persistent retirement.
The prototype adapter/helper has no production Reserve/Start/retirement outbox;
it must not be enabled as-is. Without caller acknowledgement its terminal set
eventually fills, deliberately fails closed, and does not evict uncertain work.
This missing caller protocol is an implementation gap, not a passing soak test.

Journal format v2 reads v1 outstanding jobs and upgrades on publication. Old v1
readers reject v2 rather than silently discarding new terminal records. Already
discarded v1 historical evidence cannot be reconstructed. Do not downgrade a
journal in place or reset its sequence to recover capacity.

Evidence includes deterministic before/after-rename failures, new tokens,
replacement of the latest cleanup receipt, sector reconciliation/removal,
reopening the journal, pending and terminal acknowledgement write errors,
negative token controls, and a finite retention boundary. Removal proof in the
fixture is synthetic; no real data is deleted. Existing independent child-process
crash/recovery tests also run. None proves power-loss/XFS durability or native
termination.

The caller regression uses actual TaskStorage, AcquireSector, generateSDR and
release. It substitutes native/index/sample and loses the handoff response after
Submit. Follow-up reserve uses the actual authority API, not an actual TreeRC
execution. Retrying the old release clears its exact pending return and leaves
the new entry/receipt unchanged. Production reachability requires the unfinished
backend installation; it is not demonstrated by this fixture.

## Production integration audit: still open, not just untested

Continued source inspection found the following concrete blockers. They were not
papered over by partially installing the optional adapter or zeroing all credit.

| Production path | Verified current behavior | Required closure |
| --- | --- | --- |
| NewSealCalls / TaskStorage.claim / storageProvider.AcquireSector | `capacity` is assigned only by test fixtures; Begin occurs at Acquire, after scheduler Do-entry | Durable Reserve/Start/retire outbox, preparation before pacing, invocation/native lease recovery |
| Local.path.stat / ReserveSDR / AcquireSector | Existing virtual profile and process-local reservations retained | Physical quota/MaxStorage scopes, aliases and full-cohort authority |
| OpenPhysicalFilesystem / ExclusiveFileBytes | XFS identity, Bavail and FIEMAP primitives; no writer fence | Real production sample across allocation/free observations, quota checks, mutation barrier extending through admission commit |
| TreeD / TreeRC | TreeRC reflinks TreeD into sealed, truncates, falls back to copy, calls PC2/C1; error cleanup removes trees | Per-stage credit lifetime and promised growth, native lease, mutation participation; long global exclusive native lock is unacceptable |
| Local.GeneratePoRepVanillaProof | Standard path calls FFI SealCommitPhase1 directly after Local.AcquireSector | Must join native lifetime/credit protocol; SealCalls-only coverage misses it |
| FinalizeSector / GenerateUnsealedSector | Nil-task Acquire, unsealed generation and ClearCache, copy removal | Preserve drain; current prototype rejects nil-task Acquire, so enabling it would block required drain |
| MoveStorage / Remote.AcquireSector | Source/destination allocations, copy/fetch, DropSector/Move/Declare; Move uses legacy void reservation Release | Destination-first reservation, exact error-returning cleanup ownership, actual physical reconciliation |
| Local.Remove/RemoveCopies / HTTP removal / scratch cleanup | RemoveAll and scratch unlink are outside capacity accounting | Existing deletion authorization plus common credit mutation protocol |
| ffiselect / native proof library | Child ExtraFiles does not carry a capacity lease; FFI delegates SDR/PC2 to pinned proofs library | Child lease inheritance, crash recovery and allocation behavior verification |
| Other root writers (Snap/unseal/stash/PDP/pieces) | No shared-root gate | Route or reject before first write without blocking supported drain |

An observed FFI wrapper call is not proof that native writes are append-only.
Native `seal::sdr` and PC2 delegate to pinned filecoin-proofs/storage-proofs-porep
19.1.0; their implementation sources were not available in the local Cargo cache.
No dependency was downloaded or upgraded. The local wrapper alone cannot justify
active-file materialization credit while allocation/truncation may overlap.

The open integration is **not implemented**, rather than a ready runtime waiting
only for Linux testing. Actual two/four-process production sampler + writer
cohorts and sustained healthy-WIP four/six-plus-six caller refills remain NOT RUN.
The existing two/four-process authority model is a different test with a synthetic
oracle. Prior TreeRC 15/28-minute model waits of 190/450 are preserved; model
cadences/envelopes/margin have not been shortened to turn them into a healthy
throughput claim.

## Verification and preserved history

Pinned Go 1.26.1, dependency graph and cached Darwin FFI, matching tags
`cgo,fvm,nosupraseal`, sanitized environment without DB credentials or integration
opt-ins. Normal/race, scoped build/vet/pinned lint and canonical fiximports are
recorded separately in the manifest. Linux compilation is not Linux execution.
No full production build, native, XFS or database execution is claimed. No
generator inputs/public generated interface changed; full generation is NOT RUN.

The initial red tests actually failed assertions. One green fixture needed an
event-channel consumer to model normal scheduler notification consumption;
otherwise refill blocked after the test's unconsumed channel filled. Its failure
is preserved. A first baseline overlay used a symlink path that Go did not use;
its accidental green is not red evidence. The physical-path overlay subsequently
produced both original assertions on the exact baseline source.

Earlier full-suite timeout and independent review's externally interrupted race
run retain their historical classifications. New PASS does not erase them.

## Cohort transition design only

Close every writer/sampler/retirement-outbox row before deployment. Inventory all
processes and root aliases on each physical filesystem: two on single, four on
dual. Bootstrap only an independently drained/reconciled cohort, not an empty
in-memory view. Adopt one protocol across the entire filesystem; do not mix old
fake-cap writers and new physical admission. Preserve the two profile names,
virtual-capacity policy, configured four/six-plus-six slots and pacing.

A rollback also requires whole-cohort quiescence and reconciliation; retain
journals, current/uncertain commitments and receipts. Do not delete a ledger,
reset an issuer, downgrade a v2 journal, or infer native exit from task absence.
No rollout/rollback command or production action is authorized by this report.
