#!/bin/sh
set -eu

EXIT_MALFORMED=2
EXIT_USAGE=4
EXIT_INTERRUPTED=130
EXIT_TERMINATED=143

home=$(cd "$(dirname "$0")" && pwd)
workspace="${DISTRIBUTION_WORKSPACE:-/mnt/code}"
file="${1:-${DISTRIBUTION_INPUT:-$workspace/input/artifact}}"
type="${DISTRIBUTION_ARTIFACT_TYPE:-}"
output="$workspace/output"

work=$(mktemp -d)
child=''
interrupt() {
  [ -z "$child" ] || kill "$child" 2>/dev/null || true
  exit "$1"
}
trap 'rm -rf "$work"' EXIT
trap 'interrupt $EXIT_TERMINATED' TERM
trap 'interrupt $EXIT_INTERRUPTED' INT

fail() {
  echo "inspect.sh: $2" >&2
  exit "$1"
}

run() {
  "$@" &
  child=$!
  status=0
  wait "$child" || status=$?
  child=''
  return "$status"
}

case "$type" in
  aab | ipa) ;;
  *) fail "$EXIT_USAGE" "DISTRIBUTION_ARTIFACT_TYPE must be aab or ipa, got '$type'" ;;
esac
[ -f "$file" ] || fail "$EXIT_USAGE" "input $file not found"

run python3 -I "$home/archive.py" --malformed "$EXIT_MALFORMED" --usage "$EXIT_USAGE" --type "$type" "$file" \
  >"$work/signatures" || exit $?

case "$type" in
  aab) run "$home/inspect-aab.sh" "$file" "$(cat "$work/signatures")" >"$work/details.json" || exit $? ;;
  ipa) run python3 -I "$home/inspect-ipa.py" "$file" >"$work/details.json" || exit $? ;;
esac

run sha256sum "$file" >"$work/digest"
digest=$(cut -d' ' -f1 "$work/digest")
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
