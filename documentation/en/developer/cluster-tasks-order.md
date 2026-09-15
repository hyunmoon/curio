# Cluster Tasks execution sections

The bounded `CurioWeb.ClusterTaskSummaryLimited` response is presented as:

| Section | Evidence | Order / time |
| --- | --- | --- |
| Running | Owned, nonempty current attempt token, `do_entry` provenance, nonfuture start | Whole-second Took descending, numeric task ID ascending on ties; zero is valid |
| Awaiting start | Owned, `claimed` or `prepared`, no execution timestamp | Ownership timestamp ascending, missing timestamps last, then task ID; Took is `—` |
| Unknown | Owned but incomplete, future or contradictory execution evidence | Ownership timestamp ascending, missing timestamps last, then task ID; Took is unknown with a reason tooltip |
| Pending preview | No owner | Existing display priority, posting timestamp, then task ID; Waiting is age since posting |

The claim trigger can clear the token before preparation allocates a new one.
That is a legitimate Awaiting start state, not Running. A claimed/prepared row
with an execution timestamp is contradictory and is Unknown. No ownership,
posting, migration/backfill or old History timestamp is substituted for Took.
Do entry is recorded by the worker; it is not evidence of native liveness or
the exact FFI entry instant. Stale workers are not diagnosed by this view.

## Snapshot and compatibility

Classification, numeric sort keys, rows and `SectionTotals` use one SQL
statement snapshot and timestamp. Selection happens **before LIMIT** in the
order Running, Awaiting start, Unknown, Pending. The combined selected-row cap
remains 500; pending defaults to at most 30 and can be disabled. Task/background
filters affect both rows and totals. A limit bounds returned rows, not scans.
No history scan or per-task polling is introduced. The existing task-type
metadata query and bounded, selected-only SpID batch enrichment remain.

`Running`, `RunningTotal`, row `State`, `AgeSeconds` and task-type `Running`
counts retain their legacy **owned-task** semantics. Additive `SectionTotals`
contains `Running`, `AwaitingStart`, `Unknown`, and `Pending` full-set counts.
`OwnershipStartedAt` is an ordering field only. Existing clients need not
consume these additions. New clients split the legacy arrays using ownership,
`TookState`, token and numeric Took; contradictory payloads are not Running.
An older server without section totals displays “N shown; total unavailable”
for owned sections, never the legacy owned total as an execution count.

The frontend sorts copies of accepted immutable snapshots. Adjacent coalescing
is applied afterward, within a section only. Cached groups are reused during
monotonic-clock ticks. A new snapshot, including a retry with lower Took,
replaces the ordering. Pause, hide/disconnect, failed refresh, stale/partial
snapshots and request-generation protection retain the existing lifecycle.

## Verification

Database-free:

```sh
node --test web/test/cluster-tasks-*.test.mjs
go test -tags=cgo,fvm,nosupraseal -count=1 -timeout=3m ./web/api/webrpc
go test -race -tags=cgo,fvm,nosupraseal -count=1 -timeout=3m ./web/api/webrpc
go vet -tags=cgo,fvm,nosupraseal,integration ./web/api/webrpc
```

`TestClusterTaskSnapshotSQL` and `TestClusterTaskOrderSQL` are opt-in real SQL
tests, built with `cgo,fvm,nosupraseal,integration`. Run only against an owned
disposable database. Clear inherited DB settings; set
`CURIO_TASK_ATTEMPT_ITEST=1` and dedicated `CURIO_TASK_ATTEMPT_ITEST_HOST`,
`PORT`, `DATABASE`, `USER`, and optionally `PASSWORD` (all with that prefix).
The host must be literal `127.0.0.1`; load balancing is disabled. The tests
create and clean a random owned schema, applying the existing Harmony task,
ownership and attempt SQL files. They assert both connections' schema, use a
60-second context, 5-second statements and 2-second locks, and run the real
summary query. They are query tests, **not** a full migration-runner, scheduler
or native execution test. The large fixture contains 510 owned task rows and
32,108 unowned task rows; an Indexing task previously excluded by ownership /
type selection must lead the bounded result. Plans and JSON size are logged.
PostgreSQL execution does not establish Yugabyte performance.

`web/test/cluster-tasks-browser.mjs` uses real Chromium and Lit with intercepted
offline requests. Supply cached `PLAYWRIGHT_MODULE`, `CHROMIUM_EXECUTABLE`,
`LIT3_FIXTURE`, `CLUSTER_SCREENSHOT` and `CLUSTER_COALESCED_SCREENSHOT`. Optional
`CLUSTER_BASELINE` serves static files from a preserved Git revision for the
before image. It checks sorting, all sections, counts, coalescing, state-label
width, Pause/error/retry and late-response rejection. It never calls a Curio
service. Browser results and SQL results must be reported separately.

## Application boundary

### Owner width and drawer sizing

The two Cluster Tasks drawer call sites opt into `cluster-tasks-drawer` sizing;
other drawers are unchanged. Desktop panels allow up to 64rem. The overlay keeps
viewport margins; below 1100px, the Overview push panel becomes an overlay rather
than being squeezed beside the navigation sidebar. Horizontal overflow belongs
to each section's table wrapper, not the page or dialog. Automatic table layout
allows long hostname/IPv6 owners to contribute their intrinsic width; no address
or node link is shortened, rewritten or substituted.

`web/test/cluster-tasks-owner-browser.mjs` loads the real Drawer, navigation shell,
both call-site HTML structures, bootstrap/main/dark CSS and local Inter fonts.
All HTTP and WebSocket calls are intercepted offline. It checks computed styles,
link/cell/wrapper bounding boxes, scroll reach, controls/close-button bounds,
all four sections and coalescing at 1280, 640 and 390px. Set the existing browser
environment variables plus `CLUSTER_OWNER_OUTPUT` (an output directory). Optional
`CLUSTER_BASELINE` serves a Git tree and expects the desktop visibility assertion
to fail; it does not call a screenshot alone a regression result. This fixture
does not load unrelated pipeline components or connect to any actual backend.

### Deployment scope

Only the WebRPC service and its static UI need this change. No worker,
scheduler, attempt writer/CAS, SDR, pacing, scratch, schema or index change is
included. Static files use the existing embedded `http.ServeContent` path;
there is no new asset version manifest. Deploy the API and static files
together when separately approved, then reload the page (hard refresh if an
intermediate browser/proxy cache retains assets). Verify the actual web
service unit and any colocated roles before restarting it; do not restart the
worker fleet for this display-only change. Mixed new-UI/old-API totals remain
explicitly unavailable. No deployment or production execution is implied by
the local tests.
