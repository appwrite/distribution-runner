#!/usr/bin/env bats

bats_require_minimum_version 1.5.0

setup_file() {
  load helpers

  export PKCS12_KEYSTORE=$BATS_FILE_TMPDIR/upload.p12
  PKCS12_PASSWORD=$(random_secret)
  export PKCS12_PASSWORD
  generate_keystore "$PKCS12_KEYSTORE" PKCS12 "$PKCS12_PASSWORD" "$PKCS12_PASSWORD"
  PKCS12_FINGERPRINT=$(keystore_fingerprint "$PKCS12_KEYSTORE" "$PKCS12_PASSWORD")
  PKCS12_EXPIRY=$(keystore_expiry "$PKCS12_KEYSTORE" "$PKCS12_PASSWORD")
  export PKCS12_FINGERPRINT PKCS12_EXPIRY

  export JKS_KEYSTORE=$BATS_FILE_TMPDIR/upload.jks
  JKS_STORE_PASSWORD=$(random_secret)
  JKS_KEY_PASSWORD=$(random_secret)
  export JKS_STORE_PASSWORD JKS_KEY_PASSWORD
  generate_keystore "$JKS_KEYSTORE" JKS "$JKS_STORE_PASSWORD" "$JKS_KEY_PASSWORD"
  JKS_FINGERPRINT=$(keystore_fingerprint "$JKS_KEYSTORE" "$JKS_STORE_PASSWORD")
  JKS_EXPIRY=$(keystore_expiry "$JKS_KEYSTORE" "$JKS_STORE_PASSWORD")
  export JKS_FINGERPRINT JKS_EXPIRY

  export EC_KEYSTORE=$BATS_FILE_TMPDIR/upload-ec.p12
  EC_PASSWORD=$(random_secret)
  export EC_PASSWORD
  generate_keystore "$EC_KEYSTORE" PKCS12 "$EC_PASSWORD" "$EC_PASSWORD" EC 256
  EC_FINGERPRINT=$(keystore_fingerprint "$EC_KEYSTORE" "$EC_PASSWORD")
  EC_EXPIRY=$(keystore_expiry "$EC_KEYSTORE" "$EC_PASSWORD")
  export EC_FINGERPRINT EC_EXPIRY

  [ "${#PKCS12_FINGERPRINT}" = 64 ] && [ "${#JKS_FINGERPRINT}" = 64 ] && [ "${#EC_FINGERPRINT}" = 64 ]
  [ "$PKCS12_FINGERPRINT" != "$JKS_FINGERPRINT" ]

  export SIGNED_AAB=$BATS_FILE_TMPDIR/signed.aab
  printf '%s' "$PKCS12_PASSWORD" >"$BATS_FILE_TMPDIR/password"
  jarsigner -keystore "$PKCS12_KEYSTORE" -storepass:file "$BATS_FILE_TMPDIR/password" \
    -sigalg SHA256withRSA -digestalg SHA-256 -signedjar "$SIGNED_AAB" "$FIXTURE" upload >/dev/null 2>&1
  export TWICE_SIGNED_AAB=$BATS_FILE_TMPDIR/twice-signed.aab
  printf '%s' "$JKS_STORE_PASSWORD" >"$BATS_FILE_TMPDIR/password"
  printf '%s' "$JKS_KEY_PASSWORD" >"$BATS_FILE_TMPDIR/key-password"
  jarsigner -keystore "$JKS_KEYSTORE" -storepass:file "$BATS_FILE_TMPDIR/password" -keypass:file "$BATS_FILE_TMPDIR/key-password" \
    -sigalg SHA256withRSA -digestalg SHA-256 -sigfile SECOND -signedjar "$TWICE_SIGNED_AAB" "$SIGNED_AAB" upload >/dev/null 2>&1

  export SHA384_AAB=$BATS_FILE_TMPDIR/sha384.aab
  printf '%s' "$PKCS12_PASSWORD" >"$BATS_FILE_TMPDIR/password"
  jarsigner -keystore "$PKCS12_KEYSTORE" -storepass:file "$BATS_FILE_TMPDIR/password" \
    -sigalg SHA384withRSA -digestalg SHA-384 -signedjar "$SHA384_AAB" "$FIXTURE" upload >/dev/null 2>&1
  rm "$BATS_FILE_TMPDIR/password" "$BATS_FILE_TMPDIR/key-password"
}

