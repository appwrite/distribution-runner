#!/bin/sh
set -eu

DISTRIBUTION_HOME=/usr/local/distribution
DISTRIBUTION_SOURCE="${DISTRIBUTION_SOURCE:-/mnt/code/source}"
export DISTRIBUTION_HOME DISTRIBUTION_SOURCE

workspace=/mnt/code
output="${DISTRIBUTION_OUTPUT_DIR:-$workspace/output}"
image="${DISTRIBUTION_IMAGE:-appwrite/distribution-fixture:dev}"

identifier="${FIXTURE_IDENTIFIER:-${DISTRIBUTION_IDENTIFIER:-}}"
version_name="${DISTRIBUTION_VERSION_NAME:-}"
version_code="${FIXTURE_VERSION_CODE:-${DISTRIBUTION_VERSION_CODE:-}}"

exit_code="${FIXTURE_EXIT_CODE:-0}"
sleep_seconds="${FIXTURE_SLEEP_SECONDS:-0}"
log_lines="${FIXTURE_LOG_LINES:-3}"
line_bytes="${FIXTURE_LINE_BYTES:-0}"

fail() {
  echo "build.sh: $2" >&2
  exit "$1"
}

integer() {
  case "$2" in
    '' | *[!0-9]*) fail 2 "$1 must be a non-negative integer, got '$2'" ;;
  esac
}

integer FIXTURE_EXIT_CODE "$exit_code"
integer FIXTURE_SLEEP_SECONDS "$sleep_seconds"
integer FIXTURE_LOG_LINES "$log_lines"
integer FIXTURE_LINE_BYTES "$line_bytes"
[ "$exit_code" -le 255 ] 2>/dev/null || fail 2 "FIXTURE_EXIT_CODE must be at most 255, got '$exit_code'"

staging=''
cleanup() {
  [ -z "$staging" ] || rm -rf "$staging"
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT

publish() {
  partial="$(dirname "$2")/.$(basename "$2").partial"
  cp "$1" "$partial"
  mv "$partial" "$2"
}

canonical() {
  (cd "$1" && pwd -P)
}

logs() {
  echo "fixture: ${identifier:-default identity}, image $image"
  line=0
  while [ "$line" -lt "$log_lines" ]; do
    line=$((line + 1))
    if [ "$line_bytes" -gt 0 ]; then
      head -c "$line_bytes" /dev/zero | tr '\0' 'x'
      echo
    else
      echo "fixture: log line $line of $log_lines"
    fi
  done
}

mkdir -p "$output" "$workspace/output"
: > "$output/mapping.txt"

logs | fold -b -w 16384

if [ "$sleep_seconds" -gt 0 ]; then
  sleep "$sleep_seconds" &
  wait "$!"
fi

if [ "$exit_code" -ne 0 ]; then
  fail "$exit_code" "failing with FIXTURE_EXIT_CODE=$exit_code"
fi

staging=$(mktemp -d "$output/.fixture.XXXXXX")
bundle="$staging/app-unsigned.aab"
result="$staging/result.json"

if [ -z "$identifier$version_name$version_code" ]; then
  cp "$DISTRIBUTION_HOME/app-unsigned.aab" "$bundle"
else
  python3 -I "$DISTRIBUTION_HOME/manifest.py" rewrite \
    --identifier "$identifier" \
    --version-name "$version_name" \
    --version-code "$version_code" \
    "$DISTRIBUTION_HOME/app-unsigned.aab" "$bundle"
fi

python3 -I "$DISTRIBUTION_HOME/manifest.py" result \
  --template "$DISTRIBUTION_HOME/result.json" \
  --image "$image" \
  "$bundle" > "$result"

mv "$bundle" "$output/app-unsigned.aab"
publish "$result" "$workspace/output/result.json"
if [ "$(canonical "$output")" != "$(canonical "$workspace/output")" ]; then
  publish "$result" "$output/result.json"
fi

if [ -n "${DISTRIBUTION_CACHE_ARTIFACT:-}" ]; then
  cache="$DISTRIBUTION_CACHE_ARTIFACT"
  case "$cache" in
    /*) ;;
    *) cache="$workspace/$cache" ;;
  esac
  mkdir -p "$(dirname "$cache")"
  echo 'appwrite distribution fixture cache placeholder' > "$staging/cache"
  publish "$staging/cache" "$cache"
fi

cat "$workspace/output/result.json"
