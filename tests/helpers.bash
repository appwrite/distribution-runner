# shellcheck disable=SC2034,SC2154 # constants are read by tools.bats; bats run sets status, output and stderr
TOOLS=/usr/local/distribution
FIXTURE=/fixture/app-unsigned.aab
FIXTURE_IDENTIFIER=io.appwrite.distribution.fixture
IPA_IDENTIFIER=io.appwrite.distribution.test
TEAM=ABCDE12345

fail() {
  printf '%s\n' "$@" >&2
  return 1
}

json() {
  python3 -I - "$@" <<'PYTHON'
import json
import sys

path, *keys = sys.argv[1:]
with open(path, encoding='utf-8') as handle:
    value = json.load(handle)
for key in keys:
    value = value[key]
print(json.dumps(value, sort_keys=True))
PYTHON
}

json_keys() {
  python3 -I -c 'import json, sys; print(" ".join(sorted(json.load(open(sys.argv[1], encoding="utf-8")))))' "$1"
}

assert_equal() {
  [ "$2" = "$3" ] || fail "$1: expected '$3', got '$2'"
}

assert_json() {
  local path=$1 expected=$2
  shift 2
  assert_equal "$path ${*}" "$(json "$path" "$@")" "$expected"
}

assert_status() {
  [ "$status" = "$1" ] || fail "exit status: expected $1, got $status" "stdout: $output" "stderr: ${stderr:-}"
}

assert_stderr_contains() {
  [[ ${stderr:-} == *"$1"* ]] || fail "stderr does not contain '$1'" "stderr: ${stderr:-}"
}

assert_missing() {
  local path
  for path in "$@"; do
    [ ! -e "$path" ] || fail "$path exists"
  done
}

sha256() {
  sha256sum "$1" | cut -d' ' -f1
}

random_secret() {
  python3 -I -c 'import secrets; print(secrets.token_hex(16))'
}

generate_keystore() {
  local path=$1 type=$2 store_password=$3 key_password=$4
  STORE_PASSWORD_INPUT=$store_password KEY_PASSWORD_INPUT=$key_password \
    keytool -genkeypair -storetype "$type" -keyalg RSA -keysize 2048 -alias upload -validity 10000 \
    -dname 'CN=Distribution Test Upload Key' -keystore "$path" \
    -storepass:env STORE_PASSWORD_INPUT -keypass:env KEY_PASSWORD_INPUT >/dev/null 2>&1 </dev/null \
    || fail "keytool could not generate $path"
}

keystore_fingerprint() {
  STORE_PASSWORD_INPUT=$2 keytool -list -v -keystore "$1" -storepass:env STORE_PASSWORD_INPUT -alias upload 2>/dev/null \
    | grep -F 'SHA256:' | head -n 1 | cut -d: -f2- | tr -d ' :' | tr 'A-F' 'a-f'
}

keystore_expiry() {
  local certificate=$BATS_FILE_TMPDIR/certificate.pem
  STORE_PASSWORD_INPUT=$2 keytool -exportcert -rfc -keystore "$1" -storepass:env STORE_PASSWORD_INPUT \
    -alias upload -file "$certificate" >/dev/null 2>&1
  openssl x509 -in "$certificate" -noout -enddate | python3 -I -c '
import datetime
import sys

value = sys.stdin.read().strip().removeprefix("notAfter=")
print(datetime.datetime.strptime(value, "%b %d %H:%M:%S %Y %Z").strftime("%Y-%m-%dT%H:%M:%SZ"))
'
}

certificate_fingerprint() {
  openssl x509 -in "$1" -outform der | sha256sum | cut -d' ' -f1
}

workspace() {
  WORKSPACE=$BATS_TEST_TMPDIR/workspace
  SCRATCH=$BATS_TEST_TMPDIR/scratch
  mkdir -p "$WORKSPACE/signing" "$SCRATCH"
}

