# agent/vm

The VM layer behind RFC §9.1: `Create`, `Exec`, `Copy` and `Destroy` on a single-use machine cloned from a golden image, and `Sweep` for what a crashed agent left behind. The lease runner codes against the `Layer` interface; `Fake` and `Lume` implement it.

## Contract

| Call | Behaviour |
|---|---|
| `Create(ctx, name)` | Clones the golden image, boots it and waits for a working SSH login. Refuses a name that already exists (`ErrExists`); on any other failure, a failed or cancelled clone included, it deletes whatever it made and returns a zero `Machine`. Names are a letter or digit followed by letters, digits, `.`, `_` or `-` |
| `Exec(ctx, machine, command)` | Runs `Arguments` with `Environment` added to the guest login environment and returns the exit code. A non-zero exit is not an error. When `ctx` ends, the command's whole process group is terminated in the guest and `Exec` returns `-1` with the context's error. For `Lume`, `255` can also be ssh's own failure |
| `Copy(ctx, machine, transfer)` | `In` copies a host file or directory into the guest; `Destination` must not be an existing directory (`ErrDestination`) and modes are preserved, so a `0600` file stays `0600`. `Out` copies one regular guest file (`ErrNotFile` for a symlink, device or directory) of at most `Limit` bytes (`ErrLimit`) into a new `0600` host file that must not exist (`ErrDestination`); a refused copy leaves nothing behind. Parents are created; relative guest paths resolve against `Machine.Home` |
| `Destroy(ctx, machine)` | Stops and deletes the machine. Succeeds when it is already gone and on a zero `Machine`, so it can always be deferred |
| `Sweep(ctx, prefix)` | Destroys every machine named with `prefix` that this layer did not create, with its host files. The agent runs it once at start with its runner-scoped prefix. Lume never sweeps or destroys the golden image, and refuses a prefix the golden image's name starts with |

## Fake

Machines are temporary directories; guest paths map under `Machine.Address` and cannot leave it. `Build func(ctx context.Context, work string, environment map[string]string) error` stands in for `/usr/local/distribution/build.sh`: `work` is the host directory that `DISTRIBUTION_WORK` names, and `Exec` waits for `Build` to return after `ctx` ends, so a build never writes into a destroyed machine. Returning `*ExitError` chooses the exit code, any other error exits `1`, and a panic propagates out of `Exec`. `Fail func(Call) error` injects an error into any call, and `Calls()` returns every call in order.

## Lume

Unproven pending spike S7: no build has run through this adapter on a real Mac yet.

Licence: lume is MIT ("Copyright (c) 2025 Cua AI, Inc.", `LICENSE.md` of trycua/cua at tag `lume-v0.7.2`). It is chosen over Tart (FSL-1.1-ALv2 since 5 June 2026, Competing Use unresolved), Anka (EULA excludes service-bureau use) and Orka (terms not public) per RFC Appendix J. The agent only executes the `lume` binary; it does not redistribute it.

Commands, verified against the `lume-v0.7.2` sources (`libs/lume/src/Commands`). Every one runs with `LUME_TELEMETRY_ENABLED=false`, which overrides the persisted telemetry preference for that process:

| Step | Command |
|---|---|
| Clone | `lume ls --format json` first, refusing a name that exists, then `lume clone <golden> <name>`; refuses a running source, and gives the clone a fresh random MAC address and machine identifier. A failed or cancelled clone is deleted |
| Boot | `lume run <name> --display none --vnc disabled --shared-dir <per-machine>/host-key:ro`, kept running as a child of the agent; the VM lives in that process |
| Address | `lume get <name> --format json`, polled until `status` is `running`, `ipAddress` is set and `sshAvailable` is true, a failed poll retried until the boot timeout. The address must parse as an IP literal |
| Destroy | kill the `lume run` child, then `lume delete <name> --force`. When the agent no longer owns the child it runs `lume stop <name> --force` first. If `delete` fails, `lume ls --format json` decides whether the machine is already gone |
| Sweep | `lume ls --format json`, then Destroy for every listed name with the prefix the agent does not own, except the golden image, and removal of their `vm-<name>-*` host directories |

The guest is reached with the system `/usr/bin/ssh` and `/usr/bin/scp`:

- `-F none`, so the agent user's ssh configuration (ProxyCommand, Match exec, control sockets) is never read; only `-i <key>` is offered (`IdentitiesOnly=yes`, `IdentityAgent=none`, `BatchMode=yes`), and agent and X11 forwarding are off.
- Host keys: before the clone, the agent mints an ed25519 host key for that machine alone (openssh-key-v1, checked against `ssh-keygen -y` in the tests), writes it to `<per-machine>/host-key/ssh_host_ed25519_key` and pins its public half in `<per-machine>/known_hosts` under the machine name. lume shares that directory read-only into this one guest, which sees it as `/Volumes/My Shared Files/host-key` (lume 0.7.2 `Commands/Run.swift` and `VMVirtualizationService.createDirectoryShare`). The golden image's launch daemon installs it as the guest's host key (`vm/README.md`, host keys); until it does, the guest answers with a key the agent refuses and the login is retried. ssh runs with `StrictHostKeyChecking=yes`, `HostKeyAlgorithms=ssh-ed25519`, `HostKeyAlias=<name>` and that file. Once the login works, the agent deletes the share's contents. So one guest cannot answer for another, even holding root in its own VM.
- The command runs as a script on ssh's standard input to `/bin/sh -s`, so arguments and environment values never appear in a host argv. Every argument and every `NAME=value` assignment is single-quoted (`'` becomes `'\''`); environment names must be shell identifiers.
- The script records its process group in `/tmp/distribution-<random>.pgid` and `exec`s the command. Cancelling sends `SIGTERM` to that group over a second connection, `SIGKILL` after ten seconds, then kills the local ssh.
- Copies in use `scp -q -p -r` over the SFTP protocol (the default since OpenSSH 9.0), so guest paths are not interpreted by a guest shell. Host paths are made absolute so a `:` never reads as a host.
- Copies out never use scp: a fixed guest script checks the source is a regular file that is not a symlink and `exec`s `cat --` on it, and the agent streams its output into an `O_CREAT|O_EXCL` `0600` file, stopping at the transfer's limit.
- Every host process gets only `PATH`, `HOME`, `TMPDIR` and `LUME_TELEMETRY_ENABLED=false` from the agent's environment.

Defaults: user `lume` (what lume's unattended setup creates), boot timeout 5 minutes, poll 2 seconds, grace 20 seconds.

Not yet proven (S7):

- Boot time and address discovery on vmnet NAT, and two concurrent clones per host. The agent refuses `--capacity 2` with lume until S7 proves the host setup in `vm/README.md` (a `pf` anchor that keeps guests from reaching each other and the host, and pinned ARP entries) isolates two guests.
- The guest automounting the `host-key` share early in boot, and the launch daemon installing the key before the first login.
- `lume delete` right after the `lume run` process is killed, and `lume stop` from a process that does not own the VM.
- The guest's sshd accepting the key and its `sftp-server` subsystem being enabled in the golden image.
