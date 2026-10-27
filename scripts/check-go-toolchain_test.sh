#!/bin/sh
# Adversarial tests for check-go-toolchain.sh.
#
# Expected results come from the requirement, not from the implementation:
# the eMudhra x509 distrust list (golang/go#82345) is present in go1.26.10+,
# go1.27.3+ and every later minor, and absent from everything else.
# The Dockerfile runs this file before it trusts the guard, so a broken guard
# fails the image build rather than silently passing every version.
#
# Usage: check-go-toolchain_test.sh [path/to/check-go-toolchain.sh]

set -u

script=${1:-$(dirname "$0")/check-go-toolchain.sh}
failures=0

# expect STATUS VERSION — run the guard on VERSION and compare its exit status.
expect() {
	want=$1
	shift
	sh "$script" "$@" >/dev/null 2>&1
	got=$?
	if [ "$got" -ne "$want" ]; then
		echo "FAIL: check-go-toolchain.sh $* exited $got, want $want" >&2
		failures=$((failures + 1))
	fi
}

# Fixed releases and everything after them on the same line.
expect 0 go1.26.10
expect 0 go1.26.11
expect 0 go1.26.100
expect 0 go1.27.3
expect 0 go1.27.4
expect 0 go1.27.10
# Newer minors branch from a master that already has the fix.
expect 0 go1.28.0
expect 0 go1.28
expect 0 go1.28rc1
expect 0 go1.30.2

# One patch short of the fix on each line. A numeric bug (string compare,
# off-by-one) shows up here: "1.26.9" > "1.26.10" as strings.
expect 1 go1.26.9
expect 1 go1.26.1
expect 1 go1.26.0
expect 1 go1.26
expect 1 go1.27.2
expect 1 go1.27.0
expect 1 go1.27
# Pre-releases of a fixed line predate its fixed patch.
expect 1 go1.27rc1
expect 1 go1.27rc2
expect 1 go1.27beta1
expect 1 go1.26rc3
# Older lines never got the backport, however high the patch.
expect 1 go1.25.12
expect 1 go1.25.99
expect 1 go1.24.20
expect 1 go1.9.10
# Leading zeros must not turn 1.26.09 into octal or a pass.
expect 1 go1.26.09
expect 0 go1.26.010

# Unparseable input fails closed rather than being treated as new enough.
expect 1 ""
expect 1 1.26.10
expect 1 go1.26.10-devel
expect 1 "go1.26.10 X:boringcrypto"
expect 1 go2.0.0
expect 1 go1.
expect 1 go1.x
expect 1 go1.26.
expect 1 go1.26.x
expect 1 go1.27rc
expect 1 go1.27rcX
expect 1 devel
expect 1 "devel go1.28-abcdef"

# Usage errors are distinguishable from "too old".
expect 2
expect 2 go1.26.10 go1.27.3
expect 2 --binary

# --binary on a file without Go build info must fail, not pass.
tmp=$(mktemp)
printf 'not a go binary\n' >"$tmp"
if command -v go >/dev/null 2>&1; then
	expect 1 --binary "$tmp"
fi
rm -f "$tmp"

if [ "$failures" -ne 0 ]; then
	echo "check-go-toolchain_test.sh: $failures failure(s)" >&2
	exit 1
fi
echo "check-go-toolchain_test.sh: all cases passed"
