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
| `--state` | `DISTRIBUTION_AGENT_STATE` | `~/Library/Application Support/appwrite-distribution/agent.json` | Runner id and token, written atomically with mode `0600`; a state file other users can read is refused |
| `--name` | `DISTRIBUTION_AGENT_NAME` | host name | |
| `--labels` | `DISTRIBUTION_AGENT_LABELS` | | Comma-separated, such as `xcode-27.2`; for `lume`, the golden image's `vm/labels` joined with commas |
| `--capacity` | `DISTRIBUTION_AGENT_CAPACITY` | `1` | Concurrent leases, at most 2 (the macOS licence ceiling) |
| `--vm` | `DISTRIBUTION_AGENT_VM` | `lume` | `lume`, or `fake` to run leases without a Mac (below) |
| `--golden` | `DISTRIBUTION_AGENT_GOLDEN` | | Golden lume VM, `IMAGE` in `vm/versions.env` ([vm/README.md](../vm/README.md)); required for `lume` and reported as the build image |
| `--ssh-key` | `DISTRIBUTION_AGENT_SSH_KEY` | | Private key the guest account authorises; required for `lume` |
| `--known-hosts` | `DISTRIBUTION_AGENT_KNOWN_HOSTS` | | Pins the golden image's host key, recorded with the golden image's name as the host (`<golden> ssh-ed25519 AAAA…`) |
| `--ssh-user` | `DISTRIBUTION_AGENT_SSH_USER` | `lume` | The account lume's unattended setup creates, which `vm/provision.sh` runs as |
| `--lume` | `DISTRIBUTION_AGENT_LUME` | `lume` on `PATH` | |
| `--directory` | `DISTRIBUTION_AGENT_DIRECTORY` | `$TMPDIR/distribution-agent` | Host scratch space, one `0700` directory per lease, removed when it ends |
| `--allow-http` | `DISTRIBUTION_AGENT_ALLOW_HTTP` | `false` | Local development only |
| `--version` | | | Prints the version set at build time with `-X main.version` |

Logs are JSON on standard error. Runner and lease tokens, signing material and the source URL never appear in them.

SIGTERM or SIGINT drains: heartbeats report `draining`, no new lease is claimed, the leases in flight finish, and the last heartbeat reports `offline`. A second signal stops the leases in flight, which destroys their VMs.

Each free slot claims a lease; an empty queue (204) or a failed claim backs off from 2 to 30 seconds.

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

It then runs `/usr/local/distribution/build.sh` with only `DISTRIBUTION_WORK` in its environment, streaming standard output and error as log events, and copies out:

| Path | |
|---|---|
| `output/app.ipa` | Required |
| `output/result.json` | Required. Only `toolchain.fastlane`, `toolchain.xcode` and `toolchain.flutter` are read; every identity field is taken from the verified IPA, and `toolchain.image` is the golden image name |
| `output/dsyms.zip` | Not copied yet: the protocol has no dSYM upload (open item for the server) |

The IPA is verified with `verify.IPA` against the lease: the identifier from `environment.DISTRIBUTION_IDENTIFIER` (which must also be a key of `signing.profiles`), `version.name`, `version.code` as `CFBundleVersion`, `signing.teamId`, and every embedded profile byte-equal to one of the lease's.

## Events

Every lease starts with `start` and `stage {stage: "source"}`; `stage {stage: "build"}` follows once the source is on the host. Build output arrives as `log` events in between.

| Outcome | Then |
|---|---|
| Success | `exit {exitCode: 0}`, `artifact result` (read, json, content below), `artifact output` (upload), `complete`. The IPA is uploaded before the exit event, so an exit of 0 always means the artifact is stored |
| Build script exits non-zero | `exit {exitCode: <status>, reason: "build", error: job_exit_nonzero}` |
| Lease timeout (`lease.timeout` seconds from the claim) | `exit {exitCode: 137, reason: "timeout", error: job_failed}`, as Docker reports one |
| Lease canceled by the server | `exit {exitCode: -1, reason: "canceled"}` |
| Lease lost (extension refused, or the lease expired before an extension succeeded) | `exit {exitCode: -1, reason: "lost"}` |
| Source download fails three times | `artifact source` failed (`download_failed`, `download_http_error` or `archive_unknown_format`), `exit {exitCode: -1, reason: "source"}` |
| VM create, copy in or exec fails | `exit {exitCode: -1, reason: "vm"}` |
| `app.ipa` or `result.json` missing or unreadable | `artifact result` failed (`artifact_not_found` or `artifact_json_invalid`), `exit {exitCode: -1, reason: "output"}` |
| Verification fails | `artifact result` failed (`artifact_failed`, message naming every mismatch), `exit {exitCode: -1, reason: "verification"}` |
| Upload fails | `artifact output` failed (`upload_failed`), `exit {exitCode: -1, reason: "upload"}` |
| Invalid lease, agent error or panic | `exit {exitCode: -1, reason: "lease"}` or `"agent"` |

A failure never uploads and never sends `complete`. Every failed exit carries `error {code, message}` with a known orchestrator code. `durationSeconds` is the build script's run time, and 0 for a `-1` exit.

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

`--vm fake` builds in a temporary directory instead of a VM. Its build stands in for the golden image's script: it reads `input/environment` and the lease's profile and writes a signed synthetic IPA (`verify.Sample`) and `result.json` for the lease's identifier, versions and team, so a full lease runs end to end against a server, on any OS. Drill knobs, set as lease environment variables (the server rejects `DISTRIBUTION_` and `APPWRITE_` names):

| Variable | Effect |
|---|---|
| `FIXTURE_EXIT_CODE` | The build exits with this status |
| `FIXTURE_IDENTIFIER` | The IPA is built for this identifier, so verification refuses it |
| `FIXTURE_SLEEP_SECONDS` | The build takes this long, to drill timeouts and cancellation |

```
DISTRIBUTION_AGENT_SECRET=... go run . --vm fake --allow-http \
  --endpoint http://localhost/v1 --state /tmp/agent.json
```

## Unproven

- The lume adapter (spike S7); see [vm/README.md](vm/README.md).
- Event data shapes, the upload field name and the `result` content are pending the server's lease events action (m4b).
- No lease has run end to end on a real Mac.
