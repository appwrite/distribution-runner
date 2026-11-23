# Fixture build image

A tiny stand-in for `appwrite/distribution-android` in end-to-end tests. Its `build.sh` honours the same job contract as the real Android build job (RFC §8.3) but builds nothing: it emits a pre-generated unsigned bundle, optionally rewritten to the requested identity, and a `result.json`.

The server's CI builds it from a pinned commit:

```sh
docker build -t appwrite/distribution-fixture:ci https://github.com/appwrite/distribution-runner.git#<sha>:images/fixture
```

## Contract

Command `/usr/local/distribution/build.sh`, workspace `/mnt/code`. The image runs as root by default and also works as any other user (`--user`) that can write the workspace.

| Input | Default | Effect |
|---|---|---|
| `DISTRIBUTION_OUTPUT_DIR` | `/mnt/code/output` | Where the bundle, `mapping.txt` and a copy of `result.json` go |
| `DISTRIBUTION_CACHE_ARTIFACT` | unset | When set, a small placeholder file is written there (a relative path is relative to the workspace). It is not a squashfs image |
| `DISTRIBUTION_IDENTIFIER` | `io.appwrite.distribution.fixture` | Manifest `package` |
| `DISTRIBUTION_VERSION_NAME` | `1.0.0` | Manifest `android:versionName` |
| `DISTRIBUTION_VERSION_CODE` | `1` | Manifest `android:versionCode` |
| `DISTRIBUTION_SOURCE` | `/mnt/code/source` | Exported for parity with the Android image; unused |

An empty value is treated exactly like an unset one. When all three identity values are empty, the output is byte-identical to the committed `app-unsigned.aab`. Otherwise `manifest.py` rewrites only the proto manifest inside the bundle and keeps every other entry and every unknown protobuf field unchanged. The bundle has no code and no resources, so its resource table names no package and needs no rewrite. The output is never signed.

| Output | |
|---|---|
| `$DISTRIBUTION_OUTPUT_DIR/app-unsigned.aab` | Written under a temporary name and renamed when complete |
| `$DISTRIBUTION_OUTPUT_DIR/mapping.txt` | Always written, always empty, also on a failed build |
| `/mnt/code/output/result.json` | Read by the sidecar; copied to `$DISTRIBUTION_OUTPUT_DIR` when that is a different directory |

`result.json` reports what the bundle actually contains, read back from its manifest:

```json
{"artifact": "app-unsigned.aab", "type": "aab", "identifier": "io.appwrite.distribution.fixture", "versionName": "1.0.0", "versionCode": 1, "toolchain": {"image": "appwrite/distribution-fixture:<version>", "fastlane": null, "java": null, "gradle": null, "flutter": null}}
```

`versionCode` is a JSON integer. The toolchain fields other than `image` are `null` because the fixture runs no toolchain.

## Validation and exit codes

| Exit | When |
|---|---|
| 0 | Bundle and `result.json` written |
| 1 | Unexpected failure |
| 2 | Invalid input: an identifier that is not at least two dot-separated segments, each starting with a letter and otherwise letters, digits or underscores; a version code that is not an integer from 1 to 2100000000; a version name that is not 1 to 255 printable characters; a knob that is not a non-negative integer |
| 3 | The bundle is signed (never expected): a top-level `META-INF/` entry, compared case-insensitively, ending in `.SF`, `.RSA`, `.DSA` or `.EC`, or starting with `SIG-`. This is the same rule as the tools image's sign and inspect jobs; before it was case-sensitive and ignored `SIG-` entries |
| `FIXTURE_EXIT_CODE` | The requested failure |
| 143 | Stopped with `SIGTERM` (130 for `SIGINT`) |

## Drill knobs

Their names avoid the `DISTRIBUTION_` and `APPWRITE_` prefixes, which the server rejects for app variables, so a test sets them as ordinary variables.

| Variable | Default | Effect |
|---|---|---|
| `FIXTURE_EXIT_CODE` | `0` | Exit with this code (0 to 255) after the logs and the sleep, writing no bundle and no `result.json` |
| `FIXTURE_SLEEP_SECONDS` | `0` | Sleep before finishing, for timeout and cancel drills. `SIGTERM` ends the sleep at once |
| `FIXTURE_LOG_LINES` | `3` | Number of log lines |
| `FIXTURE_LINE_BYTES` | `0` | When positive, each log line is this many bytes. Lines pass through `fold -b -w 16384`, so none exceeds 16384 bytes |
| `FIXTURE_IDENTIFIER` | unset | Overrides `DISTRIBUTION_IDENTIFIER`, so the sign job sees a package it did not expect and refuses it |
| `FIXTURE_VERSION_CODE` | unset | Overrides `DISTRIBUTION_VERSION_CODE`, for the same refusal on the version code |

## The committed bundle

`app-unsigned.aab` has identifier `io.appwrite.distribution.fixture`, versionName `1.0.0`, versionCode `1`, minSdk 24, targetSdk 36 and `android:hasCode="false"`. It was generated on `linux/amd64` by GitHub Actions run 45111799251 of the `Fixture` workflow (`.github/workflows/fixture.yml`) with the Linux `aapt2`, and is reproducible: regenerating it with the pinned inputs yields the same bytes (SHA-256 `51627b51c597a259f79f1276f549aa81bbd64f1bd4b0058caec48b28bf775be8`). `provenance.json` records the tools and checksums that produced it.

Regenerate it with `make fixture-generate`, which runs `generate.sh` in a pinned `linux/amd64` Debian container: it downloads aapt2, the `platforms;android-36` `android.jar` and bundletool with pinned checksums, links the manifest in proto format, builds the bundle twice to confirm it is deterministic, checks that it is unsigned and passes `bundletool validate`, and reads the manifest back. Only `app-unsigned.aab` and `provenance.json` are committed. If `aapt2` cannot run under emulation, dispatch the `Fixture` workflow on an amd64 runner (see the root README).

`tests/fixture.sh` (`make test-fixture`) covers this contract. It also reads each rewritten bundle back with the tools image's bundletool and signs one with `sign-android.sh`; locally those checks are skipped when the tools image is missing, and with `CI=true` the suite fails instead.
