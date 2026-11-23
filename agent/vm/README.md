# agent/vm

The VM layer behind RFC §9.1: `Create`, `Exec`, `Copy` and `Destroy` on a single-use machine cloned from a golden image. The lease runner codes against the `Layer` interface; `Fake` and `Lume` implement it.

## Contract

| Call | Behaviour |
|---|---|
| `Create(ctx, name)` | Clones the golden image, boots it and waits for a working SSH login. On failure it removes the clone and returns a zero `Machine`. Names are a letter or digit followed by letters, digits, `.`, `_` or `-` |
| `Exec(ctx, machine, command)` | Runs `Args` with `Env` added to the guest login environment and returns the exit code. A non-zero exit is not an error. When `ctx` ends, the command's whole process group is terminated in the guest and `Exec` returns `-1` with the context's error. For `Lume`, `255` can also be ssh's own failure |
| `Copy(ctx, machine, transfer)` | `In` copies host → guest, `Out` guest → host, a file or a directory. `Destination` names the copy itself; its parents are created and it must not be an existing directory (`ErrDestination`). Relative guest paths resolve against `Machine.Home`. Modes are preserved, so a `0600` file stays `0600` |
| `Destroy(ctx, machine)` | Stops and deletes the machine. Succeeds when it is already gone and on a zero `Machine`, so it can always be deferred |

## Fake

Machines are temporary directories; guest paths map under `Machine.Address` and cannot leave it. `Build func(work string, env map[string]string) error` stands in for `/usr/local/distribution/build.sh`: `work` is the host directory that `DISTRIBUTION_WORK` names. Returning `*ExitError` chooses the exit code, any other error exits `1`, and a panic propagates out of `Exec`. `Fail func(Call) error` injects an error into any call, and `Calls()` returns every call in order.

## Lume

Unproven pending spike S7: no build has run through this adapter on a real Mac yet.

Licence: lume is MIT ("Copyright (c) 2025 Cua AI, Inc.", `LICENSE.md` of trycua/cua at tag `lume-v0.7.2`). It is chosen over Tart (FSL-1.1-ALv2 since 5 June 2026, Competing Use unresolved), Anka (EULA excludes service-bureau use) and Orka (terms not public) per RFC Appendix J. The agent only executes the `lume` binary; it does not redistribute it.

Commands, verified against the `lume-v0.7.2` sources (`libs/lume/src/Commands`). Every one runs with `LUME_TELEMETRY_ENABLED=false`, which overrides the persisted telemetry preference for that process:

| Step | Command |
|---|---|
| Clone | `lume clone <golden> <name>`; refuses a running source, and gives the clone a fresh random MAC address and machine identifier |
| Boot | `lume run <name> --display none --vnc disabled`, kept running as a child of the agent; the VM lives in that process |
| Address | `lume get <name> --format json`, polled until `status` is `running`, `ipAddress` is set and `sshAvailable` is true. The address must parse as an IP literal |
| Destroy | kill the `lume run` child, then `lume delete <name> --force`. When the agent no longer owns the child it runs `lume stop <name> --force` first. If `delete` fails, `lume ls --format json` decides whether the machine is already gone |

The guest is reached with the system `/usr/bin/ssh` and `/usr/bin/scp`:

- `-F none`, so the agent user's ssh configuration (ProxyCommand, Match exec, control sockets) is never read; only `-i <key>` is offered (`IdentitiesOnly=yes`, `IdentityAgent=none`, `BatchMode=yes`), and agent and X11 forwarding are off.
- Host keys: set `KnownHosts` to a file holding the golden image's host key under `Alias` (the golden name by default), and every clone, which shares that key, is checked with `StrictHostKeyChecking=yes`. Without `KnownHosts`, each machine trusts the first key it presents (`accept-new`) in its own file.
- The command runs as a script on ssh's standard input to `/bin/sh -s`, so arguments and environment values never appear in a host argv. Every argument and every `NAME=value` assignment is single-quoted (`'` becomes `'\''`); environment names must be shell identifiers.
- The script records its process group in `/tmp/distribution-<random>.pgid` and `exec`s the command. Cancelling sends `SIGTERM` to that group over a second connection, `SIGKILL` after ten seconds, then kills the local ssh.
- `scp -q -p -r` uses the SFTP protocol (the default since OpenSSH 9.0), so guest paths are not interpreted by a guest shell, and keeps scp's filename checks on copies out of the untrusted guest. Host paths are made absolute so a `:` never reads as a host.

Defaults: user `lume` (what lume's unattended setup creates), boot timeout 5 minutes, poll 2 seconds, grace 20 seconds.

Not yet proven (S7):

- Boot time and address discovery on vmnet NAT, and two concurrent clones per host.
- `lume delete` right after the `lume run` process is killed, and `lume stop` from a process that does not own the VM.
- The guest's sshd accepting the key and its `sftp-server` subsystem being enabled in the golden image.