write_bundle() {
  local keystore=$1 store_password=$2 alias=$3 key_password=$4
  shift 4
  workspace
  BUNDLE_KEYSTORE=$(base64 -w0 "$keystore")
  BUNDLE_SECRETS=("$store_password" "$key_password" "$BUNDLE_KEYSTORE" "${BUNDLE_KEYSTORE:0:48}" "$@")
  python3 -I - "$WORKSPACE/signing/bundle.json" "$BUNDLE_KEYSTORE" "$store_password" "$alias" "$key_password" <<'PYTHON'
import json
import sys

path, keystore, store_password, alias, key_password = sys.argv[1:]
with open(path, 'w', encoding='utf-8') as handle:
    json.dump({'keystore': keystore, 'storePassword': store_password, 'keyAlias': alias, 'keyPassword': key_password}, handle)
PYTHON
}

write_raw_bundle() {
  workspace
  printf '%s' "$1" >"$WORKSPACE/signing/bundle.json"
  shift
  BUNDLE_SECRETS=("$@")
}

without_bundle() {
  workspace
  BUNDLE_SECRETS=()
}

sign() {
  local defaults=(
    TMPDIR="$SCRATCH"
    DISTRIBUTION_WORKSPACE="$WORKSPACE"
    DISTRIBUTION_INPUT="$FIXTURE"
    DISTRIBUTION_IDENTIFIER="$FIXTURE_IDENTIFIER"
    DISTRIBUTION_VERSION_CODE=1
  )
  local options=() assignments=() kept=() assignment
  while [ "$#" -gt 0 ]; do
    case "$1" in
      -u)
        options+=(-u "$2")
        kept=()
        for assignment in "${defaults[@]}"; do
          [[ $assignment == "$2="* ]] || kept+=("$assignment")
        done
        defaults=("${kept[@]}")
        shift 2
        ;;
      *) assignments+=("$1"); shift ;;
    esac
  done
  run --separate-stderr env "${options[@]}" "${defaults[@]}" "${assignments[@]}" "$TOOLS/sign-android.sh"
}

assert_bundle_consumed() {
  assert_missing "$WORKSPACE/signing/bundle.json"
  [ -z "$(ls -A "$SCRATCH")" ] || fail "temporary files left behind: $(ls -A "$SCRATCH")"
  local secret
  for secret in "${BUNDLE_SECRETS[@]}"; do
    [[ $output != *"$secret"* ]] || fail 'a signing secret appears in stdout'
    [[ ${stderr:-} != *"$secret"* ]] || fail 'a signing secret appears in stderr'
    ! grep -rqF -- "$secret" "$WORKSPACE" || fail 'a signing secret was written to the workspace'
  done
}

assert_not_signed() {
  assert_missing "$WORKSPACE/output/app.aab" "$WORKSPACE/output/result.json"
}

inspect() {
  local type=$1 file=$2
  shift 2
  workspace
  run --separate-stderr env DISTRIBUTION_WORKSPACE="$WORKSPACE" DISTRIBUTION_ARTIFACT_TYPE="$type" "$@" \
    "$TOOLS/inspect.sh" "$file"
}

assert_inspected() {
  local file=$1 result=$WORKSPACE/output/result.json
  assert_status 0
  assert_equal 'stdout' "$output" "$(cat "$result")"
  assert_equal 'result keys' "$(json_keys "$result")" \
    'digest entitlements fingerprint identifier minSdk profileExpiresAt signed size targetSdk teamId type versionCode versionName'
  assert_json "$result" "\"$(sha256 "$file")\"" digest
  assert_json "$result" "$(stat -c %s "$file")" size
}

mint() {
  local out=$1
  shift
  python3 -I "$TOOLS/test/mint-ipa.py" --identifier "$IPA_IDENTIFIER" --version-name 2.3 --version-code 7 \
    "$@" --out "$out" >"$BATS_TEST_TMPDIR/mint.log" 2>&1 || fail "mint-ipa.py $* failed" "$(cat "$BATS_TEST_TMPDIR/mint.log")"
}
