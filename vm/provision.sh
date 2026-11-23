#!/bin/bash
set -euo pipefail

readonly home=/usr/local/distribution
repository=$(cd -- "$(dirname -- "$0")/.." && pwd -P)
readonly repository
user=$(id -un)
readonly user
scratch=''

# shellcheck source=/dev/null
. "$repository/vm/versions.env"

unset GEM_HOME GEM_PATH RUBYOPT RUBYLIB
export PATH=$home/ruby/bin:$home/node/bin:$home/flutter/bin:/usr/bin:/bin:/usr/sbin:/sbin
export FASTLANE_SKIP_UPDATE_CHECK=1 FASTLANE_OPT_OUT_USAGE=1 FASTLANE_HIDE_CHANGELOG=1

info() {
  printf '[provision] %s\n' "$*"
}

fail() {
  printf '[provision] %s\n' "$*" >&2
  exit 1
}

cleanup() {
  if [[ -n $scratch ]]; then
    rm -rf -- "$scratch"
  fi
}

download() {
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --retry 3 --output "$3" "$1"
  if [[ $(shasum -a 256 "$3" | cut -d ' ' -f 1) != "$2" ]]; then
    fail "checksum mismatch for $1"
  fi
}

check_labels() {
  local expected
  expected=$(printf '%s\n' "macos-$MACOS_VERSION" "xcode-$XCODE_VERSION" "flutter-$FLUTTER_VERSION" "node-$NODE_VERSION" "cocoapods-$COCOAPODS_VERSION")
  if [[ $(< "$repository/vm/labels") != "$expected" ]]; then
    fail 'vm/labels does not match vm/versions.env'
  fi
}

check_guest() {
  local version tool
  if (($(id -u) == 0)); then
    fail 'run as the guest user, not root'
  fi
  if [[ $(uname -m) != arm64 ]]; then
    fail 'the golden image is arm64 only'
  fi
  version=$(sw_vers -productVersion)
  if [[ $version != "$MACOS_VERSION" ]]; then
    fail "macOS $version, expected $MACOS_VERSION"
  fi
  if [[ ! -s $HOME/.ssh/authorized_keys ]]; then
    fail 'add the agent public key to ~/.ssh/authorized_keys first: password login is turned off below'
  fi
  for tool in /bin/ps /bin/sh /usr/bin/scp /usr/libexec/sftp-server /usr/bin/git /usr/bin/python3 /usr/bin/ditto; do
    if [[ ! -x $tool ]]; then
      fail "$tool is missing"
    fi
  done
}

check_xcode() {
  local expected actual
  expected=$(printf 'Xcode %s\nBuild version %s' "$XCODE_VERSION" "$XCODE_BUILD")
  actual=$(xcodebuild -version 2> /dev/null) || fail 'Xcode is not installed or not selected: see vm/README.md'
  if [[ $actual != "$expected" ]]; then
    fail "found $actual, expected $expected"
  fi
  sudo xcodebuild -license accept
  sudo xcodebuild -runFirstLaunch
  xcrun --sdk iphoneos --show-sdk-version > /dev/null || fail 'the iOS platform is missing: run xcodebuild -downloadPlatform iOS'
  python3 -I -c 'import datetime, hashlib, json, plistlib, zipfile' || fail 'python3 from the Xcode command line tools does not run'
}

install_files() {
  info "files: $home"
  sudo install -d -o root -g wheel -m 0755 "$home" "$home/fastlane"
  sudo install -o root -g wheel -m 0644 "$repository/lanes/Fastfile" "$home/fastlane/Fastfile"
  sudo install -o root -g wheel -m 0644 "$repository/lanes/Gemfile" "$repository/lanes/Gemfile.lock" \
    "$repository/vm/versions.env" "$repository/vm/labels" "$home/"
  sudo install -o root -g wheel -m 0755 "$repository/scripts/build-ios.sh" "$home/build.sh"
  sudo install -o root -g wheel -m 0755 "$repository/vm/host-key.sh" "$home/host-key.sh"
  sudo install -d -o root -g wheel -m 0755 "$home/cocoapods"
  sudo install -o root -g wheel -m 0644 "$repository/vm/cocoapods/Gemfile" "$repository/vm/cocoapods/Gemfile.lock" "$home/cocoapods/"
  sudo install -d -o "$user" -g staff -m 0755 "$home/ruby" "$home/vendor" "$home/.bundle" "$home/flutter" "$home/cocoapods/.bundle"
}

