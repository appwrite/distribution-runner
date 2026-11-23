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

  [ "${#PKCS12_FINGERPRINT}" = 64 ] && [ "${#JKS_FINGERPRINT}" = 64 ]
  [ "$PKCS12_FINGERPRINT" != "$JKS_FINGERPRINT" ]

  export SIGNED_AAB=$BATS_FILE_TMPDIR/signed.aab
  printf '%s' "$PKCS12_PASSWORD" >"$BATS_FILE_TMPDIR/password"
  jarsigner -keystore "$PKCS12_KEYSTORE" -storepass:file "$BATS_FILE_TMPDIR/password" \
    -sigalg SHA256withRSA -digestalg SHA-256 -signedjar "$SIGNED_AAB" "$FIXTURE" upload >/dev/null 2>&1
  rm "$BATS_FILE_TMPDIR/password"
}

setup() {
  load helpers
}

assert_signed_bundle() {
  local result=$1 fingerprint=$2 expiry=$3 bundle
  bundle="$(dirname "$result")/app.aab"

  run jarsigner -verify -verbose -certs "$bundle"
  [[ $output == *$'\njar verified.'* ]] || fail 'jarsigner did not report jar verified.' "$output"
  [[ $output == *'Digest algorithm: SHA-256'* ]] || fail 'digest algorithm is not SHA-256' "$output"
  [[ $output == *'Signature algorithm: SHA256withRSA'* ]] || fail 'signature algorithm is not SHA256withRSA' "$output"
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
}

@test 'sign: a JKS keystore with a key password other than the store password signs' {
  write_bundle "$JKS_KEYSTORE" "$JKS_STORE_PASSWORD" upload "$JKS_KEY_PASSWORD"

  sign

  assert_status 0
  assert_bundle_consumed
  assert_signed_bundle "$WORKSPACE/output/result.json" "$JKS_FINGERPRINT" "$JKS_EXPIRY"
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
  assert_stderr_contains 'signature present but does not verify'
  assert_missing "$WORKSPACE/output/result.json"
}

@test 'inspect: more entries than MAX_ENTRIES is rejected with 2' {
  inspect aab "$FIXTURE" MAX_ENTRIES=1

  assert_status 2
  assert_stderr_contains 'too many entries, limit 1'
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