setup() {
  load helpers
}

assert_signed_bundle() {
  local result=$1 fingerprint=$2 expiry=$3 algorithm=${4:-SHA256withRSA} bundle
  bundle="$(dirname "$result")/app.aab"

  run jarsigner -verify -verbose -certs "$bundle"
  [[ $output == *$'\njar verified.'* ]] || fail 'jarsigner did not report jar verified.' "$output"
  [[ $output == *'Digest algorithm: SHA-256'* ]] || fail 'digest algorithm is not SHA-256' "$output"
  [[ $output == *"Signature algorithm: $algorithm"* ]] || fail "signature algorithm is not $algorithm" "$output"
  [[ $output != *'unsigned entries'* ]] || fail 'bundle has unsigned entries' "$output"

  assert_equal 'result keys' "$(json_keys "$result")" \
    'artifact digest expiresAt fingerprint identifier minSdk targetSdk type versionCode versionName'
  assert_json "$result" '"app.aab"' artifact
  assert_json "$result" '"aab"' type
  assert_json "$result" "\"$(sha256 "$bundle")\"" digest
  assert_json "$result" "\"$fingerprint\"" fingerprint
  assert_json "$result" "\"$FIXTURE_IDENTIFIER\"" identifier
  assert_json "$result" '"1.0.0"' versionName
  assert_json "$result" 1 versionCode
  assert_json "$result" 24 minSdk
  assert_json "$result" 36 targetSdk
  assert_json "$result" "\"$expiry\"" expiresAt
}

assert_verification_fails() {
  verify_shim "$1"
  write_bundle "$PKCS12_KEYSTORE" "$PKCS12_PASSWORD" upload "$PKCS12_PASSWORD"

  sign PATH="$SHIMS:$PATH"

  assert_status 6
  assert_stderr_contains "$2"
  assert_bundle_consumed
  assert_not_signed
}

@test 'sign: a PKCS#12 keystore signs the fixture bundle and reports it exactly' {
  write_bundle "$PKCS12_KEYSTORE" "$PKCS12_PASSWORD" upload "$PKCS12_PASSWORD"
  local published=$BATS_TEST_TMPDIR/published

  sign DISTRIBUTION_OUTPUT_DIR="$published"

  assert_status 0
  assert_bundle_consumed
  local result=$WORKSPACE/output/result.json
  assert_equal 'stdout' "$output" "$(cat "$result")"
  assert_signed_bundle "$result" "$PKCS12_FINGERPRINT" "$PKCS12_EXPIRY"
  cmp "$WORKSPACE/output/app.aab" "$published/app.aab"
  cmp "$result" "$published/result.json"
  assert_missing "$WORKSPACE/output/.app.aab.partial" "$WORKSPACE/output/.result.json.partial"
  assert_equal 'output directory mode' "$(stat -c %a "$WORKSPACE/output")" 755
}

@test 'sign: a JKS keystore with a key password other than the store password signs' {
  write_bundle "$JKS_KEYSTORE" "$JKS_STORE_PASSWORD" upload "$JKS_KEY_PASSWORD"

  sign

  assert_status 0
  assert_bundle_consumed
  assert_signed_bundle "$WORKSPACE/output/result.json" "$JKS_FINGERPRINT" "$JKS_EXPIRY"
}

@test 'sign: an EC keystore signs with SHA256withECDSA' {
  write_bundle "$EC_KEYSTORE" "$EC_PASSWORD" upload "$EC_PASSWORD"

  sign

  assert_status 0
  assert_bundle_consumed
  assert_signed_bundle "$WORKSPACE/output/result.json" "$EC_FINGERPRINT" "$EC_EXPIRY" SHA256withECDSA
}

@test 'sign: a package other than the expected one is refused with 3' {
  write_bundle "$PKCS12_KEYSTORE" "$PKCS12_PASSWORD" upload "$PKCS12_PASSWORD"

  sign DISTRIBUTION_IDENTIFIER=io.appwrite.distribution.other

  assert_status 3
  assert_stderr_contains "package is '$FIXTURE_IDENTIFIER', expected 'io.appwrite.distribution.other'"
  assert_bundle_consumed
  assert_not_signed
}

