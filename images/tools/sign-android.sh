#!/bin/sh
set -eu
umask 077

EXIT_REFUSED=3
EXIT_ALREADY_SIGNED=4
EXIT_SIGNING=5
EXIT_VERIFICATION=6
EXIT_USAGE=7
EXIT_TIMEOUT=8
EXIT_INTERRUPTED=130
EXIT_TERMINATED=143
TIMED_OUT=124
KILLED=137
KILL_AFTER_SECONDS=10

home=$(cd "$(dirname "$0")" && pwd)
workspace="${DISTRIBUTION_WORKSPACE:-/mnt/code}"
bundle="$workspace/signing/bundle.json"
input="${DISTRIBUTION_INPUT:-$workspace/input/app-unsigned.aab}"
output="$workspace/output"
java_seconds="${DISTRIBUTION_MAX_JAVA_SECONDS:-600}"

exec 3>&2
secrets=$(mktemp -d /dev/shm/signing.XXXXXX 2>/dev/null) || secrets=$(mktemp -d)
work=$(mktemp -d)
staged=''
child=''
cleanup() {
  rm -rf "$secrets" "$work"
  [ -z "$staged" ] || rm -f "$staged"
}
interrupt() {
  [ -z "$child" ] || kill "$child" 2>/dev/null || true
  exit "$1"
}
trap cleanup EXIT
trap 'interrupt $EXIT_TERMINATED' TERM
trap 'interrupt $EXIT_INTERRUPTED' INT

fail() {
  echo "sign-android.sh: $2" >&3
  exit "$1"
}

run() {
  "$@" 3>&- &
  child=$!
  status=0
  wait "$child" || status=$?
  child=''
  return "$status"
}

run_java() {
  status=0
  run timeout --kill-after="$KILL_AFTER_SECONDS" "$java_seconds" "$@" || status=$?
  case "$status" in
    "$TIMED_OUT" | "$KILLED") fail "$EXIT_TIMEOUT" "$1 ran longer than $java_seconds seconds" ;;
  esac
  return "$status"
}

run python3 -I "$home/bundle.py" "$bundle" "$secrets" || exit "$EXIT_USAGE"

identifier="${DISTRIBUTION_IDENTIFIER:-}"
version_code="${DISTRIBUTION_VERSION_CODE:-}"
[ -n "$identifier" ] || fail "$EXIT_USAGE" 'DISTRIBUTION_IDENTIFIER is required'
case "$version_code" in
  '' | 0* | *[!0-9]*) fail "$EXIT_USAGE" "DISTRIBUTION_VERSION_CODE must be a positive integer without leading zeros, got '$version_code'" ;;
esac
case "$java_seconds" in
  '' | 0* | *[!0-9]*) fail "$EXIT_USAGE" "DISTRIBUTION_MAX_JAVA_SECONDS must be a positive integer, got '$java_seconds'" ;;
esac
[ -f "$input" ] || fail "$EXIT_USAGE" "input bundle $input not found"

run python3 -I "$home/archive.py" --malformed "$EXIT_REFUSED" --usage "$EXIT_USAGE" --type aab "$input" >"$work/signatures" \
  || exit $?
[ "$(cat "$work/signatures")" = 0 ] || fail "$EXIT_ALREADY_SIGNED" 'refusing to sign: the bundle already carries a signature'

run_java bundletool dump manifest --bundle "$input" >"$work/manifest.xml" 2>"$work/bundletool.log" \
  || { cat "$work/bundletool.log" >&2; fail "$EXIT_REFUSED" 'refusing to sign: cannot read the bundle manifest'; }
run python3 -I "$home/manifest.py" --exit "$EXIT_REFUSED" --identifier "$identifier" --version-code "$version_code" \
  "$work/manifest.xml" >"$work/manifest.json" || exit $?

alias=$(cat "$secrets/alias")
certificate="$secrets/certificate.pem"
run_java keytool -exportcert -rfc -keystore "$secrets/keystore" -storepass:file "$secrets/store-password" \
  -alias "$alias" -file "$certificate" >"$work/keytool.log" 2>&1 \
  || { cat "$work/keytool.log" >&2; fail "$EXIT_SIGNING" 'cannot read the keystore with the given store password and alias'; }
openssl x509 -in "$certificate" -noout -text >"$work/certificate.txt" 2>&1 \
  || fail "$EXIT_SIGNING" 'cannot read the signing certificate'
