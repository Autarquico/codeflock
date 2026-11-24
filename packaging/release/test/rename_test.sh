# shellcheck shell=sh
# shellcheck disable=SC2154,SC2034
# Scenarios of the cf -> cflk rename in the installer and the release set: an existing cf is never touched,
# the old CF_* variables, old releases, and the release check on the tarballs.

installed_release() {
	good_release "$KEY_A"
	serve_dist
}

# --- Order: cflk is validated before anything lands -----------------------------------

# 1.2.3 is not 1.2.3-beta.1: the version is compared whole.
TestScenario_Order_aCflkOfALongerVersionIsNotTheOneRequested() {
	make_dist "$T/dist"
	python3 -I "$RELEASE_DIR/test/mktar.py" "$T/dist/cflk_${BARE_VERSION}_linux_amd64.tar.gz" ok "$BARE_VERSION-beta.1"
	finalize_dist "$T/dist" "$KEY_A"
	serve_dist
	run_install "$KEY_A/install.sh"
	assert_rc 4
	assert_out_contains "does not report $VERSION"
	[ -z "$(ls -A "$T/bin")" ] || fail "something was installed: $(ls -A "$T/bin")"
}

TestScenario_Order_aBrokenCflkLeavesTheCfAlone() {
	make_dist "$T/dist"
	python3 -I "$RELEASE_DIR/test/mktar.py" "$T/dist/cflk_${BARE_VERSION}_linux_amd64.tar.gz" ok 6.6.6
	finalize_dist "$T/dist" "$KEY_A"
	serve_dist
	seed_old_cf
	run_install "$KEY_A/install.sh"
	assert_rc 4
	assert_out_contains "does not report $VERSION"
	assert_install_dir_untouched
}

# An existing cf is not read, run or replaced: cflk lands next to it.
TestScenario_ExistingCf_isLeftAlone() {
	installed_release
	seed_old_cf
	run_install "$KEY_A/install.sh"
	assert_rc 0
	cmp -s "$T/bin/cf" "$T/old-cf" || fail "the cf was modified"
	[ -e "$T/bin/cflk" ] && [ "$(find "$T/bin" -mindepth 1 | wc -l | tr -d " ")" -eq 2 ] || fail "unexpected files in the install dir"
	[ "$("$T/bin/cflk" version)" = "$BARE_VERSION" ] || fail "cflk was not installed"
}

# --- Releases from before the rename --------------------------------------------------

TestScenario_OldRelease_isRefusedWithTheInstallerToUse() {
	good_release "$KEY_A"
	signed_checksums_body "$(printf '%064d  cf_%s_linux_amd64.tar.gz' 0 "$BARE_VERSION")"
	serve_dist
	seed_old_cf
	run_install "$KEY_A/install.sh"
	assert_rc 5
	assert_out_contains "predates the cflk rename"
	assert_out_contains "curl -fsSL .*/$VERSION/install.sh | CF_VERSION=$VERSION CF_INSTALL_DIR='$T/bin' sh"
	assert_install_dir_untouched
}

# --- The CF_* variables keep working, with a warning --------------------------------------

TestScenario_VariableAntigua_installerReadsThemWithAWarning() {
	installed_release
	rc=0
	env PATH="$SHIM:$PATH" HOME="$T/home" CF_RELEASE_BASE_URL="$URL/$TEST_NAME" CF_INSTALL_DIR="$T/bin" CF_VERSION="$VERSION" \
		FAKE_UNAME_S=Linux FAKE_UNAME_M=x86_64 "$(command -v sh)" "$KEY_A/install.sh" >"$T/out" 2>&1 || rc=$?
	assert_rc 0
	[ "$("$T/bin/cflk" version)" = "$BARE_VERSION" ] || fail "the CF_* variables were not honoured"
	assert_out_contains "CF_VERSION is deprecated"
	assert_out_contains "CF_INSTALL_DIR is deprecated"
	assert_out_contains "CF_RELEASE_BASE_URL is deprecated"
}

