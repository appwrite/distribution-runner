# agent

The trusted macOS host agent of Appwrite Distribution (RFC §9.1–9.3): one static Go binary, standard library only. It enrols the host, sends a heartbeat every 30 seconds, claims iOS build leases and runs each one in a VM cloned from a golden image, which it destroys whatever happens. It never executes customer code itself: everything from the repository runs inside the VM.

| Package | Role |
|---|---|
| `.` (`main.go`) | Flags, enrolment, heartbeat, claim loop, draining |
| `lease` | Runs one lease: source, VM, build, verification, upload, events |
| `protocol` | Runner and lease HTTP client, event reporter, chunked upload |
| `vm` | `Layer` interface with `Fake` and `Lume`; see [vm/README.md](vm/README.md) for the lume adapter and what spike S7 still has to prove |
| `verify` | IPA verifier and `Sample`, the synthetic signed IPA |

## Build and test

```
make agent          # CGO_ENABLED=0 darwin/arm64 and darwin/amd64 binaries in agent/bin, version from TAG
make test-agent     # go vet and go test
```

## Configuration

Every flag has an environment variable; the flag wins.

| Flag | Environment | Default | |
|---|---|---|---|
| `--endpoint` | `DISTRIBUTION_AGENT_ENDPOINT` | | Appwrite endpoint including `/v1`; HTTPS unless `--allow-http` |
| `--secret-file` | `DISTRIBUTION_AGENT_SECRET_FILE` | | File holding the enrolment secret. `DISTRIBUTION_AGENT_SECRET` works too. Read only when the state file has no credentials; never a flag value, so it stays out of `ps` |
| `--state` | `DISTRIBUTION_AGENT_STATE` | `~/Library/Application Support/appwrite-distribution/agent.json` | Runner id and token, written atomically with mode `0600`; a state file other users can read is refused. `<state>.lock` admits one agent per state file |
| `--name` | `DISTRIBUTION_AGENT_NAME` | host name | |
| `--labels` | `DISTRIBUTION_AGENT_LABELS` | | Comma-separated, such as `xcode-27.2`; for `lume`, the golden image's `vm/labels` joined with commas |
| `--capacity` | `DISTRIBUTION_AGENT_CAPACITY` | `1` | Concurrent leases, at most 2 (the macOS licence ceiling). `lume` refuses 2 until spike S7 proves that two guests on one host cannot reach each other ([vm/README.md](../vm/README.md), host setup) |
| `--vm` | `DISTRIBUTION_AGENT_VM` | `lume` | `lume`, or `fake` to run leases without a Mac (below); `fake` needs `--allow-http` |
| `--golden` | `DISTRIBUTION_AGENT_GOLDEN` | | Golden lume VM, `IMAGE` in `vm/versions.env` ([vm/README.md](../vm/README.md)); required for `lume` and reported as the build image |
| `--ssh-key` | `DISTRIBUTION_AGENT_SSH_KEY` | | Private key the guest account authorises; required for `lume` |
| `--ssh-user` | `DISTRIBUTION_AGENT_SSH_USER` | `lume` | The account lume's unattended setup creates, which `vm/provision.sh` runs as |
| `--lume` | `DISTRIBUTION_AGENT_LUME` | `lume` on `PATH` | |
| `--directory` | `DISTRIBUTION_AGENT_DIRECTORY` | `$TMPDIR/distribution-agent` | Host scratch space, one `0700` directory per lease, removed when it ends. It must be the agent user's own and mode `0700`; agents with different state files may share it |
| `--allow-http` | `DISTRIBUTION_AGENT_ALLOW_HTTP` | `false` | Local development only |
| `--version` | | | Prints the version set at build time with `-X main.version` |

Logs are JSON on standard error. Runner and lease tokens, signing material and the source URL never appear in them.

`DISTRIBUTION_AGENT_SECRET` is removed from the agent's environment as it starts, and every child (`lume`, `ssh`, `scp`) gets only `PATH`, `HOME`, `TMPDIR` and `LUME_TELEMETRY_ENABLED=false`. The state file must be a regular file the agent user owns, mode `0600`; it is written atomically and the directory is synced after the rename.

Every VM and scratch directory the agent creates is named for its runner: VMs `distribution-<scope>-<lease id>` and scratch directories `lease-distribution-<scope>-*`, where `<scope>` is the first 12 hex characters of the SHA-256 of the runner id, logged at start as `machines`. At start the agent removes what an earlier run of the same runner left behind after a crash or `kill -9`: those scratch directories under `--directory` (which can hold signing material) and those VMs with their host files. It never touches the golden image, a rollback image, or another runner's VMs and directories, so two agents with their own state files can share a host and a `--directory`. A golden image named with the runner's own prefix makes the sweep refuse to run, and the lock beside the state file stops a second agent from starting as the same runner.

