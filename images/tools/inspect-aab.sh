#!/bin/sh
set -eu

file="$1"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fail() {
  echo "inspect-aab.sh: $2" >&2
  exit "$1"
}

manifest() {
  bundletool dump manifest --bundle "$file" --xpath "$1" 2>>"$work/bundletool.log" \
    || { cat "$work/bundletool.log" >&2; fail 2 'cannot read the bundle manifest'; }
}

identifier=$(manifest '/manifest/@package')
version_code=$(manifest '/manifest/@android:versionCode')
version_name=$(manifest '/manifest/@android:versionName')
min_sdk=$(manifest '/manifest/uses-sdk/@android:minSdkVersion')
target_sdk=$(manifest '/manifest/uses-sdk/@android:targetSdkVersion')

signatures=$(python3 -I - "$file" <<'PYTHON'
import sys
import zipfile

SIGNATURE_EXTENSIONS = ('.SF', '.RSA', '.DSA', '.EC')

with zipfile.ZipFile(sys.argv[1]) as archive:
    names = [name.upper() for name in archive.namelist()]
print(sum(
    1 for name in names
    if name.startswith('META-INF/') and '/' not in name[9:]
    and (name.endswith(SIGNATURE_EXTENSIONS) or name[9:].startswith('SIG-'))
))
PYTHON
)

signed=false
fingerprint=''
if [ "$signatures" != 0 ]; then
  jarsigner -verify "$file" >"$work/verify.log" 2>&1 || true
  if ! grep -qFx 'jar verified.' "$work/verify.log" || grep -qF 'unsigned entries' "$work/verify.log"; then
    tail -n 20 "$work/verify.log" >&2
    fail 3 'signature present but does not verify'
  fi
  signed=true
  fingerprint=$(keytool -printcert -jarfile "$file" | grep -F 'SHA256:' | head -n 1 | cut -d: -f2- | tr -d ' :' | tr 'A-F' 'a-f')
  [ -n "$fingerprint" ] || fail 3 'signature present but no signer certificate'
fi

python3 -I - "$identifier" "$version_name" "$version_code" "$min_sdk" "$target_sdk" "$signed" "$fingerprint" <<'PYTHON'
import json
import sys

identifier, version_name, version_code, min_sdk, target_sdk, signed, fingerprint = sys.argv[1:]


def integer(name, value, required):
    if not value and not required:
        return None
    if not (value.isascii() and value.isdigit()):
        print(f'inspect-aab.sh: {name} is {value!r}, not an integer', file=sys.stderr)
        sys.exit(2)
    return int(value)


print(json.dumps({
    'type': 'aab',
    'identifier': identifier or None,
    'versionName': version_name or None,
    'versionCode': integer('versionCode', version_code, True),
    'minSdk': integer('minSdk', min_sdk, False),
    'targetSdk': integer('targetSdk', target_sdk, False),
    'signed': signed == 'true',
    'fingerprint': fingerprint or None,
}))
PYTHON