@test 'sign: a version code other than the expected one is refused with 3' {
  write_bundle "$PKCS12_KEYSTORE" "$PKCS12_PASSWORD" upload "$PKCS12_PASSWORD"

  sign DISTRIBUTION_VERSION_CODE=2

  assert_status 3
  assert_stderr_contains "versionCode is '1', expected '2'"
  assert_bundle_consumed
  assert_not_signed
}

@test 'sign: an already signed bundle is refused with 4' {
  write_bundle "$PKCS12_KEYSTORE" "$PKCS12_PASSWORD" upload "$PKCS12_PASSWORD"

  sign DISTRIBUTION_INPUT="$SIGNED_AAB"

  assert_status 4
  assert_stderr_contains 'already carries a signature'
  assert_bundle_consumed
  assert_not_signed
}

@test 'sign: a lowercase signature entry is refused with 4' {
  write_bundle "$PKCS12_KEYSTORE" "$PKCS12_PASSWORD" upload "$PKCS12_PASSWORD"
  tamper lowercase-signature "$FIXTURE" "$BATS_TEST_TMPDIR/lowercase.aab"

  sign DISTRIBUTION_INPUT="$BATS_TEST_TMPDIR/lowercase.aab"

  assert_status 4
  assert_bundle_consumed
  assert_not_signed
}

@test 'sign: an input that is not a zip archive is refused with 3' {
  write_bundle "$PKCS12_KEYSTORE" "$PKCS12_PASSWORD" upload "$PKCS12_PASSWORD"
  tamper not-zip "$FIXTURE" "$BATS_TEST_TMPDIR/not-zip.aab"

  sign DISTRIBUTION_INPUT="$BATS_TEST_TMPDIR/not-zip.aab"

  assert_status 3
  assert_stderr_contains 'not a zip archive'
  assert_bundle_consumed
  assert_not_signed
}

@test 'sign: duplicate entry names are refused with 3' {
  write_bundle "$PKCS12_KEYSTORE" "$PKCS12_PASSWORD" upload "$PKCS12_PASSWORD"
  tamper duplicate "$FIXTURE" "$BATS_TEST_TMPDIR/duplicate.aab"

  sign DISTRIBUTION_INPUT="$BATS_TEST_TMPDIR/duplicate.aab"

  assert_status 3
  assert_stderr_contains 'duplicate entry names'
  assert_bundle_consumed
  assert_not_signed
}

@test 'sign: more entries than DISTRIBUTION_MAX_ENTRIES is refused with 3' {
  write_bundle "$PKCS12_KEYSTORE" "$PKCS12_PASSWORD" upload "$PKCS12_PASSWORD"

  sign DISTRIBUTION_MAX_ENTRIES=1

  assert_status 3
  assert_stderr_contains 'too many entries, limit 1'
  assert_bundle_consumed
  assert_not_signed
}

@test 'sign: an end record that understates the entry count is refused with 3' {
  write_bundle "$PKCS12_KEYSTORE" "$PKCS12_PASSWORD" upload "$PKCS12_PASSWORD"
  tamper lying-count "$FIXTURE" "$BATS_TEST_TMPDIR/lying.aab"

  sign DISTRIBUTION_INPUT="$BATS_TEST_TMPDIR/lying.aab" DISTRIBUTION_MAX_ENTRIES=10

  assert_status 3
  assert_stderr_contains 'too many entries, limit 10'
  assert_bundle_consumed
  assert_not_signed
}

@test 'sign: a version code of 0 or with a leading zero is refused with 7' {
  local code
  for code in 0 01; do
    write_bundle "$PKCS12_KEYSTORE" "$PKCS12_PASSWORD" upload "$PKCS12_PASSWORD"

    sign DISTRIBUTION_VERSION_CODE="$code"

    assert_status 7
    assert_stderr_contains "without leading zeros, got '$code'"
    assert_bundle_consumed
    assert_not_signed
  done
}

