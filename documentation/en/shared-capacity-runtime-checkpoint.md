# Shared-capacity runtime integration checkpoint (incomplete)

## Disposition

This is a development checkpoint, **not a deployment candidate**. It extends the
reviewed `4651bc520500df55ae46d9dc6567614cf77b7fca` accounting component. It does
not replace the personal filesystem capacity policy.

| Question | Answer |
|---|---|
| Accounting component complete? | Review2 decision-loss fix implemented; earlier sampler-error and Apply/Resolve safeguards preserved. Correctness is conditional on the sampler/writer contract. |
| Actual caller integration complete? | **No.** Claim/Acquire/SDR and asynchronous preparation seams are implemented and exercised, but only a test adapter is installed. |
| Supported writer set closed? | **No.** The downstream/native/copy/cleanup paths below still need integration. |
| Production sampler/mutation protocol complete? | **No.** Linux measurement primitives are not a safe all-writer sampler. |
| Linux XFS execution run? | **NOT RUN** in this checkpoint. |
| Native type-8 execution run? | **NOT RUN.** Native bodies in caller tests are substitutes. |
| Production rollout authorized? | **No.** No startup switch installs this private integration seam. |

In particular, the two-process last-permit test is not a full two/four-process
SDR + Trees cohort test. Neither it nor the continuous model proves the requested
4 / 6+6 throughput with real filesystem credit. This remains an implementation
gap, not merely an operator-validation gap.

## R2-02: measured capacity denial

The reproduced 100-GiB free / 200-GiB additional / 32-GiB margin request was
correctly denied but returned a zero `Outcome.Decision` before the repair.
`Apply` now copies the measured decision before returning the denial. The new
`MeasurementValid` field is set only after all sample and arithmetic validation.
It is distinct from the private validated-pressure disposition; returning the
`ErrCapacity` sentinel from a sampler still cannot authorize a resident request.
Denial creates neither a token nor a ledger mutation. Invalid samples can contain
partial diagnostics, but are never marked measurement-valid.

The review's Go attachment was unavailable locally. The red test was reconstructed
from the exact documented inputs, not represented as the original attachment.
The final regression additionally checks the validity flag and unchanged ledger.

## R2-03: duration sensitivity, not native performance

All rows below are 24-hour **virtual models**, with 15-second polling and an exact
materialization oracle. The existing 44m / 25m45s model cadence is unchanged; it
is not a measurement of, or change to, the configured 43m45s / 25m20s profiles.
Peak envelope, 64-MiB assumed allowance, margin and slot counts were not reduced.

| Condition | Starts / SDR completions | Capacity-wait polls | Largest shortfall (bytes) |
|---|---|---:|---:|
| Single, TreeRC 2m | 33 / 29 | 0 | 0 |
| Single, TreeRC 15m | 32 / 28 | 190 | 511,791,267,104 |
| Single, TreeRC 28m | 31 / 27 | 450 | 511,791,267,104 |
| Dual, TreeRC 2 / 15 / 28m | 56+56 / 50+50 in each case | 0 | 0 |
| Single, 95% usable, TreeRC 15m | 27 / 24 | 1,070 | 146,443,905,856 |
| Single, 100 GiB background, TreeRC 15m | 27 / 24 | 1,070 | 93,818,088,256 |
| Single, PreCommit/Move 30m, TreeRC 15m | 27 / 24 | 1,064 | 511,791,267,104 |
| Single, WaitSeed 150m, TreeRC 15m | 26 / 23 | 1,352 | 511,791,267,104 |

These sensitivity assertions now expect conservative waiting where it actually
occurs. They preserve the no-overrun assertion; they do **not** turn the failed
zero-denial performance target into a PASS. Completion counts above are SDR
completions, not full pipeline completions. Earlier WIP remains during refills.
Existing per-stage and combined stall/recovery model scenarios remain in place.

The model retains the whole 525,347,361,248-byte envelope until MoveStorage.
Six envelopes plus margin leave only 13,556,094,144 bytes on nominal 3.20 TB.
This is not a verified XFS/native upper bound. Two possible reductions differ:

* Reducing after `FinalizeSector` could release future SDR/tree work only after
  synchronous native return, physical file inspection, and mutation fencing.
  `after_finalize` alone proves neither free bytes nor that another writer has
  stopped. Move/fetch may still create another copy and needs a destination grant.
* Restricting concurrent TreeRC work can reduce overlap only with a proven phase
  envelope and a cross-process stage permit. Silently reducing SDR slots or
  pacing instead would change the requested target and is not implemented.

Neither alternative is activated here. No model time or budget was tuned to
hide the longer-TreeRC shortfall.

## Implemented caller seams and evidence

| Function / file | Added boundary | Executed test evidence | Limit |
|---|---|---|---|
| `TaskStorage.claim` / `PrepareStorageClaim` | Acquire paths, then optional admission; no simultaneous legacy `ReserveSDR` charge; unwind pre-entry claim | Actual Claim, denial, cancellation, last-permit subprocess race | Existing path selection still uses legacy stats. No production admission backend is installed. |
| `taskAdmission.prepare` | Optional storage preparation runs in the existing bounded admission worker | Storage held at a barrier while scheduler drain returns; denial/late cancellation release resources without recording execution failure | Does not close native Start durability or all existing synchronous caller paths. |
| `storageProvider.AcquireSector` | Optional claim Begin precedes fetch/write | Actual reserved Acquire used by SDR fixture | Non-task Acquire is deliberately rejected only in the private test seam, **not** offered as an acceptable production downstream policy. |
| `SealCalls.generateSDR` | Captures this execution's claim; records Returned only after the synchronous body and its deferred cleanup return | Normal, native substitute error, post-publication error, cancellation, panic, Goexit | Returned retains residency. No deletion authorization is inferred; panic/Goexit retains responsibility. |
| `capacity_admission_test.go` | Apply-based test adapter and durable-intent fixture | Two independent processes compete for one permit; exactly one writes | Synthetic free-space oracle, 64-MiB fixture envelope, local temporary files; **not** physical type-8 accounting. |

