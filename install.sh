#!/bin/sh
# Code Flock installer: curl -fsSL https://github.com/Autarquico/codeflock/releases/latest/download/install.sh | sh
#
# Env:  CFLK_VERSION=vX.Y.Z|latest  release to install (default latest, the newest non-prerelease)
#       CFLK_INSTALL_DIR            where cflk goes (default ~/.local/bin)
#       CFLK_RELEASE_BASE_URL       testing/mirror override of the download base; never skips verification
#       CF_VERSION, CF_INSTALL_DIR, CF_RELEASE_BASE_URL are still read, with a warning; CFLK_* wins when both are set
# Exit: 2 invalid arguments, 3 unsupported platform, 4 prerequisite failure, 5 download/checksum/signature failure
#
# It verifies the signed checksum list, then the tarball, and only then installs cflk and validates it.
# An existing cf (the Code Flock beta's, or any other tool's) is never read, run or touched: cflk is the only command installed.
# The trust root is the allowed_signers block below. While it holds the PLACEHOLDER no signature verifies,
# so the installer fails closed. Review-first users verify this file with install.sh.sig.
set -eu
# a CDPATH would make `cd` print the directory it picks and can pick another one
unset CDPATH

DEFAULT_BASE_URL='https://github.com/Autarquico/codeflock/releases/download'
LATEST_URL='https://github.com/Autarquico/codeflock/releases/latest'
NAMESPACE='codefabric-release'
PRINCIPAL='release@codefabric'

log() { printf 'install.sh: %s\n' "$*" >&2; }
die() { code=$1; shift; log "error: $*"; exit "$code"; }

write_trust_roots() {
	cat >"$WORK/allowed_signers" <<'TRUST'
# BEGIN allowed_signers
release@codefabric namespaces="codefabric-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOZwPPPOq0JVZPk3SPcMUDHnCNq5WLxZwBXDdpGq8s1p
# END allowed_signers
TRUST
	cat >"$WORK/revoked_keys" <<'TRUST'
# BEGIN revoked_keys
# END revoked_keys
TRUST
	cat >"$WORK/selftest_signers" <<'TRUST'
selftest@codefabric namespaces="codefabric-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICK4UoHMqGP3DPuaMooT7NdUdvyWM6Q+8IbsdGos1k20
TRUST
	cat >"$WORK/selftest.sig" <<'TRUST'
-----BEGIN SSH SIGNATURE-----
U1NIU0lHAAAAAQAAADMAAAALc3NoLWVkMjU1MTkAAAAgIrhSgcyoY/cM+5oyihPs11R2/J
YzpD7whux0aizWTbQAAAASY29kZWZhYnJpYy1yZWxlYXNlAAAAAAAAAAZzaGE1MTIAAABT
AAAAC3NzaC1lZDI1NTE5AAAAQDQNRDopCkBtnMEGvpI533Tck0mZ39LawrOk/yWhw/rlpG
otEVuooXgRC/DgIuRfHNZuWvrRniZWSWmgnVXfPAM=
-----END SSH SIGNATURE-----
TRUST
	printf 'codefabric-installer-selftest\n' >"$WORK/selftest.msg"
}

have() { command -v "$1" >/dev/null 2>&1; }

detect_platform() {
	case $(uname -s) in
		Linux) OS=linux ;;
		Darwin) OS=darwin ;;
		*) die 3 "unsupported OS: $(uname -s)" ;;
	esac
	case $(uname -m) in
		x86_64 | amd64) ARCH=amd64 ;;
		aarch64 | arm64) ARCH=arm64 ;;
		*) die 3 "unsupported architecture: $(uname -m)" ;;
	esac
	# A shell running under Rosetta on Apple silicon reports x86_64; the native binary is the right one.
	if [ "$OS" = darwin ] && [ "$ARCH" = amd64 ] && have sysctl && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null)" = 1 ]; then
		ARCH=arm64
	fi
}

require_commands() {
	for c in curl tar awk sed grep mktemp ssh-keygen cp chmod mv mkdir rm cat wc tr basename dirname; do
		have "$c" || die 4 "required command not found: $c"
	done
	have sha256sum || have shasum || die 4 'required command not found: sha256sum or shasum'
}

verify_signature() { # file signature
	ssh-keygen -Y verify -f "$WORK/allowed_signers" -r "$WORK/revoked_keys" \
		-I "$PRINCIPAL" -n "$NAMESPACE" -s "$2" <"$1" >/dev/null 2>&1
}