@test 'sign: a keystore that is not valid base64 is refused with 7' {
  write_raw_bundle "$(python3 -I -c 'import json, sys; print(json.dumps({"keystore": "not base64!", "storePassword": sys.argv[1], "keyAlias": "upload", "keyPassword": sys.argv[1]}))' "$PKCS12_PASSWORD")" \
    "$PKCS12_PASSWORD"

  sign

  assert_status 7
  assert_stderr_contains 'keystore is not valid base64'
  assert_bundle_consumed
  assert_not_signed
}

@test 'sign: a key alias starting with a hyphen is refused with 7' {
  write_bundle "$PKCS12_KEYSTORE" "$PKCS12_PASSWORD" -upload "$PKCS12_PASSWORD"

  sign

  assert_status 7
  assert_stderr_contains 'keyAlias must not start with a hyphen'
  assert_bundle_consumed
  assert_not_signed
}

@test 'sign: a missing signing bundle is refused with 7' {
  without_bundle

  sign

  assert_status 7
  assert_stderr_contains 'is missing'
  assert_bundle_consumed
  assert_not_signed
}

@test 'sign: a signing bundle that is not JSON is refused with 7' {
  write_raw_bundle "{\"storePassword\": \"$PKCS12_PASSWORD\"" "$PKCS12_PASSWORD"

  sign

  assert_status 7
  assert_stderr_contains 'is not valid JSON'
  assert_bundle_consumed
  assert_not_signed
}

@test 'sign: a signing bundle with an extra key is refused with 7' {
  local keystore
  keystore=$(base64 -w0 "$PKCS12_KEYSTORE")
  write_raw_bundle "$(python3 -I -c 'import json, sys; print(json.dumps({"keystore": sys.argv[1], "storePassword": sys.argv[2], "keyAlias": "upload", "keyPassword": sys.argv[2], "extra": "value"}))' "$keystore" "$PKCS12_PASSWORD")" \
    "$PKCS12_PASSWORD" "$keystore"

  sign

  assert_status 7
  assert_stderr_contains 'must have exactly the keys'
  assert_bundle_consumed
  assert_not_signed
}

@test 'sign: a wrong store password fails signing with 5' {
  local wrong
  wrong=$(random_secret)
  write_bundle "$PKCS12_KEYSTORE" "$wrong" upload "$wrong" "$PKCS12_PASSWORD"

  sign

  assert_status 5
  assert_stderr_contains 'cannot read the keystore'
  assert_bundle_consumed
  assert_not_signed
}

@test 'sign: a verification that does not report jar verified. fails with 6' {
  assert_verification_fails unverified 'jarsigner did not report jar verified.'
}

@test 'sign: a verification that reports two signers fails with 6' {
  assert_verification_fails two-signers 'expected exactly one signer, found 2'
}

@test 'sign: a verification that reports another digest algorithm fails with 6' {
  assert_verification_fails digest "digest algorithm is 'SHA-1', expected SHA-256"
}

@test 'sign: a verification that reports another signature algorithm fails with 6' {
  assert_verification_fails algorithm "signature algorithm is 'SHA1withRSA', expected SHA256withRSA"
}

@test 'sign: a verification that reports unsigned entries fails with 6' {
  assert_verification_fails unsigned 'the bundle contains unsigned entries'
}

@test 'sign: a bundle signer other than the keystore certificate fails with 6' {
  shim keytool "case \" \$* \" in
  *' -printcert '*) echo 'SHA256: 00:11:22:33' ;;
  *) exec \"\$real\" \"\$@\" ;;
esac"
  write_bundle "$PKCS12_KEYSTORE" "$PKCS12_PASSWORD" upload "$PKCS12_PASSWORD"

  sign PATH="$SHIMS:$PATH"

  assert_status 6
  assert_stderr_contains 'the bundle signer does not match the keystore certificate'
  assert_bundle_consumed
  assert_not_signed
}

@test 'sign: an unreadable bundle signer fails with 6' {
  shim keytool "case \" \$* \" in
  *' -printcert '*) exit 1 ;;
  *) exec \"\$real\" \"\$@\" ;;