SIGTERM or SIGINT drains: heartbeats report `draining`, no new lease is claimed, the leases in flight finish, and the last heartbeat reports `offline`. A second signal stops the leases in flight, which destroys their VMs; a third ends the process at once.

Each free slot claims a lease; an empty queue (204) or a failed claim backs off from 2 to 30 seconds. A claim answered 401 (a revoked runner token) stops the agent with exit status 1 once the leases in flight finish. A lease that fails on the host (reason `vm` or `agent`) backs off the same way; three in a row stop claiming, and the agent heartbeats `draining` until it is fixed and restarted, so a broken host does not fail the whole queue.

Every request has its own deadline: 30 seconds per attempt of a JSON call, 5 minutes per upload chunk and for any response's headers. A lease extension never outlives the lease it extends. Lease expiry times are moved onto the host clock with the response's `Date` header, so host clock skew neither shortens nor stretches a lease.

## VM contract

Shared with the iOS lane (`scripts/build-ios.sh`, installed as `/usr/local/distribution/build.sh` in the golden image). `DISTRIBUTION_WORK` is `$HOME/distribution` in the guest.

The agent copies in `input/`, every file mode `0600`:

| Path | Content |
|---|---|
| `input/source.tar.gz` | The repository tarball, downloaded on the host, never inside the VM. A provider's single-root archive, so the script strips one level |
| `input/environment` | `export NAME='value'` lines, sorted, each `'` written as `'\''`, for the script to source. The lease's `environment`, then `DISTRIBUTION_PLATFORM`, `DISTRIBUTION_FRAMEWORK`, `DISTRIBUTION_ROOT_DIRECTORY`, `DISTRIBUTION_INSTALL_COMMAND`, `DISTRIBUTION_BUILD_COMMAND`, `DISTRIBUTION_VERSION_NAME`, `DISTRIBUTION_VERSION_CODE`, `DISTRIBUTION_TEAM_ID`, `DISTRIBUTION_BUILD_ID` and `CI=true`, which win over the lease's values. `DISTRIBUTION_IDENTIFIER` (and `DISTRIBUTION_CONFIGURATION` or `DISTRIBUTION_SCHEME`) come from the lease's `environment`. No signing material |
| `input/signing/certificate.p12` | The PKCS#12 identity |
| `input/signing/password` | Its password, with no trailing newline |
| `input/signing/profiles/<bundle id>.mobileprovision` | One per profile in the lease |

It then runs `/usr/local/distribution/build.sh` with only `DISTRIBUTION_WORK` in its environment, streaming standard output and error as log events, and copies out, each as one regular file (no symlink, device or directory) streamed into a new `0600` host file and refused past its limit:

