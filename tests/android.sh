#!/usr/bin/env bash
set -euo pipefail

image=${ANDROID_IMAGE:-appwrite/distribution-android:dev}
workspace=$(mktemp -d "${TMPDIR:-/tmp}/android.XXXXXX")
log=$workspace.log
identifier=io.appwrite.distribution.smoke
output=$workspace/volume/builds/smoke
container=android-terminate-$$
script=/mnt/code/source/smoke/android/app/build.gradle.kts

inside() {
  docker run --rm -v "$workspace:/mnt/code" "$image" "$@"
}

cleanup() {
  docker rm -f "$container" > /dev/null 2>&1 || true
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
    -e DISTRIBUTION_IDENTIFIER="$identifier" \
    -e DISTRIBUTION_VERSION_NAME=9.8.7-smoke \
    -e DISTRIBUTION_VERSION_CODE=4242 \
    "$@" "$image" /bin/sh -c /usr/local/distribution/build.sh 2>&1 | tee "$log" || status=$?
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

reject_log() {
  if grep -qF -- "$1" "$log"; then
    fail "log contains: $1"
  fi
}

reset_outputs() {
  inside rm -rf /mnt/code/output /mnt/code/volume
}

restore_script() {
  inside cp "$script.pristine" "$script"
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

expected = {
    'artifact': 'app-unsigned.aab',
    'type': 'aab',
    'identifier': identifier,
    'versionName': '9.8.7-smoke',
    'versionCode': int(code),
    'toolchain': {
        'image': image,
        'fastlane': '2.242.0',
        'java': '21.0.13.1+2-LTS',
        'gradle': gradle,
        'flutter': '3.41.2',
    },
}
problems = [f'{key} is {result.get(key)!r}, expected {value!r}' for key, value in expected.items() if result.get(key) != value]
if type(result.get('versionCode')) is not int:
    problems.append('versionCode is not a JSON integer')

with zipfile.ZipFile(bundle_path) as bundle:
    signatures = [
        name for name in (entry.upper() for entry in bundle.namelist())
        if name.startswith('META-INF/') and '/' not in name[9:]
        and (name.endswith(('.SF', '.RSA', '.DSA', '.EC')) or name[9:].startswith('SIG-'))
    ]
if signatures:
    problems.append(f'the bundle has signature entries: {signatures}')

if problems:
    sys.exit('\n'.join(problems))
PYTHON
  [[ $(manifest /manifest/@package) == "$identifier" ]] || fail 'manifest package differs'
  [[ $(manifest /manifest/@android:versionCode) == "$code" ]] || fail 'manifest versionCode differs'
  [[ $(manifest /manifest/@android:versionName) == 9.8.7-smoke ]] || fail 'manifest versionName differs'
  [[ -s $output/mapping.txt ]] || fail 'mapping.txt is missing or empty for a minified release build'
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
  inside cp "$script" "$script.pristine"
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
  inside rm -rf /mnt/code/.distribution /mnt/code/source/smoke/build /mnt/code/source/smoke/.dart_tool /mnt/code/source/smoke/android/.gradle /mnt/code/source/smoke/android/app/.cxx
  job -e DISTRIBUTION_VERSION_CODE=4243 || status=$?
  expect_status 0 "$status" 'warm build'
  expect_log 'cache: restored'
  check_result 4243
  check_cache
  pass 'warm flutter build restored the cache and saved it again'
}

test_signed() {
  local status=0
  reset_outputs
  inside sh -c 'mkdir -p /mnt/code/cache && echo unreadable > /mnt/code/cache/corrupt.sqfs'
  job \
    -e DISTRIBUTION_INSTALL_COMMAND= \
    -e DISTRIBUTION_BUILD_COMMAND="python3 -c \"import os, zipfile; os.makedirs('signed', exist_ok=True); bundle = zipfile.ZipFile('signed/app.aab', 'w'); bundle.writestr('meta-inf/UPLOAD.SF', 'x'); bundle.close()\"" \
    -e DISTRIBUTION_OUTPUT_PATH='signed/*.aab' \
    -e DISTRIBUTION_CACHE_ARTIFACT=cache/corrupt.sqfs \
    || status=$?
  expect_status 3 "$status" 'signed build'
  expect_log 'build_already_signed'
  expect_log 'cache: unreadable, continuing without it'
  [[ ! -e $output/app-unsigned.aab ]] || fail 'a signed bundle was published'
  [[ ! -e $workspace/cache/corrupt.sqfs ]] || fail 'the unreadable cache was left in the workspace'
  pass 'a signed bundle exits 3 with build_already_signed, and an unreadable cache is skipped and removed'
}

test_keystore() {
  local status=0
  reset_outputs
  inside sh -c "printf '\nval keystoreText = rootProject.file(\"key.properties\").readText()\n' >> $script"
  job -e DISTRIBUTION_INSTALL_COMMAND= -e DISTRIBUTION_CACHE_ARTIFACT= || status=$?
  restore_script
  expect_status 4 "$status" 'keystore build'
  expect_log 'key.properties (No such file or directory)'
  expect_log 'build_keystore_required'
  pass 'an unguarded key.properties read exits 4 with build_keystore_required'
}

test_compilation_error() {
  local status=0
  reset_outputs
  inside sh -c "printf '\nval signingNote: Int = \"key.properties\"\n' >> $script"
  job -e DISTRIBUTION_INSTALL_COMMAND= -e DISTRIBUTION_CACHE_ARTIFACT= || status=$?
  restore_script
  expect_status 1 "$status" 'compilation error build'
  expect_log 'Script compilation error'
  reject_log 'build_keystore_required'
  pass 'a build script compilation error that mentions key.properties exits 1, not 4'
}

test_java_17() {
  local status=0
  reset_outputs
  inside sh -c 'mkdir -p /mnt/code/source/legacy/gradle/wrapper && printf "%s\n" "distributionUrl = https\\://services.gradle.org/distributions/gradle-8.4-bin.zip" > /mnt/code/source/legacy/gradle/wrapper/gradle-wrapper.properties'
  job \
    -e DISTRIBUTION_FRAMEWORK= \
    -e DISTRIBUTION_ROOT_DIRECTORY=legacy \
    -e DISTRIBUTION_INSTALL_COMMAND= \
    -e DISTRIBUTION_BUILD_COMMAND='java -version' \
    -e DISTRIBUTION_OUTPUT_PATH='none/*.aab' \
    -e DISTRIBUTION_CACHE_ARTIFACT= \
    || status=$?
  expect_status 2 "$status" 'java 17 build'
  expect_log 'gradle: 8.4, java: /opt/java/17'
  expect_log 'version "17.0.21.1"'
  pass 'a Gradle 8.4 wrapper written with spaces selects JDK 17'
}

test_react_native() {
  local status=0
  reset_outputs
  inside mkdir -p /mnt/code/source/native/android
  job \
    -e DISTRIBUTION_FRAMEWORK=react-native \
    -e DISTRIBUTION_ROOT_DIRECTORY=native \
    -e DISTRIBUTION_INSTALL_COMMAND=pwd \
    -e DISTRIBUTION_BUILD_COMMAND=pwd \
    -e DISTRIBUTION_OUTPUT_PATH='android/app/build/outputs/bundle/release/*.aab' \
    -e DISTRIBUTION_CACHE_ARTIFACT= \
    || status=$?
  expect_status 2 "$status" 'react native layout'
  grep -qxF /mnt/code/source/native "$log" || fail 'the install command did not run in the root'
  expect_log /mnt/code/source/native/android
  pass 'react-native installs in the root and builds in android/'
}

test_terminate() {
  local status started=0 elapsed
  reset_outputs
  docker run -d --name "$container" -v "$workspace:/mnt/code" \
    -e DISTRIBUTION_ROOT_DIRECTORY=smoke \
    -e DISTRIBUTION_BUILD_COMMAND='sleep 600' \
    -e DISTRIBUTION_OUTPUT_PATH='none/*.aab' \
    "$image" /bin/sh -c /usr/local/distribution/build.sh > /dev/null
  for _ in $(seq 1 120); do
    docker logs "$container" > "$log" 2>&1
    if grep -qF '$ sleep 600' "$log"; then
      started=1
      break
    fi
    sleep 1
  done
  (( started )) || fail 'the lane never started the build command'
  expect_log '[distribution] build: sleep 600'
  SECONDS=0
  docker stop -t 60 "$container" > /dev/null
  elapsed=$SECONDS
  status=$(docker inspect -f '{{.State.ExitCode}}' "$container")
  docker logs "$container" > "$log" 2>&1
  docker rm "$container" > /dev/null
  expect_status 143 "$status" 'terminated build'
  expect_log 'build: terminated by signal'
  (( elapsed < 20 )) || fail "the build took ${elapsed}s to stop after SIGTERM"
  pass "SIGTERM through /bin/sh -c stops the build in ${elapsed}s with exit 143, and logs stream live"
}

test_cold
test_warm
test_signed
test_keystore
test_compilation_error
test_java_17
test_react_native
test_terminate