esac"
  write_bundle "$PKCS12_KEYSTORE" "$PKCS12_PASSWORD" upload "$PKCS12_PASSWORD"

  sign PATH="$SHIMS:$PATH"

  assert_status 6
  assert_stderr_contains 'cannot read the signer certificate of the signed bundle'
  assert_bundle_consumed
  assert_not_signed
}

@test 'sign: a Java tool that runs past DISTRIBUTION_MAX_JAVA_SECONDS fails with 8' {
  shim bundletool 'exec sleep 60'
  write_bundle "$PKCS12_KEYSTORE" "$PKCS12_PASSWORD" upload "$PKCS12_PASSWORD"

  sign PATH="$SHIMS:$PATH" DISTRIBUTION_MAX_JAVA_SECONDS=1

  assert_status 8
  assert_stderr_contains 'bundletool ran longer than 1 seconds'
  assert_bundle_consumed
  assert_not_signed
}

@test 'sign: SIGTERM stops the Java child and removes the tmpfs secrets directory' {
  local marker=$BATS_TEST_TMPDIR/keytool.started
  shim keytool "touch '$marker'
exec sleep 300"
  write_bundle "$PKCS12_KEYSTORE" "$PKCS12_PASSWORD" upload "$PKCS12_PASSWORD"
  assert_no_secrets_directory

  env TMPDIR="$SCRATCH" DISTRIBUTION_WORKSPACE="$WORKSPACE" DISTRIBUTION_INPUT="$FIXTURE" \
    DISTRIBUTION_IDENTIFIER="$FIXTURE_IDENTIFIER" DISTRIBUTION_VERSION_CODE=1 PATH="$SHIMS:$PATH" \
    "$TOOLS/sign-android.sh" >"$BATS_TEST_TMPDIR/stdout" 2>"$BATS_TEST_TMPDIR/stderr" &
  local pid=$! directory
  for _ in $(seq 100); do
    [ ! -e "$marker" ] || break
    sleep 0.1
  done
  [ -e "$marker" ] || fail 'keytool never started'
  directory=$(secrets_directories)
  [ -n "$directory" ] && [ -f "$directory/keystore" ] || fail "no secrets directory with a keystore: '$directory'"
  assert_equal 'secrets filesystem' "$(stat -f -c %T "$directory")" tmpfs

  local started=$SECONDS
  kill -TERM "$pid"
  status=0
  wait "$pid" || status=$?

  assert_equal 'exit status' "$status" 143
  [ $((SECONDS - started)) -lt 5 ] || fail "sign-android.sh took $((SECONDS - started))s to stop"
  assert_missing "$directory"
  assert_no_secrets_directory
  for _ in $(seq 50); do
    running 'sleep 300' || break
    sleep 0.1
  done
  ! running 'sleep 300' || fail 'the Java child is still running'
  assert_missing "$WORKSPACE/signing/bundle.json" "$WORKSPACE/output/app.aab"
}

@test 'sign: a missing DISTRIBUTION_VERSION_CODE is refused with 7 after the bundle is deleted' {
  write_bundle "$PKCS12_KEYSTORE" "$PKCS12_PASSWORD" upload "$PKCS12_PASSWORD"

  sign -u DISTRIBUTION_VERSION_CODE

  assert_status 7
  assert_stderr_contains 'DISTRIBUTION_VERSION_CODE must be a positive integer'
  assert_bundle_consumed
  assert_not_signed
}

@test 'inspect: the unsigned fixture bundle is reported unsigned' {
  inspect aab "$FIXTURE"

  assert_inspected "$FIXTURE"
  local result=$WORKSPACE/output/result.json
  assert_json "$result" '"aab"' type
  assert_json "$result" false signed
  assert_json "$result" null fingerprint
  assert_json "$result" "\"$FIXTURE_IDENTIFIER\"" identifier
  assert_json "$result" '"1.0.0"' versionName
  assert_json "$result" 1 versionCode
  assert_json "$result" 24 minSdk
  assert_json "$result" 36 targetSdk
  assert_json "$result" null entitlements
  assert_json "$result" null profileExpiresAt
  assert_json "$result" null teamId
}

