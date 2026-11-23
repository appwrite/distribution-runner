# Appwrite Distribution Runner

Build images, fixed lanes and the macOS host agent that Appwrite Distribution uses to build, sign and inspect iOS and Android apps. The Appwrite server owns the API, state and store submission; this repository owns everything that runs inside a build or signing job.

Each directory documents its contract in its own README.

## Layout

| Path | Purpose |
|---|---|
| `images/android` | Untrusted Android build image: JDK 17 and 21, Android SDK, Node, Flutter, fastlane, the entry script and the Gradle init script |
| `images/tools` | Trusted signing and inspection image: JDK, bundletool, openssl, python3, Apple root certificates, `sign-android.sh` and `inspect.sh`. Its `test` stage adds a test-only CA |
| `images/fixture` | Tiny build image for end-to-end tests that emits a pre-generated unsigned bundle and a `result.json` |
| `lanes` | The fixed fastlane lane for iOS builds |
| `scripts` | Entry scripts installed into the macOS build VM |
| `vm` | Golden macOS VM image recipe |
| `agent` | macOS host agent: enrols with the server, claims leases, drives a single-use VM per build, verifies and uploads the result |
| `tests` | Image tests run by the Makefile |

## Trust zones

| Zone | Runs customer code | May hold | Never holds |
|---|---|---|---|
| Untrusted Linux job (`images/android`, `images/fixture`) | yes | source, app variables | keystores, store credentials, Appwrite API keys |
| Untrusted macOS VM (`vm`, `lanes`, `scripts`) | yes | source, app variables, the target's iOS signing identity in a temporary keychain | store credentials, other tenants' anything, Appwrite API keys |
| Trusted Linux job (`images/tools`) | no | the Android upload keystore for one build | store credentials |
| macOS host agent (`agent`) | no | lease token, signing identity in transit, a short-lived source URL | store credentials |

Store credentials never reach this repository's images or the agent. They are used only by the server.

## Make targets

| Target | Does |
|---|---|
| `make lint` | `shellcheck` on every `*.sh` (and `tests/*.bats`, `tests/helpers.bash` as bash), hadolint and actionlint through pinned containers |
| `make check` | `lint`, then builds tools, the tools test stage and fixture, then runs `test-tools`, `test-fixture` and `test-agent` |
| `make tools` | `docker build -t appwrite/distribution-tools:$(TAG) images/tools` |
| `make tools-test` | `docker build --target test -t appwrite/distribution-tools:test images/tools` |
| `make fixture` | `docker build -t appwrite/distribution-fixture:$(TAG) images/fixture` |
| `make fixture-generate` | Regenerates the fixture bundle in a pinned `linux/amd64` Debian container |
| `make android` | Builds `appwrite/distribution-android:$(TAG)` for `linux/amd64`. Refuses unless `CI=true` |
| `make agent` | Static (`CGO_ENABLED=0`) agent binaries for `AGENT_PLATFORMS` (default `darwin/arm64 darwin/amd64`) in `agent/bin` |
| `make test-tools` | bats in the tools test image with `--network none` |
| `make test-fixture` | `tests/fixture.sh` |
| `make test-android` | `tests/android.sh`. Refuses unless `CI=true` |
| `make test-agent` | `go vet` and `go test` (`GOTESTFLAGS=-race` in CI) |
| `make scan-tools`, `scan-fixture`, `scan-android` | Trivy gate and report for an image |
| `make scan-agent` | Trivy gate for the agent binaries |

`TAG` defaults to `dev`. Every target whose sources do not exist yet prints `skip:` and exits 0.

## Building from the server's CI

Pin a commit SHA, never a branch:

```sh
docker build -t appwrite/distribution-tools:ci https://github.com/appwrite/distribution-runner.git#<sha>:images/tools
docker build --target test -t appwrite/distribution-tools:test https://github.com/appwrite/distribution-runner.git#<sha>:images/tools
docker build -t appwrite/distribution-fixture:ci https://github.com/appwrite/distribution-runner.git#<sha>:images/fixture
```

Every image build context is self-contained, so these work without a checkout.

## Publishing

Images are published to Docker Hub only from a pushed `X.Y.Z` tag, and only when the `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` secrets are set. Tools and fixture are published for `linux/amd64` and `linux/arm64`, android for `linux/amd64`, each tagged with the version. The tools `test` stage is never published. Pull requests and branch pushes build, scan and test but never publish.

## Vulnerability policy

`make scan-<image>` runs Trivy twice:

- **Gate:** OS packages with a fixed CRITICAL vulnerability fail the build (`--pkg-types os --severity CRITICAL --ignore-unfixed`). For the agent, Go standard library and module findings at CRITICAL or HIGH fail the build; the fix is a Go bump.
- **Report:** library findings at CRITICAL or HIGH (bundletool's jars, Gradle, Flutter, Node, gems) are printed but never fail the build.

Exceptions go only in `.trivyignore`, one `CVE-ID exp:YYYY-MM-DD` per entry with the reason on the comment line above it, and change only through a pull request.

## Regenerating the fixture bundle

`make fixture-generate` runs the generator under `linux/amd64`. If `aapt2` fails under emulation on an arm64 host, run the `Fixture` workflow on an amd64 runner instead:

```sh
gh workflow run fixture.yml --ref <branch>
gh run download --name fixture
```

## License

[BSD 3-Clause](LICENSE)
