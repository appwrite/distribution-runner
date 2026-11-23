#!/bin/bash
set -euo pipefail
shopt -s inherit_errexit

readonly workspace=/mnt/code
readonly home=$workspace/.distribution
readonly width=16384
readonly transcript_bytes=1048576
readonly stale_minutes=60
readonly stop_seconds=60
readonly fastlane_version=2.242.0
readonly java_17=/opt/java/17
readonly java_21=/opt/java/21
readonly sdk=/opt/android-sdk
readonly flutter_root=/opt/flutter

export DISTRIBUTION_HOME=/usr/local/distribution
export DISTRIBUTION_SOURCE=${DISTRIBUTION_SOURCE:-$workspace/source}
export DISTRIBUTION_ROOT_DIRECTORY=${DISTRIBUTION_ROOT_DIRECTORY:-.}
export DISTRIBUTION_OUTPUT_DIR=${DISTRIBUTION_OUTPUT_DIR:-$workspace/output}
export ANDROID_HOME=$sdk
export ANDROID_SDK_ROOT=$sdk
export FLUTTER_ROOT=$flutter_root
export PATH=$java_21/bin:$sdk/cmdline-tools/latest/bin:$sdk/platform-tools:/opt/node/bin:$flutter_root/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export GRADLE_USER_HOME=$home/gradle
export PUB_CACHE=$home/pub
export npm_config_cache=$home/npm
export ANDROID_USER_HOME=$home/android
export COREPACK_ENABLE_DOWNLOAD_PROMPT=0
export CI=true

gradle_version=''
gradle_project=''
temporary=()

info() {
  printf '[distribution] %s\n' "$*"
}

fail() {
  local code=$1
  shift
  printf '[distribution] %s\n' "$*" >&2
  exit "$code"
}

failed() {
  local status=$1 code=$1
  shift
  if (( code >= 2 && code <= 4 )); then
    code=1
  fi
  fail "$code" "$*: failed with exit status $status"
}