verify_the_verifier() {
	ssh-keygen -Y verify -f "$WORK/selftest_signers" -I selftest@codefabric -n "$NAMESPACE" \
		-s "$WORK/selftest.sig" <"$WORK/selftest.msg" >/dev/null 2>&1 ||
		die 4 'ssh-keygen cannot verify OpenSSH signatures (OpenSSH 8.2 or newer is required)'
}

# pick NEW_NAME NEW_VALUE OLD_NAME OLD_VALUE: the CFLK_* value, else the CF_* one (warning); sets PICKED and PICKED_NAME.
pick() {
	PICKED=$2
	PICKED_NAME=$1
	if [ -n "$4" ]; then
		if [ -n "$2" ]; then
			log "warning: $3 is ignored because $1 is set"
		else
			PICKED=$4
			PICKED_NAME=$3
			log "warning: $3 is deprecated and goes away in 0.5; use $1"
		fi
	fi
}

read_env() {
	pick CFLK_VERSION "${CFLK_VERSION:-}" CF_VERSION "${CF_VERSION:-}"
	REQUESTED=${PICKED:-latest}
	REQUESTED_NAME=$PICKED_NAME
	pick CFLK_INSTALL_DIR "${CFLK_INSTALL_DIR:-}" CF_INSTALL_DIR "${CF_INSTALL_DIR:-}"
	INSTALL_DIR=${PICKED:-$HOME/.local/bin}
	pick CFLK_RELEASE_BASE_URL "${CFLK_RELEASE_BASE_URL:-}" CF_RELEASE_BASE_URL "${CF_RELEASE_BASE_URL:-}"
	RELEASE_BASE_OVERRIDE=$PICKED
}

fetch() { # url destination
	if [ -n "$RELEASE_BASE_OVERRIDE" ]; then
		curl -fsSL --retry 2 -o "$2" "$1" || die 5 "download failed: $1"
	else
		curl -fsSL --proto '=https' --tlsv1.2 --retry 2 -o "$2" "$1" || die 5 "download failed: $1"
	fi
}

is_version() {
	[ "$(printf '%s\n' "$1" | wc -l | tr -d ' ')" -eq 1 ] || return 1
	printf '%s\n' "$1" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'
}

sha256_of() {
	if have sha256sum; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi
}