TestScenario_VariableAntigua_cflkWinsWhenBothAreSet() {
	installed_release
	rc=0
	env PATH="$SHIM:$PATH" HOME="$T/home" CFLK_RELEASE_BASE_URL="$URL/$TEST_NAME" CFLK_INSTALL_DIR="$T/bin" CFLK_VERSION="$VERSION" \
		CF_INSTALL_DIR="$T/other" CF_VERSION=v9.9.9 FAKE_UNAME_S=Linux FAKE_UNAME_M=x86_64 \
		"$(command -v sh)" "$KEY_A/install.sh" >"$T/out" 2>&1 || rc=$?
	assert_rc 0
	[ -x "$T/bin/cflk" ] || fail "CFLK_INSTALL_DIR was not used"
	[ ! -e "$T/other" ] || fail "CF_INSTALL_DIR was used although CFLK_INSTALL_DIR is set"
	assert_out_contains "CF_INSTALL_DIR is ignored because CFLK_INSTALL_DIR is set"
	assert_out_contains "CF_VERSION is ignored because CFLK_VERSION is set"
}

# --- The release set: every tarball carries cflk -----------------------------------------

TestScenario_ReleaseSet_checkRejectsATarballWithAnUnsafeMemberPath() {
	make_dist "$T/dist"
	python3 -I "$RELEASE_DIR/test/mktar.py" "$T/dist/cflk_${BARE_VERSION}_linux_amd64.tar.gz" dotdot "$BARE_VERSION"
	finalize_dist "$T/dist" "$KEY_A"
	run_cmd sh "$RELEASE_DIR/check.sh" "$T/dist" "$KEY_A/install.sh"
	assert_rc 1
	assert_out_contains "cflk_${BARE_VERSION}_linux_amd64.tar.gz has an unsafe member path"
}


TestScenario_ReleaseSet_fourTarballsWithCflkPassCheck() {
	good_release "$KEY_A"
	run_cmd sh "$RELEASE_DIR/check.sh" "$T/dist" "$KEY_A/install.sh"
	assert_rc 0
	for target in linux_amd64 linux_arm64 darwin_amd64 darwin_arm64; do
		tar -tzf "$T/dist/cflk_${BARE_VERSION}_$target.tar.gz" | grep -qx cflk || fail "$target lacks cflk"
	done
}

TestScenario_ReleaseSet_checkRejectsATarballWithoutCflk() {
	make_dist "$T/dist"
	python3 -I "$RELEASE_DIR/test/mktar.py" "$T/dist/cflk_${BARE_VERSION}_darwin_arm64.tar.gz" notcflk "$BARE_VERSION"
	finalize_dist "$T/dist" "$KEY_A"
	run_cmd sh "$RELEASE_DIR/check.sh" "$T/dist" "$KEY_A/install.sh"
	assert_rc 1
	assert_out_contains "cflk_${BARE_VERSION}_darwin_arm64.tar.gz does not contain cflk"
}

# --- CDPATH does not redirect a relative install directory -----------------------------

TestScenario_Cdpath_aRelativeInstallDirIsNotRedirected() {
	installed_release
	mkdir -p "$T/work/bin" "$T/decoy/bin"
	cd "$T/work" || exit 1
	rc=0
	env PATH="$SHIM:$PATH" HOME="$T/home" CDPATH="$T/decoy" \
		CFLK_RELEASE_BASE_URL="$URL/$TEST_NAME" CFLK_INSTALL_DIR=bin CFLK_VERSION="$VERSION" \
		FAKE_UNAME_S=Linux FAKE_UNAME_M=x86_64 "$(command -v sh)" "$KEY_A/install.sh" >"$T/out" 2>&1 || rc=$?
	assert_rc 0
	[ -x "$T/work/bin/cflk" ] || fail "cflk was not installed in the relative directory"
	[ -z "$(ls -A "$T/decoy/bin")" ] || fail "the CDPATH directory was used"
}

TestScenario_ReleaseSet_checkRejectsASymlinkedCflk() {
	make_dist "$T/dist"
	python3 -I "$RELEASE_DIR/test/mktar.py" "$T/dist/cflk_${BARE_VERSION}_darwin_amd64.tar.gz" symlink "$BARE_VERSION"
	finalize_dist "$T/dist" "$KEY_A"
	run_cmd sh "$RELEASE_DIR/check.sh" "$T/dist" "$KEY_A/install.sh"
	assert_rc 1
	assert_out_contains "cflk in cflk_${BARE_VERSION}_darwin_amd64.tar.gz is not a regular file"
}
