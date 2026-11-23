#!/bin/sh
set -eu

EXIT_MALFORMED=2
EXIT_INVALID=3
EXIT_USAGE=4
EXIT_TIMEOUT=8
EXIT_INTERRUPTED=130
EXIT_TERMINATED=143
TIMED_OUT=124
KILLED=137
KILL_AFTER_SECONDS=10

home=$(cd "$(dirname "$0")" && pwd)
file="$1"
signatures="$2"
java_seconds="${DISTRIBUTION_MAX_JAVA_SECONDS:-600}"

exec 3>&2
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
  echo "inspect-aab.sh: $2" >&3
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

case "$java_seconds" in
  '' | 0* | *[!0-9]*) fail "$EXIT_USAGE" "DISTRIBUTION_MAX_JAVA_SECONDS must be a positive integer, got '$java_seconds'" ;;
esac

run_java bundletool dump manifest --bundle "$file" >"$work/manifest.xml" 2>"$work/bundletool.log" \
  || { cat "$work/bundletool.log" >&2; fail "$EXIT_MALFORMED" 'cannot read the bundle manifest'; }
run python3 -I "$home/manifest.py" --exit "$EXIT_MALFORMED" "$work/manifest.xml" >"$work/manifest.json" || exit $?

signed=false
fingerprint=''
if [ "$signatures" != 0 ]; then
  run_java jarsigner -verify -verbose -certs "$file" >"$work/verify.log" 2>&1 || true
  run python3 -I "$home/signature.py" --exit "$EXIT_INVALID" "$work/verify.log" || exit $?
  run_java keytool -printcert -jarfile "$file" >"$work/signer.txt" 2>&1 \
    || fail "$EXIT_INVALID" 'signature present but no readable signer certificate'
  fingerprint=$(grep -F 'SHA256:' "$work/signer.txt" | head -n 1 | cut -d: -f2- | tr -d ' :' | tr 'A-F' 'a-f')
  [ -n "$fingerprint" ] || fail "$EXIT_INVALID" 'signature present but no signer certificate'
  signed=true
fi

python3 -I - "$work/manifest.json" "$signed" "$fingerprint" <<'PYTHON'
import json
import sys

manifest_path, signed, fingerprint = sys.argv[1:]
with open(manifest_path, encoding='utf-8') as handle:
    manifest = json.load(handle)
print(json.dumps({
    'type': 'aab',
    **manifest,
    'signed': signed == 'true',
    'fingerprint': fingerprint or None,
}))
PYTHON
