#!/usr/bin/env bash
set -euo pipefail

fixture="${FIXTURE_IMAGE:-appwrite/distribution-fixture:dev}"
tools="${TOOLS_IMAGE:-appwrite/distribution-tools:dev}"
repository=$(cd "$(dirname "$0")/.." && pwd)
committed="$repository/images/fixture/app-unsigned.aab"
user="$(id -u):$(id -g)"

root=$(mktemp -d "${TMPDIR:-/tmp}/fixture.XXXXXX")
containers=()
cleanup() {
  if [ "${#containers[@]}" -gt 0 ]; then
    docker rm -f "${containers[@]}" >/dev/null 2>&1 || true
  fi
  rm -rf "$root"
}
trap cleanup EXIT

failures=0

pass() {
  echo "ok: $1"
}

fail() {
  echo "not ok: $1" >&2
  failures=$((failures + 1))
}

check() {
  local name=$1
  shift
  if "$@"; then
    pass "$name"
  else
    fail "$name"
  fi
}

workspace() {
  mktemp -d "$root/run.XXXXXX"
}

build() {
  local directory=$1
  shift
  docker run --rm --network none --user "$user" -v "$directory:/mnt/code" "$@" \
    "$fixture" /usr/local/distribution/build.sh
}

status_of() {
  local status=0
  "$@" >"$root/last.log" 2>&1 || status=$?
  echo "$status"
}

evaluate() {
  local directory=$1
  shift
  docker run --rm --network none --user "$user" -v "$directory:/w:ro" --entrypoint python3 "$fixture" -I -c "$@"
}

tool() {
  local directory=$1 entrypoint=$2
  shift 2
  docker run --rm --network none --user "$user" -v "$directory:/mnt/code" --entrypoint "$entrypoint" "$tools" "$@"
}

result_is() {
  evaluate "$1" '
import json, sys
path, identifier, version_name, version_code = sys.argv[1:]
with open(path, encoding="utf-8") as file:
    document = json.load(file)
expected = {
    "artifact": "app-unsigned.aab",
    "type": "aab",
    "identifier": identifier,
    "versionName": version_name,
    "versionCode": int(version_code),
}
problems = [
    f"{key} is {document.get(key)!r}, expected {value!r}"
    for key, value in expected.items()
    if document.get(key) != value or type(document.get(key)) is not type(value)
]
toolchain = document.get("toolchain") or {}
if not str(toolchain.get("image", "")).startswith("appwrite/distribution-fixture:"):
    problems.append(f"toolchain.image is {toolchain.get("image")!r}")
for key in ("fastlane", "java", "gradle", "flutter"):
    if key not in toolchain or toolchain[key] is not None:
        problems.append(f"toolchain.{key} must be null")
for problem in problems:
    print(problem, file=sys.stderr)
sys.exit(1 if problems else 0)
' "/w/$2" "$3" "$4" "$5"
}

unsigned() {
  evaluate "$1" '
import sys, zipfile
names = zipfile.ZipFile(sys.argv[1]).namelist()
signatures = [
    name for name in names
    if name.startswith("META-INF/") and "/" not in name[9:] and name.rsplit(".", 1)[-1] in ("SF", "RSA", "DSA", "EC")
]
sys.exit(1 if signatures or "base/manifest/AndroidManifest.xml" not in names else 0)
' "/w/$2"
}

no_leftovers() {
  [ -z "$(find "$1" -mindepth 1 -name '.*')" ]
}

empty_file() {
  [ -f "$1" ] && [ ! -s "$1" ]
}

absent() {
  [ ! -e "$1" ]
}

manifest_value() {
  tool "$1" bundletool dump manifest --bundle "/mnt/code/$2" --xpath "$3"
}

manifest_is() {
  [ "$(manifest_value "$1" "$2" "$3")" = "$4" ]
}

if ! docker image inspect "$fixture" >/dev/null 2>&1; then
  echo "fixture image $fixture is not built; run make fixture" >&2
  exit 1
fi

if docker image inspect "$tools" >/dev/null 2>&1; then
  tools_present=true
else
  tools_present=false
fi

local_device=$(workspace)
build "$local_device" \
  -e DISTRIBUTION_OUTPUT_DIR=/mnt/code/volume/builds/local \
  -e DISTRIBUTION_CACHE_ARTIFACT=/mnt/code/volume/cache/key.sqfs >/dev/null
