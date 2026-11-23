#!/bin/sh
set -eu
umask 077

workspace="${DISTRIBUTION_WORKSPACE:-/mnt/code}"
bundle="$workspace/signing/bundle.json"
input="${DISTRIBUTION_INPUT:-$workspace/input/app-unsigned.aab}"
output="$workspace/output"

secrets=$(mktemp -d)
staged=''
cleanup() {
  rm -rf "$secrets"
  [ -z "$staged" ] || rm -f "$staged"
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT

fail() {
  echo "sign-android.sh: $2" >&2
  exit "$1"
}

python3 -I - "$bundle" "$secrets" <<'PYTHON' || exit 7
import base64
import binascii
import json
import os
import sys

KEYS = ('keystore', 'storePassword', 'keyAlias', 'keyPassword')


def refuse(reason):
    print(f'sign-android.sh: signing bundle {reason}', file=sys.stderr)
    sys.exit(7)


def consume(path):
    try:
        with open(path, 'rb') as handle:
            return handle.read()
    except OSError:
        return None
    finally:
        try:
            os.remove(path)
        except OSError:
            pass


def parse(raw):
    try:
        document = json.loads(raw)
    except ValueError:
        refuse('is not valid JSON')
    if not isinstance(document, dict) or sorted(document) != sorted(KEYS):
        refuse('must have exactly the keys ' + ', '.join(KEYS))
    for key in KEYS:
        if not isinstance(document[key], str) or not document[key]:
            refuse(f'{key} must be a non-empty string')
    for key in ('storePassword', 'keyPassword', 'keyAlias'):
        if any(character in document[key] for character in '\r\n\0'):
            refuse(f'{key} must be a single line')
    if document['keyAlias'].startswith('-'):
        refuse('keyAlias must not start with a hyphen')
    try:
        keystore = base64.b64decode(document['keystore'], validate=True)
    except binascii.Error:
        refuse('keystore is not valid base64')
    if not keystore:
        refuse('keystore is empty')
    return {
        'keystore': keystore,
        'store-password': document['storePassword'].encode(),
        'key-password': document['keyPassword'].encode(),
        'alias': document['keyAlias'].encode(),
    }


def main(path, directory):
    raw = consume(path)
    if raw is None:
        refuse(f'{path} is missing')
    for name, content in parse(raw).items():
        descriptor = os.open(os.path.join(directory, name), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, 'wb') as handle:
            handle.write(content)


try:
    main(*sys.argv[1:])
except SystemExit:
    raise
except Exception:
    refuse('could not be read')
PYTHON

identifier="${DISTRIBUTION_IDENTIFIER:-}"
version_code="${DISTRIBUTION_VERSION_CODE:-}"
[ -n "$identifier" ] || fail 7 'DISTRIBUTION_IDENTIFIER is required'
case "$version_code" in
  '' | *[!0-9]*) fail 7 'DISTRIBUTION_VERSION_CODE must be a positive integer' ;;
esac
[ -f "$input" ] || fail 7 "input bundle $input not found"

python3 -I - "$input" <<'PYTHON' || exit $?
import os
import sys
import zipfile

SIGNATURE_EXTENSIONS = ('.SF', '.RSA', '.DSA', '.EC')

path = sys.argv[1]
max_entries = int(os.environ.get('MAX_ENTRIES') or 100_000)
max_uncompressed_bytes = int(os.environ.get('MAX_UNCOMPRESSED_BYTES') or 16_000_000_000)


def refuse(code, reason):
    print(f'sign-android.sh: refusing to sign: {reason}', file=sys.stderr)
    sys.exit(code)


try:
    with zipfile.ZipFile(path) as archive:
        entries = archive.infolist()
except (OSError, zipfile.BadZipFile):
    refuse(3, 'input is not a readable zip archive')
if len(entries) > max_entries:
    refuse(3, f'too many entries: {len(entries)}')
if len({entry.filename for entry in entries}) != len(entries):
    refuse(3, 'duplicate entry names')
if sum(entry.file_size for entry in entries) > max_uncompressed_bytes:
    refuse(3, 'uncompressed size too large')
for entry in entries:
    name = entry.filename.upper()
    if not name.startswith('META-INF/') or '/' in name[9:]:
        continue
    if name.endswith(SIGNATURE_EXTENSIONS) or name[9:].startswith('SIG-'):
        refuse(4, 'bundle already carries a signature')
PYTHON

manifest() {
  bundletool dump manifest --bundle "$input" --xpath "$1" 2>>"$secrets/bundletool.log" \
    || { cat "$secrets/bundletool.log" >&2; fail 3 'refusing to sign: cannot read the bundle manifest'; }
}

level() {
  case "$2" in
    *[!0-9]*) fail 3 "refusing to sign: $1 is '$2', not an API level" ;;
  esac
}

found_identifier=$(manifest '/manifest/@package')
found_version_code=$(manifest '/manifest/@android:versionCode')
version_name=$(manifest '/manifest/@android:versionName')
min_sdk=$(manifest '/manifest/uses-sdk/@android:minSdkVersion')
target_sdk=$(manifest '/manifest/uses-sdk/@android:targetSdkVersion')
level minSdk "$min_sdk"
level targetSdk "$target_sdk"

