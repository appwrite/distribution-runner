#!/bin/bash
# Installed as /usr/local/distribution/host-key.sh and run as root at boot by io.appwrite.distribution.host-key.
set -euo pipefail

readonly automount='/Volumes/My Shared Files'
readonly mountpoint=/private/var/run/distribution-host-key
readonly device=com.apple.virtio-fs.automount
readonly share=host-key
readonly file=ssh_host_ed25519_key
readonly key=/etc/ssh/$file
readonly wait=300

# shared prints the directory holding the shared key under a mount: lume exposes a single --shared-dir either as the
# mount itself or as a directory named after it, so both layouts are accepted.
shared() {
  local directory
  for directory in "$1/$share" "$1"; do
    if [[ -f $directory/$file && ! -L $directory/$file ]]; then
      printf '%s' "$directory"
      return 0
    fi
  done
  return 1
}

source_directory() {
  local attempt
  for ((attempt = 0; attempt < wait; attempt++)); do
    if shared "$automount"; then
      return 0
    fi
    if ((attempt == 30)) && [[ ! -d $mountpoint ]]; then
      /bin/mkdir -m 0700 "$mountpoint"
      /sbin/mount_virtiofs -o rdonly "$device" "$mountpoint" || true
    fi
    if [[ -d $mountpoint ]] && shared "$mountpoint"; then
      return 0
    fi
    sleep 1
  done
  return 1
}

main() {
  local directory
  umask 077
  directory=$(source_directory) || {
    printf '[host-key] no host key was shared within %d seconds\n' "$wait" >&2
    exit 1
  }
  /usr/bin/install -o root -g wheel -m 0600 "$directory/$file" "$key.new"
  /usr/bin/install -o root -g wheel -m 0644 "$directory/$file.pub" "$key.pub.new"
  /bin/mv -f "$key.new" "$key"
  /bin/mv -f "$key.pub.new" "$key.pub"
  printf '[host-key] installed the host key the agent minted for this machine\n'
}

main "$@"
