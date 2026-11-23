#!/bin/sh
set -eu

home=$(cd "$(dirname "$0")" && pwd)
workspace="${DISTRIBUTION_WORKSPACE:-/mnt/code}"
file="${1:-${DISTRIBUTION_INPUT:-$workspace/input/artifact}}"
type="${DISTRIBUTION_ARTIFACT_TYPE:-}"
output="$workspace/output"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
trap 'exit 143' TERM
trap 'exit 130' INT

fail() {
  echo "inspect.sh: $2" >&2
  exit "$1"
}

case "$type" in
  aab | ipa) ;;
  *) fail 4 "DISTRIBUTION_ARTIFACT_TYPE must be aab or ipa, got '$type'" ;;
esac
[ -f "$file" ] || fail 4 "input $file not found"

python3 -I - "$file" "$type" <<'PYTHON' || exit $?
import os
import struct
import sys
import zipfile

END_OF_DIRECTORY = b'PK\x05\x06'
ZIP64_LOCATOR = b'PK\x06\x07'
ZIP64_END_OF_DIRECTORY = b'PK\x06\x06'
MANIFEST = 'base/manifest/AndroidManifest.xml'

path, kind = sys.argv[1:]
max_entries = int(os.environ.get('MAX_ENTRIES') or 100_000)
max_uncompressed_bytes = int(os.environ.get('MAX_UNCOMPRESSED_BYTES') or 16_000_000_000)
max_manifest_bytes = int(os.environ.get('MAX_MANIFEST_BYTES') or 8 << 20)


def reject(reason):
    print(f'inspect.sh: {reason}', file=sys.stderr)
    sys.exit(2)


def declared_entries(handle):
    handle.seek(0, os.SEEK_END)
    size = handle.tell()
    window = min(size, 0xFFFF + 22)
    handle.seek(size - window)
    tail = handle.read(window)
    position = tail.rfind(END_OF_DIRECTORY)
    if position < 0 or position + 22 > len(tail):
        reject('not a zip archive: no end of central directory record')
    entries = struct.unpack_from('<H', tail, position + 10)[0]
    if entries != 0xFFFF:
        return entries
    locator = position - 20
    if locator < 0 or tail[locator:locator + 4] != ZIP64_LOCATOR:
        return entries
    offset = struct.unpack_from('<Q', tail, locator + 8)[0]
    handle.seek(offset)
    record = handle.read(56)
    if len(record) < 56 or record[:4] != ZIP64_END_OF_DIRECTORY:
        reject('malformed zip64 end of central directory record')
    return struct.unpack_from('<Q', record, 32)[0]


with open(path, 'rb') as handle:
    if declared_entries(handle) > max_entries:
        reject(f'too many entries, limit {max_entries}')

try:
    with zipfile.ZipFile(path) as archive:
        entries = archive.infolist()
except (OSError, zipfile.BadZipFile, struct.error, ValueError):
    reject('not a readable zip archive')

names = [entry.filename for entry in entries]
if len(entries) > max_entries:
    reject(f'too many entries: {len(entries)}, limit {max_entries}')
if len(set(names)) != len(names):
    reject('duplicate entry names')
total = sum(entry.file_size for entry in entries)
if total > max_uncompressed_bytes:
    reject(f'uncompressed size {total} exceeds {max_uncompressed_bytes}')
if kind == 'aab':
    manifest = next((entry for entry in entries if entry.filename == MANIFEST), None)
    if manifest is None:
        reject(f'{MANIFEST} is missing')
    if manifest.file_size > max_manifest_bytes:
        reject(f'{MANIFEST} declares {manifest.file_size} bytes, limit {max_manifest_bytes}')
PYTHON

status=0
case "$type" in
  aab) "$home/inspect-aab.sh" "$file" >"$work/details.json" || status=$? ;;
  ipa) python3 -I "$home/inspect-ipa.py" "$file" >"$work/details.json" || status=$? ;;
esac
[ "$status" = 0 ] || exit "$status"

digest=$(sha256sum "$file" | cut -d' ' -f1)
size=$(stat -c %s "$file")

mkdir -p "$output"
python3 -I - "$work/details.json" "$output/result.json" "$digest" "$size" "$type" <<'PYTHON'
import json
import os
import sys

KEYS = (
    'identifier', 'versionName', 'versionCode', 'signed', 'fingerprint', 'minSdk', 'targetSdk',
    'entitlements', 'profileExpiresAt', 'teamId',
)

details_path, path, digest, size, kind = sys.argv[1:]
with open(details_path, encoding='utf-8') as handle:
    details = json.load(handle)
result = {'digest': digest, 'size': int(size), 'type': kind}
result.update({key: details.get(key) for key in KEYS})
result['signed'] = bool(result['signed'])
partial = os.path.join(os.path.dirname(path), '.result.json.partial')
with open(partial, 'w', encoding='utf-8') as handle:
    json.dump(result, handle)
    handle.write('\n')
os.replace(partial, path)
PYTHON

cat "$output/result.json"
