#!/bin/bash
# Installed as /usr/local/distribution/host-key.sh and run as root at boot by io.appwrite.distribution.host-key.
set -euo pipefail

readonly automount='/Volumes/My Shared Files/host-key'
readonly mountpoint=/private/var/run/distribution-host-key
readonly device=com.apple.virtio-fs.automount
readonly key=/etc/ssh/ssh_host_ed25519_key
readonly wait=300

source_directory() {
  local attempt
  for ((attempt = 0; attempt < wait; attempt++)); do
    if [[ -f $automount/ssh_host_ed25519_key ]]; then
      printf '%s' "$automount"
      return 0
    fi
    if ((attempt == 30)) && [[ ! -d $mountpoint ]]; then
      /bin/mkdir -m 0700 "$mountpoint"
      /sbin/mount_virtiofs -o rdonly "$device" "$mountpoint" || true
    fi
    if [[ -f $mountpoint/host-key/ssh_host_ed25519_key ]]; then
      printf '%s' "$mountpoint/host-key"
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
  /usr/bin/install -o root -g wheel -m 0600 "$directory/ssh_host_ed25519_key" "$key.new"
  /usr/bin/install -o root -g wheel -m 0644 "$directory/ssh_host_ed25519_key.pub" "$key.pub.new"
  /bin/mv -f "$key.new" "$key"
  /bin/mv -f "$key.pub.new" "$key.pub"
  printf '[host-key] installed the host key the agent minted for this machine\n'
}

main "$@"