| Path | Limit | |
|---|---|---|
| `output/app.ipa` | 4,000,000,000 bytes (RFC §14's `_APP_DISTRIBUTION_SIZE_LIMIT` default) | Required |
| `output/result.json` | 5 MiB | Required. Only `toolchain.fastlane`, `toolchain.xcode` and `toolchain.flutter` are read; every identity field is taken from the verified IPA, and `toolchain.image` is the golden image name |
| `output/dsyms.zip` | | Not copied yet: the protocol has no dSYM upload (open item for the server) |

The source tarball is refused past 4,000,000,000 bytes too. Its download keeps the descriptor's headers only while redirects stay on the descriptor's host.

The IPA is verified with `verify.IPA` against the lease: the identifier from `environment.DISTRIBUTION_IDENTIFIER` (which must also be a key of `signing.profiles`), `version.name`, `version.code` as `CFBundleVersion`, `signing.teamId`, the embedded profile equal to the lease's for that identifier, and each extension's or watch app's embedded profile equal to the lease's for that bundle's own `CFBundleIdentifier`. `signed` also needs a non-empty CMS signature blob, as RFC §14's inspect job requires. A hostile IPA is bounded: the bytes `archive/zip` may read to index the central directory (16 MiB), 100,000 entries, 8 slices in a universal binary, 1 MiB of entitlements, and a binary property list that may not decode to more than 16 times its size plus 1 MiB. A compressed executable is inflated into the lease's own scratch directory.

## Events

Every lease starts with `start` and `stage {stage: "source"}`; `stage {stage: "build"}` follows once the source is on the host. Build output arrives as `log` events in between.

| Outcome | Then |
|---|---|
| Success | `exit {exitCode: 0}`, `artifact result` (read, json, content below), `artifact output` (upload), `complete`. The IPA is uploaded before the exit event, so an exit of 0 always means the artifact is stored |
| Build script exits non-zero | `exit {exitCode: <status>, reason: "build", error: job_exit_nonzero}` |
| Lease timeout (`lease.timeout` seconds from the claim, setup included) | `exit {exitCode: 137, reason: "timeout", error: job_failed}`, as Docker reports one |
| Lease canceled by the server | `exit {exitCode: -1, reason: "canceled"}` |
| Lease lost (extension refused, or the lease expired before an extension succeeded) | `exit {exitCode: -1, reason: "lost"}` |
| Source download fails three times, or the source is too large | `exit {exitCode: -1, reason: "source"}`, then `artifact source` failed (`download_failed`, `download_http_error` or `archive_unknown_format`) |
| VM create, copy in or exec fails | `exit {exitCode: -1, reason: "vm"}` |
| `app.ipa` or `result.json` missing, not a regular file, too large or unreadable | `exit {exitCode: -1, reason: "output"}`, then `artifact result` failed (`artifact_not_found`, `artifact_read_failed` or `artifact_json_invalid`) |
| Verification fails, or the verified result does not fit in one event | `exit {exitCode: -1, reason: "verification"}`, then `artifact result` failed (`artifact_failed`, message naming every mismatch) |
| Upload fails | `exit {exitCode: -1, reason: "upload"}`, then `artifact output` failed (`upload_failed`) |
| Invalid lease, agent error or panic | `exit {exitCode: -1, reason: "lease"}` or `"agent"` |

A failure never uploads and never sends `complete`; like success, it sends the exit first and the failed artifact after it. Every failed exit carries `error {code, message}` with a known orchestrator code. The message is what the customer sees: for `vm`, `agent` and host-side `source`, `output` and `verification` errors it is a fixed sentence, and the detail, which can name host paths, is only logged. `durationSeconds` is the build script's run time whenever it ran, and 0 when the lease failed before it. An exit of 137 with reason `build` is the script killed by a signal (an OOM kill, say), not a timeout: the server must key on `reason`.

Build output is capped at 50,000,000 bytes per lease, the server's default `_APP_DISTRIBUTION_LOGS_LIMIT`: past it one `stderr` truncation line is sent and the rest dropped. While 16 MiB of output waits to be sent (a slow or failing server), the agent stops reading the guest's output, which pushes back through ssh to the build.

Chunks are uploaded under an id the agent mints (26 lowercase base32 characters, a valid UID) and sends as `x-appwrite-id` on every chunk, the first included, so a first chunk resent after a lost response names the same upload. The upload fails unless a response reports `chunksUploaded` equal to `chunksTotal`.

The `result` content:

```json
{
  "artifact": "app.ipa", "type": "ipa",
  "digest": "<sha256 the agent computed>", "size": 123,
  "identifier": "com.example.app", "versionName": "1.4.0", "versionCode": 57, "buildNumber": "57",
  "teamId": "ABCDE12345", "signed": true, "entitlements": {},
  "toolchain": {"image": "<golden image>", "fastlane": "2.242.0", "xcode": "27.2", "flutter": null}
}
```

The lease is extended every third of its remaining time.

## Running without a Mac

`--vm fake` builds in a temporary directory instead of a VM. It needs `--allow-http` and adds the `fake` label, so it is never mistaken for a production runner and only leases that require `fake` land on it. Its build stands in for the golden image's script: it reads `input/environment` and the lease's profile and writes a signed synthetic IPA (`verify.Sample`) and `result.json` for the lease's identifier, versions and team, so a full lease runs end to end against a server, on any OS. Drill knobs, set as lease environment variables (the server rejects `DISTRIBUTION_` and `APPWRITE_` names):

| Variable | Effect |
|---|---|
| `FIXTURE_EXIT_CODE` | The build exits with this status |
| `FIXTURE_IDENTIFIER` | The IPA is built for this identifier, so verification refuses it |
| `FIXTURE_SLEEP_SECONDS` | The build takes this long, to drill timeouts and cancellation; it stops as soon as the lease does |

```
DISTRIBUTION_AGENT_SECRET=... go run . --vm fake --allow-http true \
  --endpoint http://localhost/v1 --state /tmp/agent.json
```

## Unproven

- The lume adapter (spike S7), including per-machine host keys; see [vm/README.md](vm/README.md).
- Event data shapes, the upload field name, the agent-minted upload id on the first chunk and the `result` content are pending the server's lease events and artifacts actions (m4b).
- A route to requeue a lease that failed on the host instead of failing the build (m4b).
- No lease has run end to end on a real Mac.