@test 'inspect: a signed bundle is reported signed with its signer fingerprint' {
  inspect aab "$SIGNED_AAB"

  assert_inspected "$SIGNED_AAB"
  local result=$WORKSPACE/output/result.json
  assert_json "$result" true signed
  assert_json "$result" "\"$PKCS12_FINGERPRINT\"" fingerprint
  assert_json "$result" "\"$FIXTURE_IDENTIFIER\"" identifier
}

@test 'inspect: a signed bundle with a changed entry is rejected with 3' {
  local tampered=$BATS_TEST_TMPDIR/tampered.aab
  python3 -I - "$SIGNED_AAB" "$tampered" <<'PYTHON'
import sys
import zipfile

source, target = sys.argv[1:]
with zipfile.ZipFile(source) as original, zipfile.ZipFile(target, 'w') as copy:
    for entry in original.infolist():
        content = original.read(entry)
        if entry.filename == 'base/resources.pb':
            content = content[:-1] + bytes([content[-1] ^ 1])
        copy.writestr(entry, content)
PYTHON
  python3 -I -c 'import sys, zipfile; names = zipfile.ZipFile(sys.argv[1]).namelist(); sys.exit(0 if "META-INF/UPLOAD.SF" in names else 1)' "$tampered"

  inspect aab "$tampered"

  assert_status 3
  assert_stderr_contains 'signature does not verify'
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: a bundle with two signers is rejected with 3' {
  inspect aab "$TWICE_SIGNED_AAB"

  assert_status 3
  assert_stderr_contains 'expected exactly one signer, found 2'
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: a bundle signed with SHA-384 is reported signed' {
  inspect aab "$SHA384_AAB"

  assert_inspected "$SHA384_AAB"
  assert_json "$WORKSPACE/output/result.json" true signed
  assert_json "$WORKSPACE/output/result.json" "\"$PKCS12_FINGERPRINT\"" fingerprint
}

@test 'inspect: a bundle verified with a SHA-1 digest is rejected with 3' {
  verify_shim digest

  inspect aab "$SIGNED_AAB" PATH="$SHIMS:$PATH"

  assert_status 3
  assert_stderr_contains "digest algorithm is 'SHA-1'"
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: a Java tool that runs past DISTRIBUTION_MAX_JAVA_SECONDS fails with 8' {
  shim bundletool 'exec sleep 60'

  inspect aab "$FIXTURE" PATH="$SHIMS:$PATH" DISTRIBUTION_MAX_JAVA_SECONDS=1

  assert_status 8
  assert_stderr_contains 'bundletool ran longer than 1 seconds'
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: more entries than DISTRIBUTION_MAX_ENTRIES is rejected with 2' {
  inspect aab "$FIXTURE" DISTRIBUTION_MAX_ENTRIES=1

  assert_status 2
  assert_stderr_contains 'too many entries, limit 1'
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: an end record that understates the entry count is rejected with 2' {
  tamper lying-count "$FIXTURE" "$BATS_TEST_TMPDIR/lying.aab"

  inspect aab "$BATS_TEST_TMPDIR/lying.aab" DISTRIBUTION_MAX_ENTRIES=10

  assert_status 2
  assert_stderr_contains 'too many entries, limit 10'
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: an end record that disagrees with the directory is rejected with 2' {
  tamper lying-count "$FIXTURE" "$BATS_TEST_TMPDIR/lying.aab"

  inspect aab "$BATS_TEST_TMPDIR/lying.aab"

  assert_status 2
  assert_stderr_contains 'end of central directory record declares 1 entries'
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: a central directory larger than DISTRIBUTION_MAX_DIRECTORY_BYTES is rejected with 2' {
  inspect aab "$FIXTURE" DISTRIBUTION_MAX_DIRECTORY_BYTES=100

  assert_status 2
  assert_stderr_contains 'limit 100'
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: a zip64 locator pointing outside the file is rejected with 2' {
  tamper zip64-locator "$FIXTURE" "$BATS_TEST_TMPDIR/zip64.aab"

  inspect aab "$BATS_TEST_TMPDIR/zip64.aab"

  assert_status 2
  assert_stderr_contains 'zip64 end of central directory record lies outside the file'
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: an input that is not a zip archive is rejected with 2' {
  tamper not-zip "$FIXTURE" "$BATS_TEST_TMPDIR/not-zip.aab"

  inspect aab "$BATS_TEST_TMPDIR/not-zip.aab"

  assert_status 2
  assert_stderr_contains 'not a zip archive'
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: an invalid ceiling is rejected with 4' {
  inspect aab "$FIXTURE" DISTRIBUTION_MAX_ENTRIES=many

  assert_status 4
  assert_stderr_contains "DISTRIBUTION_MAX_ENTRIES must be a positive integer, got 'many'"
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: a minted signed IPA reports its team, profile, entitlements and signer' {
  local ipa=$BATS_TEST_TMPDIR/app.ipa leaf=$BATS_TEST_TMPDIR/leaf.pem
  mint "$ipa" --team "$TEAM" --expires 2031-02-03T04:05:06Z --leaf-out "$leaf"

  inspect ipa "$ipa"

  assert_inspected "$ipa"
  local result=$WORKSPACE/output/result.json
  assert_json "$result" '"ipa"' type
  assert_json "$result" true signed
  assert_json "$result" "\"$(certificate_fingerprint "$leaf")\"" fingerprint
  assert_json "$result" "\"$TEAM\"" teamId
  assert_json "$result" '"2031-02-03T04:05:06Z"' profileExpiresAt
  assert_json "$result" "\"$TEAM.$IPA_IDENTIFIER\"" entitlements application-identifier
  assert_json "$result" "\"$IPA_IDENTIFIER\"" identifier
  assert_json "$result" '"2.3"' versionName
  assert_json "$result" '"7"' versionCode
  assert_json "$result" null minSdk
  assert_json "$result" null targetSdk
}