# The newest non-prerelease, from where the latest-release URL redirects to (.../tag/vX.Y.Z).
latest_version() {
	if [ -n "$RELEASE_BASE_OVERRIDE" ]; then
		url="${RELEASE_BASE_OVERRIDE%/}/latest"
		final=$(curl -fsSL --retry 2 -o /dev/null -w '%{url_effective}' "$url") || die 5 "download failed: $url"
	else
		url=$LATEST_URL
		final=$(curl -fsSL --proto '=https' --tlsv1.2 --retry 2 -o /dev/null -w '%{url_effective}' "$url") || die 5 "download failed: $url"
	fi
	VERSION=${final##*/}
	is_version "$VERSION" || die 5 "could not resolve the latest release from $final"
}

resolve_version() {
	if [ "$REQUESTED" = latest ]; then
		latest_version
	else
		is_version "$REQUESTED" || die 2 "invalid $REQUESTED_NAME: $REQUESTED (vX.Y.Z or latest)"
		VERSION=$REQUESTED
	fi
	log "installing $VERSION"
}

# The signed list: line 1 "version: vX.Y.Z", then "<sha256>  cflk_<ver>_<os>_<arch>.tar.gz" lines only.
# A release from before the rename lists cf_<ver>_... instead: its tarballs hold no cflk, and the beta installer
# that shipped with it is the one that installs it.
check_checksums() {
	header=$(sed -n 1p "$WORK/checksums.txt")
	[ "$header" = "version: $VERSION" ] || die 5 "checksums.txt is signed for '${header#version: }', not $VERSION"
	if sed 1d "$WORK/checksums.txt" | grep -Eq '^[0-9a-f]{64}  cf_'; then
		old_dir=
		[ "$INSTALL_DIR" = "$HOME/.local/bin" ] || old_dir=" CF_INSTALL_DIR='$INSTALL_DIR'"
		die 5 "$VERSION predates the cflk rename (its assets are cf_*, not cflk_*); install it with the installer published with that release: curl -fsSL $BASE_URL/$VERSION/install.sh | CF_VERSION=$VERSION$old_dir sh"
	fi
	if sed 1d "$WORK/checksums.txt" | grep -Evq '^[0-9a-f]{64}  cflk_[0-9A-Za-z.+-]+_(linux|darwin)_(amd64|arm64)\.tar\.gz$'; then
		die 5 'checksums.txt contains a malformed line'
	fi
}

# Only the regular file `cflk` leaves the tarball, and it lands in WORK, never in the install dir.
extract_binaries() {
	tar -tzf "$1" >"$WORK/members" || die 5 "$(basename "$1") is not readable"
	if grep -Eq '(^/|(^|/)\.\.(/|$))' "$WORK/members"; then die 5 "$(basename "$1") has an unsafe member path"; fi
	mkdir "$WORK/unpacked"
	[ "$(grep -cx cflk "$WORK/members")" -eq 1 ] || die 5 "$(basename "$1") does not contain exactly one cflk"
	tar -xzf "$1" -C "$WORK/unpacked" cflk || die 5 "could not extract cflk from $(basename "$1")"
	if [ ! -f "$WORK/unpacked/cflk" ] || [ -L "$WORK/unpacked/cflk" ]; then die 5 "cflk in $(basename "$1") is not a regular file"; fi
}

download_tarball() {
	name="cflk_${VERSION#v}_${OS}_${ARCH}.tar.gz"
	fetch "$BASE_URL/$VERSION/checksums.txt" "$WORK/checksums.txt"
	fetch "$BASE_URL/$VERSION/checksums.txt.sig" "$WORK/checksums.txt.sig"
	verify_signature "$WORK/checksums.txt" "$WORK/checksums.txt.sig" || die 5 'signature verification failed: checksums.txt'
	check_checksums
	expected=$(awk -v n="$name" 'NR > 1 && $2 == n { print $1; c++ } END { exit c != 1 }' "$WORK/checksums.txt") ||
		die 5 "$name is not listed exactly once in the signed checksums"
	fetch "$BASE_URL/$VERSION/$name" "$WORK/$name"
	[ "$(sha256_of "$WORK/$name")" = "$expected" ] || die 5 "checksum mismatch for $name"
	extract_binaries "$WORK/$name"
}

# place SRC DEST: copy next to DEST, then rename over it, so a reader never sees half a file.
# Fails (leaving no TMP_BIN behind that the exit trap does not remove) when DEST's directory is not writable.
place() {
	dest_dir=$(dirname "$2")
	TMP_BIN=$(mktemp "$dest_dir/.$(basename "$2").XXXXXX") || return 1
	{ cp "$1" "$TMP_BIN" && chmod 755 "$TMP_BIN"; } || return 1
}

install_cflk() {
	place "$WORK/unpacked/cflk" "$DIR/cflk" || die 4 "cannot write to $DIR"
	"$TMP_BIN" version >"$WORK/cflk.version" 2>&1 </dev/null || die 4 'the new cflk does not run; nothing was installed'
	# the version is a whole word of the output: 0.4.0 is not 0.4.0-beta.1, nor 10.4.0
	awk -v v="${VERSION#v}" '{ for (i = 1; i <= NF; i++) if ($i == v) found = 1 } END { exit !found }' "$WORK/cflk.version" ||
		die 4 "the new cflk does not report $VERSION; nothing was installed"
	mv -f "$TMP_BIN" "$DIR/cflk" || die 4 "cannot install into $DIR"
	TMP_BIN=
	log "installed $DIR/cflk ($VERSION)"
}

install_binaries() {
	DIR=${INSTALL_DIR%/}
	mkdir -p "$DIR" || die 4 "cannot create $DIR"
	[ ! -d "$DIR/cflk" ] || die 4 "$DIR/cflk is a directory (or a link to one); move it away and run the installer again"
	install_cflk
	case ":$PATH:" in
		*":$DIR:"*) ;;
		*) log "$DIR is not on your PATH; add this line to your shell profile (~/.profile, ~/.zshrc): export PATH=\"$DIR:\$PATH\"" ;;
	esac
	log "next: 'cflk controller init' on the machine that will be the Controller, or 'cflk setup' to join an existing one"
}

main() {
	[ $# -eq 0 ] || die 2 "unexpected arguments: $* (configure with CFLK_VERSION and CFLK_INSTALL_DIR)"
	read_env
	BASE_URL=${RELEASE_BASE_OVERRIDE:-$DEFAULT_BASE_URL}
	BASE_URL=${BASE_URL%/}
	TMP_BIN=
	detect_platform
	require_commands
	WORK=$(mktemp -d) || die 4 'cannot create a temporary directory'
	trap 'rm -rf "$WORK"; [ -z "$TMP_BIN" ] || rm -f "$TMP_BIN"' EXIT
	write_trust_roots
	verify_the_verifier
	resolve_version
	download_tarball
	install_binaries
}

main "$@"