The test adapter's intent file is not a production restart journal: it has no
exclusive cross-process client lease or outcome-resolution recovery. The
component's independently tested Apply/Resolve implementation remains available,
but actual caller post-rename recovery is **not implemented** by this fixture.

The nil-backend path keeps the existing capacity profile, local reservation,
slot limits, pacing, scratch authorization and UI behavior. No public config,
dependency, migration, build/backend selection or personal ref is changed.

## Concrete remaining closure work / self-review blockers

| Path | Actual behavior requiring a common protocol | Current disposition |
|---|---|---|
| `paths.path.stat`, `Local.AcquireSector`, `Reserve`, `ReserveSDR` | Profile replaces physical free; per-process reservation/selection counts remain | No production physical admission replacement. Do not activate only the TaskStorage seam. |
| `SealCalls.TreeD`, `TreeRC` | Tree writes, reflink/copy fallback, truncate, native PC2/C1 checks, error tree removal | No capacity mutation lease. Existing SDR-label immutability is not whole-cache immutability. |
| `Local.GeneratePoRepVanillaProof` | Direct standard `ffi.SealCommitPhase1` bypasses SealCalls' storage provider | No shared admission/return lease here. |
| `FinalizeSector`, `GenerateUnsealedSector` | Generate/move unsealed, `ffi.ClearCache`, copy removal | Required drain path, not safe to always reject. Needs fenced lifecycle/physical reconciliation. |
| `SealCalls.MoveStorage`, `Local.MoveStorage`, `Remote.AcquireSector` / fetch | Destination allocations and copy, source removal, retry/resume | Destination must be reserved first; source responsibility cannot be refunded merely because the task returned. |
| `Local.Remove` / `RemoveCopies`, HTTP removal, scratch cleanup | Can unlink credited files independently of accounting lock | Must join the same credit-mutation fence without weakening current deletion authorization. |
| `Local.StashCreate`, Snap/unseal/PDP/piece and other non-supported writers | Paths can write without TaskStorage | Root-scoped before-I/O rejection/routing not implemented; global rejection would also break required drains. |
| `OpenPhysicalFilesystem` / `ExclusiveFileBytes` | XFS identity/FIEMAP/stable-stat primitives only | No production sampler collecting pinned, deduplicated, mutation-fenced credit; quota/MaxStorage and mount/boot reconciliation remain missing. |
| `ffiselect.call` | Child process, `ExtraFiles`, `cmd.Wait` | No inherited capacity execution lease. Parent death must not be equated to termination of a surviving child. |

A specific unsafe interleaving remains: sampler observes allocated credit; an
uncoordinated truncate/unlink frees those blocks; fresh Bavail then includes the
freed bytes; a grant subtracts the stale credit as well. An FD and a stable stat
before this interleaving do not prevent double admission. The accounting lock
does not currently fence these actual mutation paths. Enabling the prototype or
assigning zero credit everywhere would not satisfy the requested safe and useful
runtime replacement, so neither is presented as a solution.

Also still open: the new claim's durable `Begin` occurs at Acquire, after scheduler
Do-entry. A Begin persistence error can therefore occur after pacing commit.
Before production integration, the reservation/start protocol must move this
failure boundary before Do-entry without pretending an early Start receipt
proves execution. There is no test/claim here that this missing contract is closed.

## Validation and next boundary

The accompanying archive has full commands, return codes and logs, including
the intentional R2-02 red assertion and the corrected green run. Unit/caller/race,
build, vet, pinned lint and import-format outcomes are recorded independently.
Database integration opt-ins are absent; no production connection is used.
Existing unsafe default-DB tests are excluded by focused selection, not weakened.

Observed validation so far: component normal PASS; new caller/admission normal
and race PASS; existing selected scratch/pacing/admission regressions PASS;
full scratch race PASS; scoped build/vet/pinned lint PASS. The component-wide
race command **failed** in the pre-existing dual cohort role model with a lock
deadline and barrier timeout while other validation jobs were also running.
The exact failed test, with unchanged deadlines, passed three consecutive
isolated race runs. This does not erase the initial failure or prove its root
cause. No race-detector data-race report occurred in those logs. See the final
manifest for exact commands, source identity and any subsequent outcomes.

Full generation is not an affected check: no generator inputs, public generated
RPC/interface schema, config model, migration or generated output changed.
The canonical targeted import formatter was run; the known unrelated full
generation environment is not represented as a PASS. No Linux/CUDA binary was
built or installed, and no database tests were executed.

Linux XFS and native type-8 execution remain NOT RUN. This is **not** the sole
blocker: the implementation gaps above must first be closed, with actual
Claim/Acquire/Do/Tree/Finalize/Move tests under a common adapter. The requested
healthy-WIP four/twelve-active caller test and full two/four-process cohort test
have not been implemented and must not be inferred from the model.

Whole-filesystem rollout remains a separate future approval: inventory every
alias/storage ID and all two/four participating processes; stop new starts and
establish a reconciled cohort before changing authority; retain resident
commitments across restart; never erase/recreate a ledger to manufacture free
space. A rollback must also transition the entire cohort, not leave legacy
fake-cap writers mixed with a new authority. Preserve existing profile names,
configured slots and pacing. This checkpoint provides **no rollout commands**.