if [ "$found_identifier" != "$identifier" ]; then
  fail 3 "refusing to sign: package is '$found_identifier', expected '$identifier'"
fi
if [ "$found_version_code" != "$version_code" ]; then
  fail 3 "refusing to sign: versionCode is '$found_version_code', expected '$version_code'"
fi

alias=$(cat "$secrets/alias")

listing=$(keytool -list -v -keystore "$secrets/keystore" -storepass:file "$secrets/store-password" -alias "$alias" 2>&1) \
  || { printf '%s\n' "$listing" >&2; fail 5 'cannot read the keystore with the given store password and alias'; }
algorithm=$(printf '%s\n' "$listing" | grep -F 'Subject Public Key Algorithm' | head -n 1) || true
case "$algorithm" in
  *RSA*) signature=SHA256withRSA ;;
  *EC*) signature=SHA256withECDSA ;;
  *) fail 5 "unsupported upload key: ${algorithm:-no key entry for the alias}" ;;
esac

mkdir -p "$output"
staged="$output/.app.aab.partial"
jarsigner -keystore "$secrets/keystore" \
  -storepass:file "$secrets/store-password" -keypass:file "$secrets/key-password" \
  -sigalg "$signature" -digestalg SHA-256 \
  -signedjar "$staged" "$input" "$alias" >"$secrets/sign.log" 2>&1 \
  || { cat "$secrets/sign.log" >&2; fail 5 'jarsigner failed to sign the bundle'; }
grep -qFx 'jar signed.' "$secrets/sign.log" || { cat "$secrets/sign.log" >&2; fail 5 'jarsigner did not sign the bundle'; }

jarsigner -verify -verbose -certs "$staged" >"$secrets/verify.log" 2>&1 || true
unverified() {
  tail -n 20 "$secrets/verify.log" >&2
  fail 6 "signed bundle does not verify: $1"
}
summary() {
  signers=0
  digest_algorithm=''
  signature_algorithm=''
  while IFS= read -r line; do
    case "$line" in
      '- Signed by '*) signers=$((signers + 1)) ;;
      '    Digest algorithm: '*) [ "$signers" = 0 ] || digest_algorithm="${line#    Digest algorithm: }" ;;
      '    Signature algorithm: '*) [ "$signers" = 0 ] || signature_algorithm="${line#    Signature algorithm: }" ;;
    esac
  done <"$secrets/verify.log"
}
summary
grep -qFx 'jar verified.' "$secrets/verify.log" || unverified 'jarsigner did not report jar verified.'
[ "$signers" = 1 ] || unverified "expected exactly one signer, found $signers"
[ "$digest_algorithm" = SHA-256 ] || unverified "digest algorithm is '$digest_algorithm', expected SHA-256"
case "$signature_algorithm" in
  "$signature, "*) ;;
  *) unverified "signature algorithm is '$signature_algorithm', expected $signature" ;;
esac
if grep -qF 'unsigned entries' "$secrets/verify.log"; then
  unverified 'bundle contains unsigned entries'
fi

fingerprint=$(keytool -printcert -jarfile "$staged" | grep -F 'SHA256:' | head -n 1 | cut -d: -f2- | tr -d ' :' | tr 'A-F' 'a-f')
keytool -exportcert -rfc -keystore "$secrets/keystore" -storepass:file "$secrets/store-password" \
  -alias "$alias" -file "$secrets/certificate.pem" >/dev/null 2>&1 \
  || fail 6 'cannot export the signing certificate from the keystore'
certificate_fingerprint=$(openssl x509 -in "$secrets/certificate.pem" -noout -fingerprint -sha256) \
  || fail 6 'cannot read the signing certificate'
certificate_fingerprint=$(echo "${certificate_fingerprint#*=}" | tr -d ':' | tr 'A-F' 'a-f')
not_after=$(openssl x509 -in "$secrets/certificate.pem" -noout -enddate -dateopt iso_8601) \
  || fail 6 'cannot read the signing certificate expiry'
not_after="${not_after#notAfter=}"
[ -n "$fingerprint" ] && [ "$fingerprint" = "$certificate_fingerprint" ] \
  || fail 6 'the bundle signer does not match the keystore certificate'

mv "$staged" "$output/app.aab"
staged=''
digest=$(sha256sum "$output/app.aab" | cut -d' ' -f1)

python3 -I - "$output/result.json" "$digest" "$fingerprint" "$identifier" "$version_name" \
  "$version_code" "$min_sdk" "$target_sdk" "$not_after" <<'PYTHON'
import datetime
import json
import os
import sys

path, digest, fingerprint, identifier, version_name, version_code, min_sdk, target_sdk, not_after = sys.argv[1:]
expires = datetime.datetime.strptime(not_after, '%Y-%m-%d %H:%M:%SZ')
result = {
    'artifact': 'app.aab',
    'type': 'aab',
    'digest': digest,
    'fingerprint': fingerprint,
    'identifier': identifier,
    'versionName': version_name,
    'versionCode': int(version_code),
    'minSdk': int(min_sdk) if min_sdk else None,
    'targetSdk': int(target_sdk) if target_sdk else None,
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