@test 'inspect: a universal IPA is inspected from its first slice' {
  local ipa=$BATS_TEST_TMPDIR/app.ipa leaf=$BATS_TEST_TMPDIR/leaf.pem
  mint "$ipa" --team "$TEAM" --universal --leaf-out "$leaf"

  inspect ipa "$ipa"

  assert_inspected "$ipa"
  assert_json "$WORKSPACE/output/result.json" true signed
  assert_json "$WORKSPACE/output/result.json" "\"$(certificate_fingerprint "$leaf")\"" fingerprint
}

@test 'inspect: a profile whose wildcard application-identifier covers the bundle is accepted' {
  local ipa=$BATS_TEST_TMPDIR/app.ipa
  mint "$ipa" --team "$TEAM" --profile-identifier 'io.appwrite.*'

  inspect ipa "$ipa"

  assert_inspected "$ipa"
  assert_json "$WORKSPACE/output/result.json" true signed
}

@test 'inspect: a profile signed by a code signing certificate is rejected with 3' {
  local ipa=$BATS_TEST_TMPDIR/app.ipa
  mint "$ipa" --team "$TEAM" --forge-profile

  inspect ipa "$ipa"

  assert_status 3
  assert_stderr_contains "provisioning profile is not signed by Apple's profile signing certificate"
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: an IPA whose Info.plist was swapped after signing is rejected with 3' {
  local ipa=$BATS_TEST_TMPDIR/app.ipa
  mint "$BATS_TEST_TMPDIR/original.ipa" --team "$TEAM"
  tamper info-identifier "$BATS_TEST_TMPDIR/original.ipa" "$ipa"

  inspect ipa "$ipa"

  assert_status 3
  assert_stderr_contains 'does not match CFBundleIdentifier io.appwrite.distribution.swapped'
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: an IPA whose Info.plist differs from the sealed one is rejected with 3' {
  local ipa=$BATS_TEST_TMPDIR/app.ipa
  mint "$BATS_TEST_TMPDIR/original.ipa" --team "$TEAM" --code-identifier io.appwrite.distribution.swapped
  tamper info-identifier "$BATS_TEST_TMPDIR/original.ipa" "$ipa"

  inspect ipa "$ipa"

  assert_status 3
  assert_stderr_contains 'the code signature does not seal this Info.plist'
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: an IPA whose entitlements were changed after signing is rejected with 3' {
  local ipa=$BATS_TEST_TMPDIR/app.ipa
  mint "$BATS_TEST_TMPDIR/original.ipa" --team "$TEAM"
  tamper entitlements "$BATS_TEST_TMPDIR/original.ipa" "$ipa"

  inspect ipa "$ipa"

  assert_status 3
  assert_stderr_contains 'the code signature does not seal these entitlements'
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: an IPA whose DER entitlements were changed after signing is rejected with 3' {
  local ipa=$BATS_TEST_TMPDIR/app.ipa
  mint "$BATS_TEST_TMPDIR/original.ipa" --team "$TEAM"
  tamper der-entitlements "$BATS_TEST_TMPDIR/original.ipa" "$ipa"

  inspect ipa "$ipa"

  assert_status 3
  assert_stderr_contains 'the code signature does not seal these DER entitlements'
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: a CodeDirectory identifier other than CFBundleIdentifier is rejected with 3' {
  local ipa=$BATS_TEST_TMPDIR/app.ipa
  mint "$ipa" --team "$TEAM" --code-identifier io.appwrite.distribution.other

  inspect ipa "$ipa"

  assert_status 3
  assert_stderr_contains "CodeDirectory identifier io.appwrite.distribution.other does not match CFBundleIdentifier $IPA_IDENTIFIER"
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: a profile application-identifier for another bundle is rejected with 3' {
  local ipa=$BATS_TEST_TMPDIR/app.ipa
  mint "$ipa" --team "$TEAM" --profile-identifier io.appwrite.distribution.other

  inspect ipa "$ipa"

  assert_status 3
  assert_stderr_contains "does not cover $IPA_IDENTIFIER"
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: malformed IPAs are rejected with 2' {
  local mode
  mint "$BATS_TEST_TMPDIR/original.ipa" --team "$TEAM"
  for mode in truncate-executable drop-executable shrink-info encrypt-info corrupt-info; do
    tamper "$mode" "$BATS_TEST_TMPDIR/original.ipa" "$BATS_TEST_TMPDIR/$mode.ipa"

    inspect ipa "$BATS_TEST_TMPDIR/$mode.ipa"

    [ "$status" = 2 ] || fail "$mode: expected exit 2, got $status" "stderr: ${stderr:-}"
    assert_missing "$WORKSPACE/output/result.json"
  done
}