output="$local_device/volume/builds/local"
check 'unset identity emits the committed bundle byte for byte' cmp -s "$output/app-unsigned.aab" "$committed"
check 'unset identity bundle is unsigned' unsigned "$local_device" volume/builds/local/app-unsigned.aab
check 'mapping.txt is written empty' empty_file "$output/mapping.txt"
check 'result.json is written to the workspace' result_is "$local_device" output/result.json io.appwrite.distribution.fixture 1.0.0 1
check 'result.json is copied next to the bundle' cmp -s "$local_device/output/result.json" "$output/result.json"
check 'cache placeholder is written' test -s "$local_device/volume/cache/key.sqfs"
check 'no temporary files are left behind' no_leftovers "$local_device"

remote_device=$(workspace)
build "$remote_device" >/dev/null
check 'remote form writes the bundle to the workspace output' cmp -s "$remote_device/output/app-unsigned.aab" "$committed"
check 'remote form writes mapping.txt and result.json' result_is "$remote_device" output/result.json io.appwrite.distribution.fixture 1.0.0 1
check 'remote form leaves no temporary files' no_leftovers "$remote_device"

trailing=$(workspace)
check 'an output directory equal to the workspace output is not copied onto itself' \
  test "$(status_of build "$trailing" -e DISTRIBUTION_OUTPUT_DIR=/mnt/code/output/)" = 0

empty=$(workspace)
build "$empty" -e DISTRIBUTION_IDENTIFIER= -e DISTRIBUTION_VERSION_NAME= -e DISTRIBUTION_VERSION_CODE= >/dev/null
check 'empty identity values are treated as unset' cmp -s "$empty/output/app-unsigned.aab" "$committed"

injected=$(workspace)
build "$injected" \
  -e DISTRIBUTION_IDENTIFIER=com.example.injected \
  -e DISTRIBUTION_VERSION_NAME=4.5.6 \
  -e DISTRIBUTION_VERSION_CODE=57 >/dev/null
check 'injected identity is reported in result.json' result_is "$injected" output/result.json com.example.injected 4.5.6 57
check 'injected bundle is unsigned' unsigned "$injected" output/app-unsigned.aab
check 'injected bundle differs from the committed bundle' test "$(status_of cmp -s "$injected/output/app-unsigned.aab" "$committed")" = 1

partial=$(workspace)
check 'an empty version name with a version code exits 0' \
  test "$(status_of build "$partial" -e DISTRIBUTION_VERSION_NAME= -e DISTRIBUTION_VERSION_CODE=57)" = 0
check 'an empty version name keeps 1.0.0 while the version code is injected' \
  result_is "$partial" output/result.json io.appwrite.distribution.fixture 1.0.0 57

override=$(workspace)
build "$override" \
  -e DISTRIBUTION_IDENTIFIER=com.example.injected \
  -e DISTRIBUTION_VERSION_CODE=57 \
  -e FIXTURE_IDENTIFIER=com.example.other \
  -e FIXTURE_VERSION_CODE=58 >/dev/null
check 'FIXTURE_IDENTIFIER and FIXTURE_VERSION_CODE override the injected identity' \
  result_is "$override" output/result.json com.example.other 1.0.0 58

for invalid in \
  'DISTRIBUTION_IDENTIFIER=fixture' \
  'DISTRIBUTION_IDENTIFIER=com.1example' \
  'DISTRIBUTION_IDENTIFIER=com.exa-mple' \
  'DISTRIBUTION_VERSION_CODE=0' \
  'DISTRIBUTION_VERSION_CODE=2100000001' \
  'DISTRIBUTION_VERSION_CODE=1.5' \
  $'DISTRIBUTION_VERSION_NAME=1.0\n1' \
  "DISTRIBUTION_VERSION_NAME=$(printf 'a%.0s' $(seq 256))" \
  'FIXTURE_LOG_LINES=many' \
  'FIXTURE_EXIT_CODE=256'; do
  rejected=$(workspace)
  label="${invalid//$'\n'/\\n}"
  label="${label:0:60}"
  check "invalid $label exits 2" test "$(status_of build "$rejected" -e "$invalid")" = 2
  check "invalid $label writes no bundle" absent "$rejected/output/app-unsigned.aab"
done

folded=$(workspace)
build "$folded" -e FIXTURE_LOG_LINES=2 -e FIXTURE_LINE_BYTES=40000 >"$folded/log"
check 'long log lines are folded to at most 16384 bytes without losing bytes' evaluate "$folded" '
import sys
lines = open(sys.argv[1], "rb").read().split(b"\n")
longest = max(len(line) for line in lines)
filler = sum(line.count(b"x") for line in lines if set(line) == {ord("x")})
sys.exit(0 if longest <= 16384 and filler == 80000 else 1)
' /w/log

