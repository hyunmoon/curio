# SDR retry verification

Run ordinary file lifecycle and accounting tests with existing verified native
link artifacts. The tests replace SDR computation with synchronous file writers;
they do not execute CUDA/native sealing. Never copy Darwin archives to Linux.

```sh
go test -tags=cgo,fvm,nosupraseal ./lib/ffi ./lib/paths \
  -run '^TestSDR' -count=1 -timeout=3m
go test -race -tags=cgo,fvm,nosupraseal ./lib/ffi ./lib/paths \
  -run '^TestSDR' -count=1 -timeout=3m
```

On a **disposable non-production Linux checkout**, these same commands exercise
the actual Linux Go no-replace wrapper, xattrs, file cleanup and subprocess
receipt reuse. Record `uname -a`, filesystem type, Go version, source HEAD/tree,
native link-artifact identity and complete output. This is separate from actual
native/XFS failure timing. An unsupported filesystem must fail, not fall back to
overwriting output.

## Actual caller/database tests

Use only an owned disposable database at literal `127.0.0.1`. No normal Curio,
HarmonyDB or libpq connection settings may be inherited. Create a dedicated
`curio_test_sdr_retry` database and `curio_sdr_retry` user there. The fixture uses
the actual pinned migration runner in a random owned `itest_*` schema, asserts
the connected address/schema, and cleans that schema. Run packages sequentially
(`-p=1`); unrelated concurrent catalog creation can invalidate Yugabyte catalog
snapshots before the test starts.

Prepare an overlay in a new directory outside the source tree:

```sh
python3 scripts/sdr-retry-overlay.py --output ../sdr-retry-overlay
```

The overlay substitutes only the synchronous `ffi.GenerateSDR` function argument
and enables the test adapter guard. Acquisition/declaration are isolated fixture
adapters; the production `SDRTask.Do`/`TaskUnsealSdr.Do`, receipt/publication,
ticket selection, real pipeline queries and completion UPDATEs execute. Storage
reservation arithmetic is covered separately through actual Local methods.
Without the overlay, the guarded tests refuse to call native proofs.

Example invocation (set the disposable server's chosen port explicitly):

```sh
env -i PATH="$PATH" HOME="$HOME" TMPDIR=/tmp \
  GOENV=off GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off CGO_ENABLED=1 \
  CURIO_SDR_RETRY_ITEST=1 CURIO_SDR_RETRY_ITEST_HOST=127.0.0.1 \
  CURIO_SDR_RETRY_ITEST_PORT="$SDR_TEST_PORT" \
  CURIO_SDR_RETRY_ITEST_DATABASE=curio_test_sdr_retry \
  CURIO_SDR_RETRY_ITEST_USER=curio_sdr_retry \
  go test -p=1 -overlay=../sdr-retry-overlay/overlay.json \
    -tags=cgo,fvm,nosupraseal,sdr_retry_itest \
    ./tasks/seal ./tasks/unseal \
    -run '^TestSDR(Caller|KeyCaller)PublishedDBRetry$' -count=1 -v -timeout=3m
```

Add only verified **local build** variables if required for native linking.
Never append production connection variables. Each test forces the actual
completion UPDATE to fail via a fixture trigger after file publication, removes
only that owned trigger, creates a new storage handle and verifies retry without
another native call. The SDR test advances the mock chain head and verifies the
original ticket/epoch; unseal uses fixed historical metadata. This proves neither
stale stage-UPDATE fencing nor native termination. Production remains NOT RUN.