install_ruby() {
  local version
  info "ruby: $RUBY_VERSION"
  download "$LIBYAML_URL" "$LIBYAML_SHA256" "$scratch/yaml.tar.gz"
  mkdir -p -- "$scratch/yaml"
  tar -xzf "$scratch/yaml.tar.gz" -C "$scratch/yaml" --strip-components 1
  (cd -- "$scratch/yaml" && ./configure --quiet --prefix="$home/ruby" && make -j "$(sysctl -n hw.ncpu)" && make install) > /dev/null
  git -c advice.detachedHead=false clone --quiet --depth 1 --branch "$RUBY_BUILD_TAG" https://github.com/rbenv/ruby-build.git "$scratch/ruby-build"
  if [[ $(git -C "$scratch/ruby-build" rev-parse HEAD) != "$RUBY_BUILD_COMMIT" ]]; then
    fail "ruby-build $RUBY_BUILD_TAG is not commit $RUBY_BUILD_COMMIT"
  fi
  RUBY_CONFIGURE_OPTS="--with-libyaml-dir=$home/ruby --disable-install-doc" \
    "$scratch/ruby-build/bin/ruby-build" "$RUBY_VERSION" "$home/ruby"
  version=$("$home/ruby/bin/ruby" -ropenssl -rpsych -e 'print RUBY_VERSION')
  if [[ $version != "$RUBY_VERSION" ]]; then
    fail "ruby $version, expected $RUBY_VERSION"
  fi
}

locked_bundler() {
  local line found=false
  while IFS= read -r line; do
    if [[ $found == true ]]; then
      printf '%s' "${line//[[:space:]]/}"
      return 0
    fi
    if [[ $line == 'BUNDLED WITH' ]]; then
      found=true
    fi
  done < "$home/Gemfile.lock"
  fail 'Gemfile.lock has no BUNDLED WITH section'
}

install_gems() {
  local bundler version
  bundler=$(locked_bundler)
  info "gems: bundler $bundler, cocoapods $COCOAPODS_VERSION, fastlane $FASTLANE_VERSION"
  gem install --no-document bundler --version "$bundler"
  (
    cd -- "$home/cocoapods"
    export BUNDLE_GEMFILE=$home/cocoapods/Gemfile
    bundle config set --local frozen true
    bundle config set --local path.system true
    bundle install --jobs "$(sysctl -n hw.ncpu)"
  )
  (
    cd -- "$home"
    BUNDLE_GEMFILE=$home/Gemfile bundle config set --local deployment true
    BUNDLE_GEMFILE=$home/Gemfile bundle install --jobs "$(sysctl -n hw.ncpu)"
  )
  version=$(cd -- "$home" && BUNDLE_GEMFILE=$home/Gemfile bundle exec fastlane --version)
  if [[ $version != *"fastlane $FASTLANE_VERSION"* ]]; then
    fail "fastlane $FASTLANE_VERSION is not what bundle exec runs"
  fi
  (cd -- "$home" && BUNDLE_GEMFILE=$home/Gemfile FL_REPORT_PATH=$scratch bundle exec fastlane lanes) > /dev/null \
    || fail 'fastlane could not load the Fastfile'
  version=$(pod --version)
  if [[ $version != "$COCOAPODS_VERSION" ]]; then
    fail "cocoapods $version, expected $COCOAPODS_VERSION"
  fi
}

install_node() {
  local version
  info "node: $NODE_VERSION"
  download "$NODE_URL" "$NODE_SHA256" "$scratch/node.tar.xz"
  sudo install -d -o root -g wheel -m 0755 "$home/node"
  sudo tar -xJf "$scratch/node.tar.xz" -C "$home/node" --strip-components 1 --no-same-owner
  version=$("$home/node/bin/node" --version)
  if [[ $version != "v$NODE_VERSION" ]]; then
    fail "node $version, expected $NODE_VERSION"
  fi
}

install_flutter() {
  local version
  info "flutter: $FLUTTER_VERSION"
  download "$FLUTTER_URL" "$FLUTTER_SHA256" "$scratch/flutter.zip"
  ditto -x -k "$scratch/flutter.zip" "$scratch/unpacked"
  ditto "$scratch/unpacked/flutter" "$home/flutter"
  flutter config --no-analytics > /dev/null
  dart --disable-analytics > /dev/null
  flutter precache --ios
  version=$(flutter --version --machine | python3 -I -c 'import json, sys; print(json.load(sys.stdin)["frameworkVersion"])')
  if [[ $version != "$FLUTTER_VERSION" ]]; then
    fail "flutter $version, expected $FLUTTER_VERSION"
  fi
}

install_intermediates() {
  local generation variable path fingerprint installed
  installed=$(security find-certificate -a -Z /Library/Keychains/System.keychain)
  for generation in $WWDR_CERTIFICATES; do
    variable=WWDR_${generation}_SHA256
    path=$scratch/AppleWWDRCA$generation.cer
    download "https://www.apple.com/certificateauthority/AppleWWDRCA$generation.cer" "${!variable}" "$path"
    fingerprint=$(shasum -a 1 "$path" | cut -d ' ' -f 1 | tr '[:lower:]' '[:upper:]')
    if [[ $installed == *"SHA-1 hash: $fingerprint"* ]]; then
      continue
    fi
    info "keychain: Apple WWDR intermediate $generation"
    sudo security add-certificates -k /Library/Keychains/System.keychain "$path"
  done
}

