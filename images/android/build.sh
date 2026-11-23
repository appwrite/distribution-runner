#!/usr/bin/env bash
set -euo pipefail
shopt -s inherit_errexit

readonly workspace=/mnt/code
readonly home=$workspace/.distribution
readonly width=16384
readonly transcript_bytes=1048576

export DISTRIBUTION_HOME=/usr/local/distribution
export DISTRIBUTION_SOURCE=${DISTRIBUTION_SOURCE:-$workspace/source}
export DISTRIBUTION_ROOT_DIRECTORY=${DISTRIBUTION_ROOT_DIRECTORY:-.}
export DISTRIBUTION_OUTPUT_DIR=${DISTRIBUTION_OUTPUT_DIR:-$workspace/output}
export GRADLE_USER_HOME=$home/gradle
export PUB_CACHE=$home/pub
export npm_config_cache=$home/npm
export ANDROID_USER_HOME=$home/android
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

cache_artifact() {
  local path=${DISTRIBUTION_CACHE_ARTIFACT:-}
  if [[ -n $path && $path != /* ]]; then
    path=$workspace/$path
  fi
  printf '%s' "$path"
}

require() {
  local name
  for name in "$@"; do
    if [[ -z ${!name:-} ]]; then
      fail 2 "$name is required"
    fi
  done
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
}

resolve_root() {
  local source root
  source=$(realpath -e -- "$DISTRIBUTION_SOURCE") || fail 2 "source directory $DISTRIBUTION_SOURCE not found"
  root=$(realpath -e -- "$source/$DISTRIBUTION_ROOT_DIRECTORY") || fail 2 "root directory $DISTRIBUTION_ROOT_DIRECTORY not found"
  if [[ $root != "$source" && $root != "$source"/* ]]; then
    fail 2 'root directory escapes the checkout'
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

detect_gradle() {
  local root=$1 project properties url version
  for project in "$root" "$root/android"; do
    properties=$project/gradle/wrapper/gradle-wrapper.properties
    if [[ ! -f $properties ]]; then
      continue
    fi
    url=$(grep -m 1 '^distributionUrl=' "$properties" || true)
    url=${url%$'\r'}
    version=${url##*/gradle-}
    version=${version%-*}
    gradle_project=$project
    if [[ $url == */gradle-* && $version == [0-9]*.* ]]; then
      gradle_version=$version
    fi
    return 0
  done
}

select_java() {
  local version=$1 major rest minor
  if [[ -z $version ]]; then
    printf '%s' "$JAVA_21_HOME"
    return 0
  fi
  major=${version%%.*}
  rest=${version#*.}
  minor=${rest%%[!0-9]*}
  if [[ -z $major || $major == *[!0-9]* || -z $minor ]]; then
    printf '%s' "$JAVA_21_HOME"
  elif (( major < 8 || (major == 8 && minor < 5) )); then
    printf '%s' "$JAVA_17_HOME"
  else
    printf '%s' "$JAVA_21_HOME"
  fi
}

heap_megabytes() {
  local limit=''
  if [[ -r /sys/fs/cgroup/memory.max ]]; then
    limit=$(< /sys/fs/cgroup/memory.max)
  elif [[ -r /sys/fs/cgroup/memory/memory.limit_in_bytes ]]; then
    limit=$(< /sys/fs/cgroup/memory/memory.limit_in_bytes)
  fi
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
  printf 'org.gradle.java.home=%s\norg.gradle.jvmargs=-Xmx%sm -XX:MaxMetaspaceSize=1g -Dfile.encoding=UTF-8\n' \
    "$java" "$heap" > "$GRADLE_USER_HOME/gradle.properties"
  cp "$DISTRIBUTION_HOME/distribution.init.gradle" "$GRADLE_USER_HOME/init.d/distribution.init.gradle"
  info "gradle: ${gradle_version:-unknown}, java: $java, heap: ${heap}m"
}

keystore_required() {
  python3 -I - "$1" "$DISTRIBUTION_SOURCE" <<'PYTHON'
import os
import sys

transcript, source = sys.argv[1], os.path.realpath(sys.argv[2])
markers = ('keystore', 'key.properties', 'storefile', 'storepassword', 'keyalias', 'keypassword', 'signingconfig', '.jks')
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

evidence = []
failure = False
for line in lines:
    if line.startswith('* What went wrong:'):
        failure = True
        continue
    if line.startswith('* '):
        failure = False
    if failure:
        evidence.append(line)
    if line.startswith(prefix) and separator in line:
        path, _, number = line[len(prefix):].partition(separator)
        evidence.append(located(path, number.strip()))

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
  fail "$status" "build: failed with exit status $status"
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
    if name.startswith('META-INF/') and name.count('/') == 1 and name.upper().endswith(extensions)
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
  bundletool dump manifest --bundle "$1" --xpath "$2"
}

flutter_version() {
  if [[ ${DISTRIBUTION_FRAMEWORK:-} != flutter ]]; then
    return 0
  fi
  flutter --version --machine 2>/dev/null \
    | python3 -I -c 'import json, sys; print(json.load(sys.stdin)["frameworkVersion"])' 2>/dev/null \
    || true
}

publish() {
  local bundle=$1 identifier name code flutter output mapping candidate result copy
  identifier=$(manifest "$bundle" /manifest/@package)
  name=$(manifest "$bundle" /manifest/@android:versionName)
  code=$(manifest "$bundle" /manifest/@android:versionCode)
  flutter=$(flutter_version)

  mkdir -p "$DISTRIBUTION_OUTPUT_DIR" "$workspace/output"

  temporary_file output "$DISTRIBUTION_OUTPUT_DIR" app-unsigned.aab
  cp -- "$bundle" "$output"
  commit "$output" "$DISTRIBUTION_OUTPUT_DIR/app-unsigned.aab"

  temporary_file mapping "$DISTRIBUTION_OUTPUT_DIR" mapping.txt
  candidate=$(dirname "$(dirname "$(dirname "$bundle")")")/mapping/$(basename "$(dirname "$bundle")")/mapping.txt
  if [[ -f $candidate ]]; then
    cp -- "$candidate" "$mapping"
  fi
  commit "$mapping" "$DISTRIBUTION_OUTPUT_DIR/mapping.txt"

  temporary_file result "$workspace/output" result.json
  python3 -I - "$result" "$identifier" "$name" "$code" "$DISTRIBUTION_IMAGE" "$FASTLANE_VERSION" "$JAVA_HOME/release" "$gradle_version" "$flutter" <<'PYTHON'
import json
import sys

path, identifier, name, code, image, fastlane, release, gradle, flutter = sys.argv[1:]

properties = {}
with open(release, encoding='utf-8') as handle:
    for line in handle:
        key, _, value = line.strip().partition('=')
        properties[key] = value.strip('"')
java = properties.get('JAVA_RUNTIME_VERSION') or properties.get('JAVA_VERSION')

if not code.isdigit():
    sys.exit(f'[distribution] versionCode {code!r} is not an integer')

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
        'flutter': flutter or None,
    },
}
with open(path, 'w', encoding='utf-8') as handle:
    json.dump(result, handle)
    handle.write('\n')
print(json.dumps(result))
PYTHON
  commit "$result" "$workspace/output/result.json"

  if [[ $(realpath -- "$DISTRIBUTION_OUTPUT_DIR") != "$(realpath -- "$workspace/output")" ]]; then
    temporary_file copy "$DISTRIBUTION_OUTPUT_DIR" result.json
    cp -- "$workspace/output/result.json" "$copy"
    commit "$copy" "$DISTRIBUTION_OUTPUT_DIR/result.json"
  fi
}

stop_daemons() {
  if [[ -n $gradle_project && -x $gradle_project/gradlew ]]; then
    (cd "$gradle_project" && ./gradlew --stop --quiet) > /dev/null 2>&1 || true
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
  require DISTRIBUTION_BUILD_COMMAND DISTRIBUTION_OUTPUT_PATH DISTRIBUTION_IMAGE FASTLANE_VERSION
  restore_cache
  root=$(resolve_root)
  install_dependencies "$root"
  configure_gradle "$root"
  run_lane
  bundle=$(locate_bundle "$root")
  assert_unsigned "$bundle"
  publish "$bundle"
  save_cache || info 'cache: save failed, the build is unaffected'
}

main "$@" 2>&1 | fold -b -w "$width"
