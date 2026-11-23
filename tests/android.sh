#!/usr/bin/env bash
set -euo pipefail

image=${ANDROID_IMAGE:-appwrite/distribution-android:dev}
workspace=$(mktemp -d "${TMPDIR:-/tmp}/android.XXXXXX")
log=$workspace.log
identifier=io.appwrite.distribution.smoke
output=$workspace/volume/builds/smoke

inside() {
  docker run --rm -v "$workspace:/mnt/code" "$image" "$@"
}

cleanup() {
  inside find /mnt/code -mindepth 1 -delete > /dev/null 2>&1 || true
  rmdir "$workspace" 2> /dev/null || true
  rm -f "$log"
}
trap cleanup EXIT

job() {
  local status=0
  docker run --rm --memory 8g -v "$workspace:/mnt/code" \
    -e DISTRIBUTION_FRAMEWORK=flutter \
    -e DISTRIBUTION_ROOT_DIRECTORY=smoke \
    -e DISTRIBUTION_INSTALL_COMMAND='flutter pub get' \
    -e DISTRIBUTION_BUILD_COMMAND='flutter build appbundle --release' \
    -e DISTRIBUTION_OUTPUT_PATH='build/app/outputs/bundle/release/*.aab' \
    -e DISTRIBUTION_OUTPUT_DIR=/mnt/code/volume/builds/smoke \
    -e DISTRIBUTION_CACHE_ARTIFACT=cache/smoke.sqfs \
    -e DISTRIBUTION_VERSION_NAME=9.8.7-smoke \
    -e DISTRIBUTION_VERSION_CODE=4242 \
    "$@" "$image" /usr/local/distribution/build.sh 2>&1 | tee "$log" || status=$?
  return "$status"
}

pass() {
  echo "pass: $*"
}

fail() {
  echo "fail: $*" >&2
  exit 1
}

expect_status() {
  local expected=$1 actual=$2 name=$3
  if [[ $actual != "$expected" ]]; then
    fail "$name exited $actual, expected $expected"
  fi
}

expect_log() {
  if ! grep -qF -- "$1" "$log"; then
    fail "log does not contain: $1"
  fi
}

reset_outputs() {
  inside rm -rf /mnt/code/output /mnt/code/volume
}

manifest() {
  docker run --rm --network none -v "$output:/w:ro" --entrypoint bundletool "$image" \
    dump manifest --bundle /w/app-unsigned.aab --xpath "$1"
}

check_folded() {
  local longest
  longest=$(LC_ALL=C awk '{ if (length($0) > longest) longest = length($0) } END { print longest + 0 }' "$log")
  if (( longest > 16384 )); then
    fail "an output line is $longest bytes, over 16384"
  fi
}

check_result() {
  local code=$1
  cmp -s "$workspace/output/result.json" "$output/result.json" || fail 'the two result.json copies differ'
  python3 -I - "$workspace/output/result.json" "$output/app-unsigned.aab" "$workspace/source/smoke/android/gradle/wrapper/gradle-wrapper.properties" "$image" "$identifier" "$code" <<'PYTHON'
import json
import sys
import zipfile

result_path, bundle_path, wrapper_path, image, identifier, code = sys.argv[1:]

with open(result_path, encoding='utf-8') as handle:
    result = json.load(handle)

gradle = None
with open(wrapper_path, encoding='utf-8') as handle:
    for line in handle:
        if line.startswith('distributionUrl='):
            gradle = line.strip().rsplit('/gradle-', 1)[1].rsplit('-', 1)[0]
major, minor = (int(part) for part in gradle.split('.')[:2])
java = '17.' if (major, minor) < (8, 5) else '21.'

expected = {
    'artifact': 'app-unsigned.aab',
    'type': 'aab',
    'identifier': identifier,
    'versionName': '9.8.7-smoke',
    'versionCode': int(code),
}
problems = [f'{key} is {result.get(key)!r}, expected {value!r}' for key, value in expected.items() if result.get(key) != value]
if type(result.get('versionCode')) is not int:
    problems.append('versionCode is not a JSON integer')
toolchain = result.get('toolchain', {})
if toolchain.get('image') != image:
    problems.append(f'toolchain.image is {toolchain.get("image")!r}, expected {image!r}')
if toolchain.get('fastlane') != '2.242.0':
    problems.append(f'toolchain.fastlane is {toolchain.get("fastlane")!r}')
if not str(toolchain.get('java')).startswith(java):
    problems.append(f'toolchain.java is {toolchain.get("java")!r}, expected {java}x for Gradle {gradle}')
if toolchain.get('gradle') != gradle:
    problems.append(f'toolchain.gradle is {toolchain.get("gradle")!r}, expected {gradle!r}')
if toolchain.get('flutter') != '3.41.2':
    problems.append(f'toolchain.flutter is {toolchain.get("flutter")!r}')

with zipfile.ZipFile(bundle_path) as bundle:
    signatures = [
        name for name in bundle.namelist()
        if name.startswith('META-INF/') and name.count('/') == 1 and name.upper().endswith(('.SF', '.RSA', '.DSA', '.EC'))
    ]
if signatures:
    problems.append(f'the bundle has signature entries: {signatures}')

if problems:
    sys.exit('\n'.join(problems))
PYTHON
  [[ $(manifest /manifest/@package) == "$identifier" ]] || fail 'manifest package differs'
  [[ $(manifest /manifest/@android:versionCode) == "$code" ]] || fail 'manifest versionCode differs'
  [[ $(manifest /manifest/@android:versionName) == 9.8.7-smoke ]] || fail 'manifest versionName differs'
  [[ -f $output/mapping.txt ]] || fail 'mapping.txt is missing'
  if [[ -n $(find "$workspace/output" "$output" "$workspace/cache" -name '.*' -type f -print -quit) ]]; then
    fail 'temporary files were left behind'
  fi
}

