#!/usr/bin/env bash
# Disposable Linux only; one freshly compiled test executable, no helper install.
(
  set -u
  set -o pipefail
  if [[ $# != 1 || ${CURIO_SDR_DISCARD_TEST_TARGET:-} != disposable || $(id -u) != 0 || $(uname -s) != Linux ]]; then
    echo 'Requires explicit disposable Linux root and one absolute test binary path.' >&2
    exit 2
  fi
  tests=$1
  [[ $tests == /* && $tests != *[[:space:]%]* && -x $tests ]] || exit 2
  for tool in systemctl mkfs.ext4 mount; do command -v "$tool" >/dev/null || exit 2; done
  fixture=$(mktemp -d /var/lib/curio-sdr-review-XXXXXXXX) || exit 1
  chmod 700 "$fixture" || exit 1
  mkdir "$fixture/tmp" || exit 1
  echo "Owned fixture and logs retained: $fixture"
  env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin HOME=/root TMPDIR="$fixture/tmp" \
    CURIO_SDR_DISCARD_TEST_TARGET=disposable CURIO_SDR_REVIEW_LINUX=1 \
    CURIO_SDR_REVIEW_ROOT="$fixture" \
    "$tests" -test.v -test.count=1 -test.timeout=4m \
    '-test.run=^(TestManagedLinuxReview(Accounting|Mounts)|TestPersonalLinuxReviewSameBinary|TestPersonalRequiresDelegationNotRootAssumption)$' \
    2>&1 | tee "$fixture/result.log"
  statuses=("${PIPESTATUS[@]}")
  result=${statuses[0]}
  echo "Test exit=$result; evidence=$fixture"
  [[ $result == 0 ]] || exit "$result"
  [[ ${statuses[1]} == 0 ]] || exit "${statuses[1]}"
  ! grep -q -- '--- SKIP:' "$fixture/result.log" || exit 1
  for name in TestManagedLinuxReviewAccounting TestManagedLinuxReviewMounts TestPersonalLinuxReviewSameBinary TestPersonalRequiresDelegationNotRootAssumption; do
    grep -q -- "--- PASS: $name " "$fixture/result.log" || exit 1
  done
)
