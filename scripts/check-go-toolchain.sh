#!/bin/sh
# check-go-toolchain.sh — fail unless a Go version carries the crypto/x509
# distrust list for the eight compromised eMudhra intermediates (golang/go#82345).
#
# Usage:
#   check-go-toolchain.sh go1.26.10            # check a version string
#   check-go-toolchain.sh --binary ./erpc-server [...]
#                                              # check the toolchain each binary was built with
#
# Why this exists: the distrust list is compiled into the binary from the
# toolchain's standard library. Neither the CA bundle in the runtime image nor
# the `go` line in go.mod can supply it. The Debian 12 bundle in distroless
# still ships `emSign ECC Root CA - G3`, and also ships the Sectigo
# `AAA Certificate Services` root that cross-signs the same intermediates.
# Removing roots therefore does not close the hole. Only a fixed toolchain does,
# so the Dockerfile runs this before and after `go build`, and a base-image
# downgrade fails the build instead of shipping a forgeable TLS client.
#
# Contract:
#   - The fix shipped as point releases on two lines: FIXED_RELEASES below.
#     A version passes if it is at or above the fixed patch on its own minor
#     line, or if its minor line is newer than every listed one (later minors
#     branch from a master that already has the fix).
#   - A minor line older than every listed one fails: it never got the backport.
#   - A pre-release (go1.27rc1, go1.27beta2) on a listed line fails, because
#     it predates that line's fixed patch. A pre-release on a newer line passes.
#   - Anything that does not parse as goMAJOR.MINOR[.PATCH|rcN|betaN] fails.
#     The script fails closed: an unknown format is never assumed to be safe.
#   - Exit 0 = acceptable, 1 = too old or unparseable, 2 = usage error.
#     Each rejection prints the reason to stderr.

set -eu

# One "MINOR.PATCH" entry per release line that received the fix (Go 1.x).
# Keep ascending. Add a line here only if Go backports the fix to it.
FIXED_RELEASES="26.10 27.3"

# strip_zeros DIGITS — print DIGITS without leading zeros ("0" stays "0").
strip_zeros() {
	n=$1
	while [ "${n#0}" != "$n" ] && [ -n "${n#0}" ]; do
		n=${n#0}
	done
	printf '%s\n' "$n"
}

# check_version VERSION
# Prints nothing and returns 0 when VERSION is acceptable. Otherwise prints the
# reason to stderr and returns 1.
check_version() {
	v=$1
	# `go version` may append experiments ("go1.26.10 X:boringcrypto");
	# callers pass only the first word, so a space here is a parse failure.
	rest=${v#go1.}
	if [ "$rest" = "$v" ]; then
		echo "check-go-toolchain: '$v' is not a go1.x version string" >&2
		return 1
	fi

	minor=${rest%%[!0-9]*}
	tail=${rest#"$minor"}
	if [ -z "$minor" ]; then
		echo "check-go-toolchain: cannot parse minor version from '$v'" >&2
		return 1
	fi

	# Classify the remainder after the minor number.
	prerelease=0
	case $tail in
		.*)
			patch=${tail#.}
			case $patch in
				'' | *[!0-9]*)
					echo "check-go-toolchain: cannot parse patch version from '$v'" >&2
					return 1
					;;
			esac
			;;
		rc[0-9]* | beta[0-9]*)
			num=${tail#rc}
			num=${num#beta}
			case $num in
				*[!0-9]*)
					echo "check-go-toolchain: cannot parse pre-release from '$v'" >&2
					return 1
					;;
			esac
			prerelease=1
			patch=0
			;;
		'')
			# Before Go 1.21, the first release of a line had no ".0".
			patch=0
			;;
		*)
			echo "check-go-toolchain: unrecognised suffix '$tail' in '$v'" >&2
			return 1
			;;
	esac

	# Strip leading zeros: some shells read "09" as invalid octal in `-lt`.
	minor=$(strip_zeros "$minor")
	patch=$(strip_zeros "$patch")

	highest_minor=0
	for entry in $FIXED_RELEASES; do
		fixed_minor=${entry%%.*}
		fixed_patch=${entry#*.}
		highest_minor=$fixed_minor
		if [ "$minor" -eq "$fixed_minor" ]; then
			if [ "$prerelease" -eq 1 ]; then
				echo "check-go-toolchain: $v is a pre-release of go1.$minor; the fix is in go1.$minor.$fixed_patch" >&2
				return 1
			fi
			if [ "$patch" -lt "$fixed_patch" ]; then
				echo "check-go-toolchain: $v lacks the eMudhra x509 distrust list; need go1.$minor.$fixed_patch or later" >&2
				return 1
			fi
			return 0
		fi
	done

	if [ "$minor" -gt "$highest_minor" ]; then
		return 0
	fi
	echo "check-go-toolchain: go1.$minor never received the eMudhra x509 distrust list; need go1.26.10, go1.27.3 or a newer minor" >&2
	return 1
}

if [ $# -eq 0 ]; then
	echo "usage: $0 goVERSION | --binary FILE..." >&2
	exit 2
fi

if [ "$1" = "--binary" ]; then
	shift
	if [ $# -eq 0 ]; then
		echo "usage: $0 --binary FILE..." >&2
		exit 2
	fi
	status=0
	for bin in "$@"; do
		# `go version FILE` prints "FILE: go1.26.10" from the build info that
		# the linker embeds. It reports the toolchain that actually compiled
		# the standard library into this binary, which is what matters.
		out=$(go version "$bin") || {
			echo "check-go-toolchain: cannot read build info from $bin" >&2
			status=1
			continue
		}
		ver=${out##*: }
		ver=${ver%% *}
		if check_version "$ver"; then
			echo "check-go-toolchain: $bin built with $ver: OK"
		else
			status=1
		fi
	done
	exit $status
fi

if [ $# -ne 1 ]; then
	echo "usage: $0 goVERSION | --binary FILE..." >&2
	exit 2
fi
if check_version "$1"; then
	echo "check-go-toolchain: $1: OK"
	exit 0
fi
exit 1