check_cache() {
  local listing
  [[ -f $workspace/cache/smoke.sqfs ]] || fail 'the cache was not saved'
  listing=$(inside unsquashfs -l /mnt/code/cache/smoke.sqfs)
  grep -q '^squashfs-root/gradle/caches/' <<< "$listing" || fail 'the cache has no gradle caches'
  grep -q '^squashfs-root/gradle/wrapper/' <<< "$listing" || fail 'the cache has no gradle wrapper'
  if grep -q -e '^squashfs-root/gradle/init.d' -e '^squashfs-root/gradle/gradle.properties' <<< "$listing"; then
    fail 'the cache contains the init script or gradle.properties'
  fi
}

test_cold() {
  local status=0
  inside flutter create --org io.appwrite.distribution --project-name smoke --platforms android /mnt/code/source/smoke > /dev/null
  job || status=$?
  expect_status 0 "$status" 'cold build'
  expect_log 'cache: none'
  expect_log 'heap: 4096m'
  check_folded
  check_result 4242
  check_cache
  pass 'cold flutter build produced an unsigned bundle, result.json and a cache'
}

test_warm() {
  local status=0
  reset_outputs
  inside rm -rf /mnt/code/.distribution /mnt/code/source/smoke/build
  job -e DISTRIBUTION_VERSION_CODE=4243 || status=$?
  expect_status 0 "$status" 'warm build'
  expect_log 'cache: restored'
  check_result 4243
  pass 'warm flutter build restored the cache'
}

test_signed() {
  local status=0
  reset_outputs
  job \
    -e DISTRIBUTION_INSTALL_COMMAND= \
    -e DISTRIBUTION_BUILD_COMMAND="python3 -c \"import os, zipfile; os.makedirs('signed', exist_ok=True); bundle = zipfile.ZipFile('signed/app.aab', 'w'); bundle.writestr('META-INF/UPLOAD.SF', 'x'); bundle.close()\"" \
    -e DISTRIBUTION_OUTPUT_PATH='signed/*.aab' \
    -e DISTRIBUTION_CACHE_ARTIFACT= \
    || status=$?
  expect_status 3 "$status" 'signed build'
  expect_log 'build_already_signed'
  [[ ! -e $output/app-unsigned.aab ]] || fail 'a signed bundle was published'
  pass 'a signed bundle exits 3 with build_already_signed'
}

test_keystore() {
  local status=0
  reset_outputs
  inside sh -c 'printf "\nval keystoreProperties = java.util.Properties().apply { load(java.io.FileInputStream(rootProject.file(\"key.properties\"))) }\n" >> /mnt/code/source/smoke/android/app/build.gradle.kts'
  job -e DISTRIBUTION_INSTALL_COMMAND= -e DISTRIBUTION_CACHE_ARTIFACT= || status=$?
  expect_status 4 "$status" 'keystore build'
  expect_log 'build_keystore_required'
  pass 'an unguarded keystore read exits 4 with build_keystore_required'
}

test_cold
test_warm
test_signed
test_keystore