configure_system() {
  local effective key
  info 'system: automatic updates, Spotlight and ssh password login off'
  for key in AutomaticCheckEnabled AutomaticDownload AutomaticallyInstallMacOSUpdates CriticalUpdateInstall ConfigDataInstall; do
    sudo defaults write /Library/Preferences/com.apple.SoftwareUpdate "$key" -bool false
  done
  sudo defaults write /Library/Preferences/com.apple.commerce AutoUpdate -bool false
  sudo softwareupdate --schedule off > /dev/null 2>&1 || info 'softwareupdate --schedule is unavailable; the preferences above apply'
  sudo mdutil -a -i off > /dev/null
  printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\n' \
    | sudo tee /etc/ssh/sshd_config.d/100-distribution.conf > /dev/null
  effective=$(sudo /usr/sbin/sshd -T)
  for key in 'passwordauthentication no' 'kbdinteractiveauthentication no' 'subsystem sftp '; do
    if [[ $'\n'$effective != *$'\n'"$key"* ]]; then
      fail "sshd does not report '$key'"
    fi
  done
}

check_cocoapods_lock() {
  if ! grep -q "^    cocoapods ($COCOAPODS_VERSION)\$" "$repository/vm/cocoapods/Gemfile.lock"; then
    fail "vm/cocoapods/Gemfile.lock does not lock cocoapods $COCOAPODS_VERSION"
  fi
}

install_host_key_daemon() {
  info 'ssh: per-machine host keys'
  sudo install -o root -g wheel -m 0644 "$repository/vm/io.appwrite.distribution.host-key.plist" /Library/LaunchDaemons/
  sudo rm -f /etc/ssh/ssh_host_*
}

check_signing_state() {
  local directory
  if defaults read com.apple.dt.Xcode DVTDeveloperAccountManagerAppleIDLists > /dev/null 2>&1; then
    fail 'an Xcode account is signed in: remove it in Xcode > Settings > Accounts, then provision again'
  fi
  if [[ $(security find-identity -v -p codesigning) != *'0 valid identities found'* ]]; then
    fail 'a keychain on the search list holds a code signing identity'
  fi
  for directory in "$HOME/Library/MobileDevice/Provisioning Profiles" "$HOME/Library/Developer/Xcode/UserData/Provisioning Profiles"; do
    if [[ -d $directory && -n $(ls -A "$directory") ]]; then
      fail "$directory is not empty"
    fi
  done
}

# lock_account gives the guest user a random password nobody keeps and takes it out of admin, so build code cannot
# become root to read another machine's secrets or reconfigure the guest. The autologin password file is rewritten to
# match, and the login keychain, encrypted with the old password, is removed for macOS to recreate at the next login.
# This is the last step that needs sudo: afterwards the image has no admin account.
lock_account() {
  local password membership
  info "account: $user gets a random password and leaves the admin group"
  password=$(/usr/bin/openssl rand -hex 32)
  sudo /usr/bin/dscl . -passwd "/Users/$user" "$password"
  printf '%s' "$password" | /usr/bin/python3 -I -c '
import sys
key = bytes([0x7D, 0x89, 0x52, 0x23, 0xD2, 0xBC, 0xDD, 0xEA, 0xA3, 0xB9, 0x1F])
plain = sys.stdin.buffer.read()
plain += bytes(-len(plain) % 12)
sys.stdout.buffer.write(bytes(byte ^ key[index % len(key)] for index, byte in enumerate(plain)))
' | sudo /usr/bin/tee /etc/kcpassword > /dev/null
  password=''
  sudo /usr/sbin/chown root:wheel /etc/kcpassword
  sudo /bin/chmod 0600 /etc/kcpassword
  rm -f -- "$HOME/Library/Keychains/login.keychain-db"
  sudo /usr/sbin/dseditgroup -o edit -d "$user" -t user admin
  sudo -k
  membership=$(/usr/sbin/dseditgroup -o checkmember -m "$user" admin) || true
  if [[ $membership != *'NOT a member'* ]]; then
    fail "$user is still an admin: $membership"
  fi
}

finish() {
  sudo chown -R root:wheel "$home"
  sudo chown -R "$user:staff" "$home/flutter"
}

main() {
  trap cleanup EXIT
  check_labels
  check_cocoapods_lock
  check_guest
  check_xcode
  scratch=$(mktemp -d)
  sudo -v
  install_files
  install_ruby
  install_gems
  install_node
  install_flutter
  install_intermediates
  configure_system
  check_signing_state
  install_host_key_daemon
  finish
  lock_account
  info "image: $IMAGE"
  info "labels: $(paste -sd , "$home/labels")"
  info 'done: the image has no admin account and no ssh host key of its own; stop it now'
}

main "$@"
