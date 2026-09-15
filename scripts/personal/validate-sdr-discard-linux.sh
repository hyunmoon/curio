#!/usr/bin/env bash
# Only an owned disposable Linux host. No production units/config/storage.
# Build binaries as a normal user first, then invoke this script as root.
(
  set -u
  set -o pipefail
  if [[ ${CURIO_SDR_DISCARD_TEST_TARGET:-} != disposable || $(id -u) != 0 || $(uname -s) != Linux || $# != 2 ]]; then
    echo 'Requires disposable Linux root. Usage: script /absolute/sdr-scratch /absolute/sdrscratch.test' >&2
    exit 2
  fi
  helper=$1
  tests=$2
  for p in "$helper" "$tests"; do
    if [[ $p != /* || $p =~ [[:space:]%] || ! -x $p ]]; then echo "Invalid executable path" >&2; exit 2; fi
  done
  fixture=$(mktemp -d /var/lib/curio-sdr-review-XXXXXXXX) || exit 1
  chmod 700 "$fixture" || exit 1
  mkdir "$fixture/tmp" || exit 1
  echo "Disposable fixture and logs retained at $fixture"
  # No inherited Curio, DB, cloud credentials or integration opt-ins.
  env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin HOME=/root TMPDIR="$fixture/tmp" \
    CURIO_SDR_DISCARD_TEST_TARGET=disposable CURIO_SDR_REVIEW_LINUX=1 \
    CURIO_SDR_REVIEW_ROOT="$fixture" CURIO_SDR_REVIEW_HELPER="$helper" \
    "$tests" -test.v -test.count=1 -test.timeout=4m \
    '-test.run=^TestManagedLinuxReview(Accounting|Mounts|Connected)$' \
    2>&1 | tee "$fixture/result.log"
  status=${PIPESTATUS[0]}
  echo "Test exit status: $status; retained fixture: $fixture"
  exit "$status"
)