case "$(grep -F 'Public Key Algorithm:' "$work/certificate.txt" | head -n 1)" in
  *rsaEncryption*) signature=SHA256withRSA ;;
  *id-ecPublicKey*) signature=SHA256withECDSA ;;
  *) fail "$EXIT_SIGNING" 'unsupported upload key: only RSA and EC keys are supported' ;;
esac
certificate_fingerprint=$(openssl x509 -in "$certificate" -noout -fingerprint -sha256) \
  || fail "$EXIT_SIGNING" 'cannot read the signing certificate fingerprint'
certificate_fingerprint=$(echo "${certificate_fingerprint#*=}" | tr -d ':' | tr 'A-F' 'a-f')
not_after=$(openssl x509 -in "$certificate" -noout -enddate -dateopt iso_8601) \
  || fail "$EXIT_SIGNING" 'cannot read the signing certificate expiry'
not_after="${not_after#notAfter=}"

mkdir -p "$output"
chmod 0755 "$output"
staged="$output/.app.aab.partial"
run_java jarsigner -keystore "$secrets/keystore" \
  -storepass:file "$secrets/store-password" -keypass:file "$secrets/key-password" \
  -sigalg "$signature" -digestalg SHA-256 \
  -signedjar "$staged" "$input" "$alias" >"$work/sign.log" 2>&1 \
  || { cat "$work/sign.log" >&2; fail "$EXIT_SIGNING" 'jarsigner failed to sign the bundle'; }
grep -qFx 'jar signed.' "$work/sign.log" || { cat "$work/sign.log" >&2; fail "$EXIT_SIGNING" 'jarsigner did not sign the bundle'; }

run_java jarsigner -verify -verbose -certs "$staged" >"$work/verify.log" 2>&1 || true
run python3 -I "$home/signature.py" --exit "$EXIT_VERIFICATION" --digest SHA-256 --algorithm "$signature" "$work/verify.log" \
  || exit $?

run_java keytool -printcert -jarfile "$staged" >"$work/signer.txt" 2>&1 \
  || { cat "$work/signer.txt" >&2; fail "$EXIT_VERIFICATION" 'cannot read the signer certificate of the signed bundle'; }
fingerprint=$(grep -F 'SHA256:' "$work/signer.txt" | head -n 1 | cut -d: -f2- | tr -d ' :' | tr 'A-F' 'a-f')
[ -n "$fingerprint" ] && [ "$fingerprint" = "$certificate_fingerprint" ] \
  || fail "$EXIT_VERIFICATION" 'the bundle signer does not match the keystore certificate'

mv "$staged" "$output/app.aab"
staged=''
run sha256sum "$output/app.aab" >"$work/digest"
digest=$(cut -d' ' -f1 "$work/digest")

python3 -I - "$work/manifest.json" "$output/result.json" "$digest" "$fingerprint" "$not_after" <<'PYTHON'
import datetime
import json
import os
import sys

manifest_path, path, digest, fingerprint, not_after = sys.argv[1:]
with open(manifest_path, encoding='utf-8') as handle:
    manifest = json.load(handle)
expires = datetime.datetime.strptime(not_after, '%Y-%m-%d %H:%M:%SZ')
result = {
    'artifact': 'app.aab',
    'type': 'aab',
    'digest': digest,
    'fingerprint': fingerprint,
    'identifier': manifest['identifier'],
    'versionName': manifest['versionName'],
    'versionCode': manifest['versionCode'],
    'minSdk': manifest['minSdk'],
    'targetSdk': manifest['targetSdk'],
    'expiresAt': expires.strftime('%Y-%m-%dT%H:%M:%SZ'),
}
partial = os.path.join(os.path.dirname(path), '.result.json.partial')
with open(partial, 'w', encoding='utf-8') as handle:
    json.dump(result, handle)
    handle.write('\n')
os.replace(partial, path)
PYTHON
chmod 0644 "$output/app.aab" "$output/result.json"

publish() {
  partial="$(dirname "$2")/.$(basename "$2").partial"
  cp "$1" "$partial"
  mv "$partial" "$2"
}

if [ -n "${DISTRIBUTION_OUTPUT_DIR:-}" ]; then
  umask 022
  mkdir -p "$DISTRIBUTION_OUTPUT_DIR"
  if [ "$(cd "$DISTRIBUTION_OUTPUT_DIR" && pwd -P)" != "$(cd "$output" && pwd -P)" ]; then
    publish "$output/app.aab" "$DISTRIBUTION_OUTPUT_DIR/app.aab"
    publish "$output/result.json" "$DISTRIBUTION_OUTPUT_DIR/result.json"
  fi
fi

cat "$output/result.json"