@test 'inspect: an IPA whose profile team differs from the signature team is rejected with 3' {
  local ipa=$BATS_TEST_TMPDIR/app.ipa
  mint "$ipa" --team "$TEAM" --profile-team ZYXWV98765

  inspect ipa "$ipa"

  assert_status 3
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: an ad hoc IPA is reported unsigned' {
  local ipa=$BATS_TEST_TMPDIR/app.ipa
  mint "$ipa" --adhoc

  inspect ipa "$ipa"

  assert_inspected "$ipa"
  local result=$WORKSPACE/output/result.json
  assert_json "$result" false signed
  assert_json "$result" null fingerprint
  assert_json "$result" "\"$IPA_IDENTIFIER\"" identifier
}

@test 'inspect: an IPA without a provisioning profile is reported unsigned' {
  local ipa=$BATS_TEST_TMPDIR/app.ipa
  mint "$ipa" --team "$TEAM" --no-profile

  inspect ipa "$ipa"

  assert_inspected "$ipa"
  local result=$WORKSPACE/output/result.json
  assert_json "$result" false signed
  assert_json "$result" null profileExpiresAt
  assert_json "$result" "\"$IPA_IDENTIFIER\"" identifier
}

@test 'inspect: a profile chained to the test CA is rejected with 3 when only Apple is trusted' {
  local ipa=$BATS_TEST_TMPDIR/app.ipa
  mint "$ipa" --team "$TEAM"

  inspect ipa "$ipa" DISTRIBUTION_TRUST="$TOOLS/certificates/apple.pem"

  assert_status 3
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: an unknown artifact type is rejected with 4' {
  inspect apk "$FIXTURE"

  assert_status 4
  assert_stderr_contains "DISTRIBUTION_ARTIFACT_TYPE must be aab or ipa, got 'apk'"
  assert_missing "$WORKSPACE/output/result.json"
}
