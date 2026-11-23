#!/bin/bash
set -euo pipefail

export DISTRIBUTION_WORK=${DISTRIBUTION_WORK:-$HOME/distribution}
if [[ ! -f $DISTRIBUTION_WORK/input/environment ]]; then
  printf '[distribution] %s not found\n' "$DISTRIBUTION_WORK/input/environment" >&2
  exit 2
fi
# shellcheck source=/dev/null
. "$DISTRIBUTION_WORK/input/environment"
IFS=$' \t\n'
unset CDPATH

readonly home=/usr/local/distribution
readonly work=$DISTRIBUTION_WORK
readonly input=$work/input
readonly signing=$input/signing
readonly output=$work/output
readonly build=$work/build
readonly archive=$build/app.xcarchive
readonly keychain=$work/signing.keychain-db
readonly options=$build/exportOptions.plist
profile_directories=(
  "$HOME/Library/MobileDevice/Provisioning Profiles"
  "$HOME/Library/Developer/Xcode/UserData/Provisioning Profiles"
)

export DISTRIBUTION_HOME=$home
export DISTRIBUTION_SOURCE=$work/source
export DISTRIBUTION_ROOT_DIRECTORY=${DISTRIBUTION_ROOT_DIRECTORY:-.}
export CI=true
export LANG=${LANG:-en_US.UTF-8}
export LC_ALL=${LC_ALL:-en_US.UTF-8}
export GEM_HOME=$HOME/.gem
export npm_config_prefix=$HOME/.npm-global
export PATH=$GEM_HOME/bin:$npm_config_prefix/bin:$home/ruby/bin:$home/node/bin:$home/flutter/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin

keychain_password=''
identity_hash=''
identity_name=''
package=''
search_list=()
search_list_saved=false
installed_profiles=()

info() {
  printf '[distribution] %s\n' "$*"
}

fail() {
  local code=$1
  shift
  printf '[distribution] %s\n' "$*" >&2
  exit "$code"
}

setting() {
  local key value
  while IFS='=' read -r key value; do
    if [[ $key == "$1" ]]; then
      printf '%s' "$value"
      return 0
    fi
  done < "$home/versions.env"
}

cleanup() {
  local status=$? path directory
  set +e
  if [[ $search_list_saved == true ]]; then
    security list-keychains -d user -s ${search_list[@]+"${search_list[@]}"}
  fi
  if [[ -e $keychain ]]; then
    security delete-keychain "$keychain"
  fi
  for path in ${installed_profiles[@]+"${installed_profiles[@]}"}; do
    rm -f -- "$path"
  done
  for directory in "${profile_directories[@]}"; do
    rmdir -- "$directory" 2> /dev/null
  done
  rm -rf -- "$signing" "$build/profiles" "$work/legacy.pem" "$work/legacy.p12"
  exit "$status"
}

prepare() {
  local name
  if [[ -z ${DISTRIBUTION_TEAM_ID:-} ]]; then
    fail 2 'DISTRIBUTION_TEAM_ID is required'
  fi
  for name in source.tar.gz signing/certificate.p12 signing/password; do
    if [[ ! -f $input/$name ]]; then
      fail 2 "input/$name not found"
    fi
  done
  if [[ ! -d $signing/profiles ]]; then
    fail 2 'input/signing/profiles not found'
  fi
  rm -rf -- "$output" "$build" "$DISTRIBUTION_SOURCE"
  mkdir -p -- "$output" "$build" "$DISTRIBUTION_SOURCE"
}

unpack_source() {
  info 'source: unpacking'
  tar -xzf "$input/source.tar.gz" -C "$DISTRIBUTION_SOURCE" --strip-components 1 || fail 2 'source: the archive could not be unpacked'
  rm -f -- "$input/source.tar.gz"
}