cleanup() {
  if (( ${#temporary[@]} )); then
    rm -rf -- "${temporary[@]}"
  fi
}

temporary_file() {
  local path
  path=$(mktemp "$2/.$3.XXXXXX") || return 1
  temporary+=("$path")
  printf -v "$1" '%s' "$path"
}

commit() {
  chmod 0644 -- "$1" && mv -f -- "$1" "$2"
}

sweep() {
  local directory=$1 minutes=$2 name filter=()
  shift 2
  if [[ ! -d $directory ]]; then
    return 0
  fi
  if (( minutes )); then
    filter=(-mmin "+$minutes")
  fi
  for name in "$@"; do
    find "$directory" -maxdepth 1 -type f -name ".$name.??????" "${filter[@]}" -delete
  done
}

cache_artifact() {
  local path=${DISTRIBUTION_CACHE_ARTIFACT:-}
  if [[ -n $path && $path != /* ]]; then
    path=$workspace/$path
  fi
  printf '%s' "$path"
}

mount_point() {
  local path=$1 point best=/
  while read -r _ _ _ _ point _; do
    if [[ $path == "$point" || $path == "$point"/* ]] && (( ${#point} > ${#best} )); then
      best=$point
    fi
  done < /proc/self/mountinfo
  printf '%s' "$best"
}

in_workspace() {
  local path
  path=$(realpath -e -- "$1") || return 1
  [[ $path == "$workspace"/* && $(mount_point "$path") == "$(mount_point "$workspace")" ]]
}

require() {
  local name
  for name in "$@"; do
    if [[ -z ${!name:-} ]]; then
      fail 2 "$name is required"
    fi
  done
}

sweep_temporary() {
  local artifact
  sweep "$DISTRIBUTION_OUTPUT_DIR" 0 app-unsigned.aab mapping.txt result.json
  sweep "$workspace/output" 0 result.json
  artifact=$(cache_artifact)
  if [[ -n $artifact ]]; then
    sweep "$(dirname "$artifact")" "$stale_minutes" "$(basename "$artifact")"
  fi
}

restore_cache() {
  local artifact
  artifact=$(cache_artifact)
  mkdir -p "$home"
  if [[ -z $artifact || ! -f $artifact ]]; then
    info 'cache: none'
    return 0
  fi
  info "cache: restoring $artifact"
  if unsquashfs -f -no-xattrs -no-progress -quiet -d "$home" "$artifact"; then
    info 'cache: restored'
  else
    info 'cache: unreadable, continuing without it'
    rm -rf "$home"
    mkdir -p "$home"
  fi
  if in_workspace "$artifact"; then
    rm -f -- "$artifact"
  fi
}

resolve_root() {
  local source root
  source=$(realpath -e -- "$DISTRIBUTION_SOURCE") || fail 2 "source directory $DISTRIBUTION_SOURCE not found"
  root=$(realpath -e -- "$source/$DISTRIBUTION_ROOT_DIRECTORY") || fail 2 "root directory $DISTRIBUTION_ROOT_DIRECTORY not found"
  if [[ $root != "$source" && $root != "$source"/* ]]; then
    fail 2 'root directory escapes the checkout'
  fi
  if [[ ${DISTRIBUTION_FRAMEWORK:-} == react-native && ! -d $root/android ]]; then
    fail 2 'react-native: the root directory has no android directory'
  fi
  printf '%s' "$root"
}

install_dependencies() {
  local root=$1
  if [[ -z ${DISTRIBUTION_INSTALL_COMMAND:-} ]]; then
    return 0
  fi
  info "install: $DISTRIBUTION_INSTALL_COMMAND"
  (cd "$root" && sh -c "$DISTRIBUTION_INSTALL_COMMAND")
}

wrapper_version() {
  local line key value version
  while IFS= read -r line || [[ -n $line ]]; do
    line=${line%$'\r'}
    key=${line%%[=:]*}
    key=${key//[[:space:]]/}
    if [[ $key != distributionUrl || $line != *[=:]* ]]; then
      continue
    fi
    value=${line#*[=:]}
    if [[ $value != */gradle-* ]]; then
      return 0
    fi
    version=${value##*/gradle-}
    version=${version%-*}
    if [[ $version == [0-9]*.* ]]; then
      printf '%s' "$version"
    fi
    return 0
  done < "$1"
}

detect_gradle() {
  local root=$1 project properties
  for project in "$root" "$root/android"; do
    properties=$project/gradle/wrapper/gradle-wrapper.properties
    if [[ ! -f $properties ]]; then
      continue
    fi
    gradle_project=$project
    gradle_version=$(wrapper_version "$properties")
    return 0
  done
}

select_java() {
  local version=$1 major rest minor
  if [[ -z $version ]]; then
    printf '%s' "$java_21"
    return 0
  fi
  major=${version%%.*}
  rest=${version#*.}
  minor=${rest%%[!0-9]*}
  if [[ -z $major || $major == *[!0-9]* || -z $minor ]]; then
    printf '%s' "$java_21"
  elif (( major < 8 || (major == 8 && minor < 5) )); then
    printf '%s' "$java_17"
  else
    printf '%s' "$java_21"
  fi
}

memory_limit() {
  local line path='' file value limit=''
  if [[ -r /proc/self/cgroup ]]; then
    while IFS= read -r line; do
      if [[ $line == 0::* ]]; then
        path=${line#0::}
      fi
    done < /proc/self/cgroup
  fi
  while [[ -n $path ]]; do
    file=/sys/fs/cgroup${path%/}/memory.max
    if [[ -r $file ]]; then
      value=$(< "$file")
      if [[ -n $value && $value != *[!0-9]* ]] && { [[ -z $limit ]] || (( value < limit )); }; then
        limit=$value
      fi
    fi
    if [[ $path == / ]]; then
      break
    fi
    path=${path%/*}
    path=${path:-/}
  done
  if [[ -z $limit && -r /sys/fs/cgroup/memory/memory.limit_in_bytes ]]; then
    limit=$(< /sys/fs/cgroup/memory/memory.limit_in_bytes)
  fi
  printf '%s' "$limit"
}

heap_megabytes() {
  local limit
  limit=$(memory_limit)
  if [[ -z $limit || $limit == *[!0-9]* ]] || (( limit > (1 << 50) )); then
    limit=$((8 << 30))
  fi
  printf '%s' $((limit / 2 / 1048576))
}

configure_gradle() {
  local root=$1 java heap
  detect_gradle "$root"
  java=$(select_java "$gradle_version")
  heap=$(heap_megabytes)
  export JAVA_HOME=$java
  export PATH=$java/bin:$PATH
  mkdir -p "$GRADLE_USER_HOME/init.d" "$ANDROID_USER_HOME"
  cat > "$GRADLE_USER_HOME/gradle.properties" <<PROPERTIES
org.gradle.java.home=$java
org.gradle.java.installations.paths=$java_17,$java_21
org.gradle.jvmargs=-Xmx${heap}m -XX:MaxMetaspaceSize=1g -Dfile.encoding=UTF-8
kotlin.compiler.execution.strategy=in-process
PROPERTIES
  cp "$DISTRIBUTION_HOME/distribution.init.gradle" "$GRADLE_USER_HOME/init.d/distribution.init.gradle"
  info "gradle: ${gradle_version:-unknown}, java: $java, heap: ${heap}m"
}

keystore_required() {
  python3 -I - "$1" "$DISTRIBUTION_SOURCE" <<'PYTHON'
import os
import sys

transcript, source = sys.argv[1], os.path.realpath(sys.argv[2])
markers = ('keystore', 'key.properties', 'storefile', 'storepassword', 'keyalias', 'keypassword', 'signingconfig', '.jks')
headers = ('Execution failed for task ', 'A problem occurred ')
prefix = "Build file '"
separator = "' line: "


def strip(line):
    return line.split('▸ ', 1)[-1].strip()


def located(path, number):
    path = os.path.realpath(path)
    if not path.startswith(source + os.sep) or not number.isdigit():
        return ''
    try:
        with open(path, encoding='utf-8', errors='replace') as handle:
            for index, line in enumerate(handle, start=1):
                if index == int(number):
                    return line
    except OSError:
        return ''
    return ''


with open(transcript, encoding='utf-8', errors='replace') as handle:
    lines = [strip(line) for line in handle]

failures = []
current = None
section = None
for line in lines:
    if line == '* Where:':
        current = {'where': '', 'wrong': []}
        failures.append(current)
        section = 'where'
        continue
    if line.startswith('* What went wrong:'):
        if current is None or current['wrong']:
            current = {'where': '', 'wrong': []}
            failures.append(current)
        section = 'wrong'
        continue
    if line.startswith('* '):
        section = None
        current = None
        continue
    if section == 'where' and line.startswith(prefix) and separator in line:
        path, _, number = line[len(prefix):].partition(separator)
        current['where'] = located(path, number.strip())
    elif section == 'wrong' and line:
        current['wrong'].append(line)

evidence = []
for failure in failures:
    wrong = failure['wrong']
    if wrong and wrong[0].startswith('Script compilation error'):
        continue
    evidence.append(failure['where'])
    evidence.extend(line for line in wrong if not line.startswith(headers))

text = '\n'.join(evidence).lower()
sys.exit(0 if any(marker in text for marker in markers) else 1)
PYTHON
}

run_lane() {
  local transcript status=0
  temporary_file transcript "${TMPDIR:-/tmp}" transcript
  info "build: $DISTRIBUTION_BUILD_COMMAND"
  (cd "$DISTRIBUTION_HOME" \
    && BUNDLE_GEMFILE=$DISTRIBUTION_HOME/Gemfile FASTLANE_SKIP_UPDATE_CHECK=1 FASTLANE_HIDE_CHANGELOG=1 FASTLANE_OPT_OUT_USAGE=1 FASTLANE_DISABLE_COLORS=1 \
       bundle exec fastlane android build) 2>&1 \
    | tee /dev/fd/2 \
    | tail -c "$transcript_bytes" > "$transcript" \
    || status=$?
  if (( status == 0 )); then
    return 0
  fi
  if keystore_required "$transcript"; then
    fail 4 'build_keystore_required: your build script requires a keystore file; guard it or remove the signing config'
  fi
  failed "$status" build
}

locate_bundle() {
  python3 -I - "$1" "$DISTRIBUTION_SOURCE" "$DISTRIBUTION_OUTPUT_PATH" <<'PYTHON'
import glob
import os
import sys

root, source, pattern = sys.argv[1], os.path.realpath(sys.argv[2]), sys.argv[3]
matches = sorted(
    path for path in (os.path.realpath(os.path.join(root, match)) for match in glob.glob(pattern, root_dir=root, recursive=True))
    if os.path.isfile(path) and path.startswith(source + os.sep)
)
if len(matches) != 1:
    print(f'[distribution] expected one bundle at {pattern}, found {len(matches)}', file=sys.stderr)
    sys.exit(2)
print(matches[0])
PYTHON
}

assert_unsigned() {
  local status=0
  python3 -I - "$1" <<'PYTHON' || status=$?
import sys
import zipfile

extensions = ('.SF', '.RSA', '.DSA', '.EC')
try:
    with zipfile.ZipFile(sys.argv[1]) as bundle:
        names = bundle.namelist()
except (OSError, zipfile.BadZipFile) as error:
    sys.exit(f'[distribution] the bundle is not a readable zip: {error}')
signed = [
    name for name in names
    if name.upper().startswith('META-INF/') and name.count('/') == 1 and name.upper().endswith(extensions)
]
sys.exit(3 if signed else 0)
PYTHON
  case $status in
    0) ;;
    3) fail 3 'build_already_signed: the build produced a signed bundle; the build job must produce an unsigned bundle' ;;
    *) fail 2 'build: the bundle could not be read' ;;
  esac
}

manifest() {
  bundletool dump manifest --bundle "$1" --xpath "$2" || fail 2 'build: the bundle has no readable manifest'
}

assert_identity() {
  local identifier=$1 code=$2
  if [[ -n ${DISTRIBUTION_IDENTIFIER:-} && $identifier != "$DISTRIBUTION_IDENTIFIER" ]]; then
    fail 2 "build: the bundle package is $identifier, expected $DISTRIBUTION_IDENTIFIER"
  fi
  if [[ -n ${DISTRIBUTION_VERSION_CODE:-} && $code != "$DISTRIBUTION_VERSION_CODE" ]]; then
    fail 2 "build: the bundle versionCode is $code, expected $DISTRIBUTION_VERSION_CODE"
  fi
}

publish() {
  local bundle=$1 output mapping result copy candidate identifier name code flutter=''
  mkdir -p "$DISTRIBUTION_OUTPUT_DIR" "$workspace/output"

  temporary_file output "$DISTRIBUTION_OUTPUT_DIR" app-unsigned.aab
  cp -- "$bundle" "$output"
  assert_unsigned "$output"
  identifier=$(manifest "$output" /manifest/@package)
  name=$(manifest "$output" /manifest/@android:versionName)
  code=$(manifest "$output" /manifest/@android:versionCode)
  assert_identity "$identifier" "$code"

  temporary_file mapping "$DISTRIBUTION_OUTPUT_DIR" mapping.txt
  candidate=$(dirname "$(dirname "$(dirname "$bundle")")")/mapping/$(basename "$(dirname "$bundle")")/mapping.txt
  if [[ -f $candidate ]]; then
    cp -- "$candidate" "$mapping"
  fi

  if [[ ${DISTRIBUTION_FRAMEWORK:-} == flutter ]]; then
    flutter=$flutter_root/bin/cache/flutter.version.json
  fi
  temporary_file result "$workspace/output" result.json
  python3 -I - "$result" "$identifier" "$name" "$code" "$DISTRIBUTION_IMAGE" "$fastlane_version" "$JAVA_HOME/release" "$gradle_version" "$flutter" <<'PYTHON'
import json
import sys

path, identifier, name, code, image, fastlane, release, gradle, flutter = sys.argv[1:]

if not code.isdigit():
    print(f'[distribution] build: versionCode {code!r} is not an integer', file=sys.stderr)
    sys.exit(2)

properties = {}
with open(release, encoding='utf-8') as handle:
    for line in handle:
        key, _, value = line.strip().partition('=')
        properties[key] = value.strip('"')
java = properties.get('JAVA_RUNTIME_VERSION') or properties.get('JAVA_VERSION')

framework = None
if flutter:
    try:
        with open(flutter, encoding='utf-8') as handle:
            framework = json.load(handle).get('frameworkVersion')
    except (OSError, ValueError):
        framework = None

result = {
    'artifact': 'app-unsigned.aab',
    'type': 'aab',
    'identifier': identifier,
    'versionName': name,
    'versionCode': int(code),
    'toolchain': {
        'image': image,
        'fastlane': fastlane,
        'java': java,
        'gradle': gradle or None,
        'flutter': framework or None,
    },
}
with open(path, 'w', encoding='utf-8') as handle:
    json.dump(result, handle)
    handle.write('\n')
print(json.dumps(result))
PYTHON

  commit "$mapping" "$DISTRIBUTION_OUTPUT_DIR/mapping.txt"
  if [[ $(realpath -- "$DISTRIBUTION_OUTPUT_DIR") != "$(realpath -- "$workspace/output")" ]]; then
    temporary_file copy "$DISTRIBUTION_OUTPUT_DIR" result.json
    cp -- "$result" "$copy"
    commit "$copy" "$DISTRIBUTION_OUTPUT_DIR/result.json"
  fi
  commit "$result" "$workspace/output/result.json"
  commit "$output" "$DISTRIBUTION_OUTPUT_DIR/app-unsigned.aab"
}

stop_daemons() {
  if [[ -n $gradle_project && -x $gradle_project/gradlew ]]; then
    (cd "$gradle_project" && timeout -k 5 "$stop_seconds" ./gradlew --stop --quiet) > /dev/null 2>&1 || true
  fi
}

save_cache() {
  local artifact directory image path roots=()
  artifact=$(cache_artifact)
  if [[ -z $artifact ]]; then
    return 0
  fi
  stop_daemons
  for path in gradle/caches gradle/wrapper pub npm; do
    if [[ -e $home/$path ]]; then
      roots+=("$path")
    fi
  done
  if (( ${#roots[@]} == 0 )); then
    info 'cache: nothing to save'
    return 0
  fi
  directory=$(dirname "$artifact")
  mkdir -p "$directory" || return 1
  temporary_file image "$directory" "$(basename "$artifact")" || return 1
  info "cache: saving ${roots[*]}"
  (cd "$home" \
    && find "${roots[@]}" \( -name '*.lock' -o -name '*.jks' -o -name '*.keystore' -o -name '*.p12' -o -name '*.pfx' \) -prune -o -print0 \
    | mksquashfs - "$image" -cpiostyle0 -noappend -no-xattrs -no-progress -quiet -comp zstd -Xcompression-level 3) || return 1
  commit "$image" "$artifact" || return 1
  info "cache: saved $artifact"
}

main() {
  local root bundle
  trap cleanup EXIT
  require DISTRIBUTION_BUILD_COMMAND DISTRIBUTION_OUTPUT_PATH DISTRIBUTION_IMAGE
  sweep_temporary
  restore_cache
  root=$(resolve_root)
  install_dependencies "$root" || failed $? install
  configure_gradle "$root"
  run_lane
  bundle=$(locate_bundle "$root")
  publish "$bundle"
  save_cache || info 'cache: save failed, the build is unaffected'
}

terminate() {
  trap '' TERM INT
  printf '[distribution] build: terminated by signal\n' >&2
  if [[ -n $job ]]; then
    kill -TERM -- "-$job" 2> /dev/null || true
  fi
  exit "$1"
}

job=''
set -m
trap 'terminate 143' TERM
trap 'terminate 130' INT
(main "$@" 2>&1 | stdbuf -oL fold -b -w "$width") < /dev/null &
job=$!
wait "$job"