failed=$(workspace)
check 'FIXTURE_EXIT_CODE=1 exits 1' test "$(status_of build "$failed" -e FIXTURE_EXIT_CODE=1)" = 1
check 'a failed build writes no bundle' absent "$failed/output/app-unsigned.aab"
check 'a failed build writes no result.json' absent "$failed/output/result.json"
check 'a failed build still writes mapping.txt' empty_file "$failed/output/mapping.txt"

coded=$(workspace)
check 'FIXTURE_EXIT_CODE=4 exits 4' test "$(status_of build "$coded" -e FIXTURE_EXIT_CODE=4)" = 4

slept=$(workspace)
started=$(date +%s)
build "$slept" -e FIXTURE_SLEEP_SECONDS=2 >/dev/null
check 'FIXTURE_SLEEP_SECONDS delays completion' test "$(($(date +%s) - started))" -ge 2

stopped=$(workspace)
name="fixture-stop-$$"
containers+=("$name")
docker run -d --name "$name" --network none --user "$user" -v "$stopped:/mnt/code" \
  -e FIXTURE_SLEEP_SECONDS=60 "$fixture" /usr/local/distribution/build.sh >/dev/null
sleep 1
started=$(date +%s)
docker stop -t 20 "$name" >/dev/null
check 'a stopped build exits promptly on SIGTERM' test "$(($(date +%s) - started))" -lt 10
check 'a stopped build exits 143' test "$(docker inspect --format '{{.State.ExitCode}}' "$name")" = 143
check 'a stopped build writes no bundle' absent "$stopped/output/app-unsigned.aab"

if [ "$tools_present" = true ]; then
  check 'bundletool reads back the injected package' manifest_is "$injected" output/app-unsigned.aab /manifest/@package com.example.injected
  check 'bundletool reads back the injected versionCode' manifest_is "$injected" output/app-unsigned.aab /manifest/@android:versionCode 57
  check 'bundletool reads back the injected versionName' manifest_is "$injected" output/app-unsigned.aab /manifest/@android:versionName 4.5.6

  keystore=$(workspace)
  tool "$keystore" keytool -genkeypair -storetype PKCS12 -keystore /mnt/code/upload.p12 \
    -storepass fixture-password -keypass fixture-password -alias upload \
    -keyalg RSA -keysize 2048 -validity 10000 -dname 'CN=Distribution Fixture Test' >/dev/null 2>&1

  sign() {
    local directory=$1 bundle=$2
    shift 2
    mkdir -p "$directory/input" "$directory/signing"
    cp "$bundle" "$directory/input/app-unsigned.aab"
    cp "$keystore/upload.p12" "$directory/signing/upload.p12"
    tool "$directory" python3 -I -c '
import base64, json, os, sys
keystore, target = sys.argv[1:]
with open(keystore, "rb") as file:
    encoded = base64.b64encode(file.read()).decode("ascii")
os.remove(keystore)
with open(target, "w", encoding="utf-8") as file:
    json.dump({"keystore": encoded, "storePassword": "fixture-password", "keyAlias": "upload", "keyPassword": "fixture-password"}, file)
' /mnt/code/signing/upload.p12 /mnt/code/signing/bundle.json
    docker run --rm --network none --user "$user" -v "$directory:/mnt/code" "$@" \
      --entrypoint /usr/local/distribution/sign-android.sh "$tools"
  }

  signed=$(workspace)
  check 'sign-android.sh signs the injected bundle' test "$(status_of sign "$signed" "$injected/output/app-unsigned.aab" \
    -e DISTRIBUTION_IDENTIFIER=com.example.injected -e DISTRIBUTION_VERSION_CODE=57)" = 0
  check 'sign-android.sh writes the signed bundle' test -s "$signed/output/app.aab"
  check 'sign-android.sh reports the injected identity' evaluate "$signed" '
import json, sys
document = json.load(open(sys.argv[1], encoding="utf-8"))
sys.exit(0 if document.get("identifier") == "com.example.injected" and document.get("versionCode") == 57 else 1)
' /w/output/result.json

  refused=$(workspace)
  check 'sign-android.sh refuses a FIXTURE_IDENTIFIER override with exit 3' test "$(status_of sign "$refused" "$override/output/app-unsigned.aab" \
    -e DISTRIBUTION_IDENTIFIER=com.example.injected -e DISTRIBUTION_VERSION_CODE=58)" = 3
else
  echo "skip: tools image not built ($tools): bundletool read-back and sign-android.sh"
fi

if [ "$failures" -gt 0 ]; then
  echo "$failures fixture check(s) failed" >&2
  exit 1
fi
echo 'all fixture checks passed'