resolve_root() {
  local source root
  source=$(cd -- "$DISTRIBUTION_SOURCE" && pwd -P)
  root=$(cd -- "$source" && cd -- "$DISTRIBUTION_ROOT_DIRECTORY" 2> /dev/null && pwd -P) \
    || fail 2 "root directory $DISTRIBUTION_ROOT_DIRECTORY not found"
  if [[ $root != "$source" && $root != "$source"/* ]]; then
    fail 2 'root directory escapes the checkout'
  fi
  printf '%s' "$root"
}

save_search_list() {
  local line
  while IFS= read -r line; do
    line=${line#"${line%%[![:space:]]*}"}
    line=${line#\"}
    line=${line%\"}
    if [[ -n $line ]]; then
      search_list+=("$line")
    fi
  done < <(security list-keychains -d user)
  search_list_saved=true
}

import_identity() {
  local password=$1 status=0
  if security import "$signing/certificate.p12" -k "$keychain" -f pkcs12 -P "$password" -T /usr/bin/codesign > /dev/null; then
    return 0
  fi
  info 'keychain: import refused, retrying with legacy PKCS#12 encryption'
  (
    umask 077
    export DISTRIBUTION_IMPORT_PASSWORD=$password
    /usr/bin/openssl pkcs12 -in "$signing/certificate.p12" -passin env:DISTRIBUTION_IMPORT_PASSWORD -nodes -out "$work/legacy.pem" \
      && /usr/bin/openssl pkcs12 -export -in "$work/legacy.pem" -keypbe PBE-SHA1-3DES -certpbe PBE-SHA1-3DES \
        -out "$work/legacy.p12" -passout env:DISTRIBUTION_IMPORT_PASSWORD
  ) 2> /dev/null || status=$?
  rm -f -- "$work/legacy.pem"
  if ((status != 0)); then
    return "$status"
  fi
  security import "$work/legacy.p12" -k "$keychain" -f pkcs12 -P "$password" -T /usr/bin/codesign > /dev/null
}

select_identity() {
  local index hash name count=0
  while read -r index hash name; do
    if [[ $index != *')' || ${#hash} -ne 40 ]]; then
      continue
    fi
    name=${name#\"}
    identity_hash=$hash
    identity_name=${name%\"}
    count=$((count + 1))
  done < <(security find-identity -v -p codesigning "$keychain")
  if ((count != 1)); then
    fail 4 "keychain: expected one valid signing identity, found $count"
  fi
  if [[ $identity_name != *"($DISTRIBUTION_TEAM_ID)" ]]; then
    fail 4 "keychain: the signing identity does not belong to team $DISTRIBUTION_TEAM_ID"
  fi
  info "keychain: signing as $identity_name"
}

create_keychain() {
  local password
  password=$(< "$signing/password")
  rm -f -- "$signing/password"
  keychain_password=$(/usr/bin/openssl rand -hex 32)
  save_search_list
  security create-keychain -p "$keychain_password" "$keychain"
  security set-keychain-settings -lut 21600 "$keychain"
  security unlock-keychain -p "$keychain_password" "$keychain"
  security list-keychains -d user -s "$keychain" ${search_list[@]+"${search_list[@]}"}
  if ! import_identity "$password"; then
    fail 4 'keychain: the signing certificate could not be imported'
  fi
  password=''
  rm -f -- "$signing/certificate.p12" "$work/legacy.p12"
  security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$keychain_password" "$keychain" > /dev/null
  select_identity
}

install_profiles() {
  local path name pairs=() bundle uuid directory
  mkdir -p -- "$build/profiles"
  for path in "$signing/profiles"/*.mobileprovision; do
    if [[ ! -f $path ]]; then
      continue
    fi
    name=$(basename -- "$path" .mobileprovision)
    security cms -D -k "$keychain" -i "$path" -o "$build/profiles/$name.plist" > /dev/null || fail 4 "profile $name could not be decoded"
    pairs+=("$name" "$build/profiles/$name.plist")
  done
  if ((${#pairs[@]} == 0)); then
    fail 2 'input/signing/profiles holds no provisioning profile'
  fi
  python3 -I - "$identity_hash" "$DISTRIBUTION_TEAM_ID" "$options" "${pairs[@]}" > "$build/profiles/mapping" << 'PYTHON'
import datetime
import hashlib
import plistlib
import sys

identity, team, path, *pairs = sys.argv[1:]
characters = set('abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-')
now = datetime.datetime.now(datetime.timezone.utc).replace(tzinfo=None)


def refuse(bundle, reason):
    print(f'[distribution] profile {bundle}: {reason}', file=sys.stderr)
    sys.exit(4)


def covers(application, prefix, bundle):
    if application == f'{prefix}.{bundle}':
        return True
    return application.endswith('*') and f'{prefix}.{bundle}'.startswith(application[:-1])


profiles = {}
for bundle, source in zip(pairs[0::2], pairs[1::2]):
    if not bundle or set(bundle) - characters:
        refuse(bundle, 'the file name is not a bundle identifier')
    with open(source, 'rb') as handle:
        profile = plistlib.load(handle)
    uuid = profile.get('UUID', '')
    if len(uuid) != 36 or set(uuid.lower()) - set('0123456789abcdef-'):
        refuse(bundle, 'missing or invalid UUID')
    if team not in profile.get('TeamIdentifier', []):
        refuse(bundle, f'does not belong to team {team}')
    prefixes = profile.get('ApplicationIdentifierPrefix') or [team]
    application = profile.get('Entitlements', {}).get('application-identifier', '')
    if not any(covers(application, prefix, bundle) for prefix in prefixes):
        refuse(bundle, f'application identifier {application} does not cover {bundle}')
    if 'ProvisionedDevices' in profile or profile.get('ProvisionsAllDevices'):
        refuse(bundle, 'not an App Store distribution profile')
    expiry = profile.get('ExpirationDate')
    if not isinstance(expiry, datetime.datetime) or expiry <= now:
        refuse(bundle, 'expired')
    certificates = {hashlib.sha1(certificate).hexdigest() for certificate in profile.get('DeveloperCertificates', [])}
    if identity.lower() not in certificates:
        refuse(bundle, 'does not contain the signing certificate')
    profiles[bundle] = uuid

options = {
    'method': 'app-store-connect',
    'destination': 'export',
    'signingStyle': 'manual',
    'teamID': team,
    'signingCertificate': identity,
    'provisioningProfiles': profiles,
    'manageAppVersionAndBuildNumber': False,
    'uploadSymbols': True,
    'stripSwiftSymbols': True,
}
with open(path, 'wb') as handle:
    plistlib.dump(options, handle)
for bundle, uuid in profiles.items():
    print(f'{bundle}\t{uuid}')
PYTHON
  for directory in "${profile_directories[@]}"; do
    mkdir -p -- "$directory"
  done
  while IFS=$'\t' read -r bundle uuid; do
    for directory in "${profile_directories[@]}"; do
      cp -- "$signing/profiles/$bundle.mobileprovision" "$directory/$uuid.mobileprovision"
      installed_profiles+=("$directory/$uuid.mobileprovision")
    done
    info "profile: $bundle -> $uuid"
  done < "$build/profiles/mapping"
  rm -rf -- "$signing" "$build/profiles"
}

install_dependencies() {
  if [[ -z ${DISTRIBUTION_INSTALL_COMMAND:-} ]]; then
    return 0
  fi
  info "install: $DISTRIBUTION_INSTALL_COMMAND"
  (cd -- "$1" && /bin/sh -c "$DISTRIBUTION_INSTALL_COMMAND") || fail 1 'install: the install command failed'
}

run_lane() {
  local name
  security unlock-keychain -p "$keychain_password" "$keychain"
  info "build: ${DISTRIBUTION_BUILD_COMMAND:-no build command}, then archive"
  (
    for name in $(compgen -e); do
      case $name in
        BUNDLE_* | GYM_* | FL_* | XCPRETTY_* | DISABLE_XCPRETTY | SUPPRESS_OUTPUT | RUBYOPT | RUBYLIB) unset "$name" ;;
      esac
    done
    cd -- "$home"
    export BUNDLE_GEMFILE=$home/Gemfile
    export FASTLANE_SKIP_UPDATE_CHECK=1 FASTLANE_OPT_OUT_USAGE=1 FASTLANE_HIDE_CHANGELOG=1 FASTLANE_DISABLE_COLORS=1
    export FL_REPORT_PATH=$build DISTRIBUTION_ARCHIVE_PATH=$archive DISTRIBUTION_SIGNING_IDENTITY=$identity_name
    exec bundle exec fastlane ios build
  ) || fail 1 'build: the fastlane lane failed'
  if [[ ! -d $archive ]]; then
    fail 1 "build: no archive at $archive"
  fi
}

export_archive() {
  local packages
  info 'export: app-store-connect'
  xcodebuild -exportArchive -archivePath "$archive" -exportPath "$build/export" -exportOptionsPlist "$options" \
    || fail 5 'export: xcodebuild -exportArchive failed'
  packages=("$build"/export/*.ipa)
  if ((${#packages[@]} != 1)) || [[ ! -f ${packages[0]} ]]; then
    fail 5 'export: expected exactly one .ipa'
  fi
  package=${packages[0]}
}

flutter_version() {
  if [[ ${DISTRIBUTION_FRAMEWORK:-} != flutter ]]; then
    return 0
  fi
  flutter --version --machine 2> /dev/null \
    | python3 -I -c 'import json, sys; print(json.load(sys.stdin)["frameworkVersion"])' 2> /dev/null \
    || true
}

place() {
  mv -f -- "$1" "$output/$2"
}

publish() {
  local xcode flutter
  xcode=$(xcodebuild -version)
  xcode=${xcode%%$'\n'*}
  flutter=$(flutter_version)
  python3 -I - "$package" "$build/result.json" "${DISTRIBUTION_IDENTIFIER:-}" "${DISTRIBUTION_VERSION_NAME:-}" \
    "${DISTRIBUTION_VERSION_CODE:-}" "$(setting IMAGE)" "$(setting FASTLANE_VERSION)" "${xcode#Xcode }" "$flutter" << 'PYTHON'
import json
import plistlib
import sys
import zipfile

package, path, identifier, name, code, image, fastlane, xcode, flutter = sys.argv[1:]

with zipfile.ZipFile(package) as bundle:
    plists = [
        entry for entry in bundle.namelist()
        if entry.startswith('Payload/') and entry.count('/') == 2 and entry.endswith('.app/Info.plist')
    ]
    if len(plists) != 1:
        sys.exit(f'[distribution] expected one application in the package, found {len(plists)}')
    info = plistlib.loads(bundle.read(plists[0]))

built = {
    'identifier': info.get('CFBundleIdentifier', ''),
    'versionName': info.get('CFBundleShortVersionString', ''),
    'versionCode': info.get('CFBundleVersion', ''),
}
if identifier and built['identifier'] != identifier:
    print(f'[distribution] built bundle identifier {built["identifier"]!r}, expected {identifier!r}', file=sys.stderr)
    sys.exit(6)
mismatches = [
    f'{key} is {built[key]!r}, expected {expected!r}'
    for key, expected in (('versionName', name), ('versionCode', code))
    if expected and built[key] != expected
]
if mismatches:
    print(f'[distribution] the version did not take: {"; ".join(mismatches)}', file=sys.stderr)
    sys.exit(3)

result = {
    'artifact': 'app.ipa',
    'type': 'ipa',
    **built,
    'toolchain': {
        'image': image or None,
        'fastlane': fastlane or None,
        'xcode': xcode or None,
        'flutter': flutter or None,
    },
}
with open(path, 'w', encoding='utf-8') as handle:
    json.dump(result, handle)
    handle.write('\n')
print(json.dumps(result))
PYTHON
  mkdir -p -- "$archive/dSYMs"
  ditto -c -k --norsrc --noextattr --noacl --keepParent "$archive/dSYMs" "$build/dsyms.zip"
  place "$package" app.ipa
  place "$build/dsyms.zip" dsyms.zip
  place "$build/result.json" result.json
}

main() {
  local root
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  prepare
  unpack_source
  root=$(resolve_root)
  create_keychain
  install_profiles
  security lock-keychain "$keychain"
  if [[ ${DISTRIBUTION_FRAMEWORK:-} == flutter ]]; then
    if [[ -n ${DISTRIBUTION_VERSION_NAME:-} ]]; then
      export FLUTTER_BUILD_NAME=$DISTRIBUTION_VERSION_NAME
    fi
    if [[ -n ${DISTRIBUTION_VERSION_CODE:-} ]]; then
      export FLUTTER_BUILD_NUMBER=$DISTRIBUTION_VERSION_CODE
    fi
  fi
  install_dependencies "$root"
  run_lane
  export_archive
  publish
}

main "$@"
