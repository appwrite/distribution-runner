# Android build image

`appwrite/distribution-android` runs the untrusted Android build job (RFC §8.3). It receives a customer checkout, builds an **unsigned** release bundle through a fixed fastlane lane, and writes the bundle and `result.json` for the trusted sign job. It never holds keystores, store credentials or Appwrite API keys.

The image is `linux/amd64` only and builds only in CI (`make android` refuses unless `CI=true`).

## Contents

| Component | Version | Source and check |
|---|---|---|
| Base | `debian:trixie-slim` | Multi-arch index digest; `apt-get upgrade` at build |
| Temurin JDK 17 | `17.0.21.1+2` | Adoptium API, SHA-256 checked |
| Temurin JDK 21 | `21.0.13.1+2` | Adoptium API, SHA-256 checked |
| Android command-line tools | `22.0` (`commandlinetools-linux-15859902`) | Google SDK repository, SHA-256 checked (SHA-1 matches `repository2-3.xml`) |
| SDK packages | `platform-tools` 37.0.3, `platforms;android-36` r2, `platforms;android-37.0` r2, `build-tools;35.0.0`, `build-tools;37.0.0`, `cmake;3.22.1`, `ndk;27.1.12297006`, `ndk;28.2.13676358` | Installed by `sdkmanager`, which checks each archive against the repository; the build fails if an installed revision differs from these pins |
| Node.js | `26.13.1` (LTS) | nodejs.org `SHASUMS256.txt` |
| corepack | `0.36.0`, which provides `yarn` and `pnpm` (Node 26 no longer bundles it) | npm registry tarball, SHA-256 checked (its `sha512` integrity matches the registry) |
| Flutter | `3.41.2` stable | `releases_linux.json`, SHA-256 checked; analytics off, Android artifacts precached |
| Ruby | 3.3 (Debian `ruby`) | apt |
| fastlane | `2.242.0` | `Gemfile.lock`, installed with `BUNDLE_FROZEN` |
| bundletool | `1.18.3` | GitHub release asset digest |
| Other | `git`, `python3`, `squashfs-tools`, `unzip`, `zip`, `xz-utils`, `file`, `libatomic1` (the official Node.js binary links `libatomic.so.1`) | apt |

Why these SDK packages: React Native 0.88.1 asks for build-tools 37.0.0, compile SDK 37 and NDK 27.1.12297006. Flutter 3.41.2 asks for compile SDK 36 and NDK 28.2.13676358, its template's AGP 8.11 defaults to build-tools 35.0.0, and its release build installs CMake 3.22.1 (seen in CI on every build before it was added here). Anything else a project asks for (another NDK or CMake) is downloaded by AGP at build time, which works because the SDK licences are accepted in the image.

`/bin/sh` is bash in this image (a local `dpkg-divert` of dash's `/usr/bin/sh`). The orchestrator starts every job as `/bin/sh -c '<command>'` on both backends; Kubernetes sets the container `command`, so an image `ENTRYPOINT` such as tini never runs. dash forks the command and stays PID 1 without a SIGTERM handler, so the kernel drops the pod's SIGTERM and a cancelled or timed-out build runs until SIGKILL at the end of the grace period. bash executes a single command in place, which makes `build.sh` PID 1 with its own handler.

Command-line tools 23.0 replaced `sdkmanager` with a shim over a native `android` CLI that ignores `--licenses`; 22.0 is the last release with the Java `sdkmanager`.

## Job contract

The orchestrator runs `/usr/local/distribution/build.sh` with the workspace at `/mnt/code`.

`build.sh` fixes the image's facts itself: `PATH`, `ANDROID_HOME`, `ANDROID_SDK_ROOT`, `FLUTTER_ROOT`, the JDK paths and the fastlane version in `result.json` are constants, so a project variable with one of those names cannot change the toolchain or what `result.json` reports.

| Variable | Use |
|---|---|
| `DISTRIBUTION_SOURCE` | Checkout, default `/mnt/code/source` |
| `DISTRIBUTION_ROOT_DIRECTORY` | App root inside the checkout, default `.`; must not escape it |
| `DISTRIBUTION_INSTALL_COMMAND` | Optional, run with `sh -c` in the root |
| `DISTRIBUTION_BUILD_COMMAND` | Required, run by the fastlane lane in the root, or in `<root>/android` for React Native |
| `DISTRIBUTION_OUTPUT_PATH` | Required, glob relative to the root that must match exactly one bundle (`**` allowed); for React Native that is `android/app/build/outputs/bundle/release/*.aab` |
| `DISTRIBUTION_OUTPUT_DIR` | Where the bundle, `mapping.txt` and a copy of `result.json` go, default `/mnt/code/output` |
| `DISTRIBUTION_CACHE_ARTIFACT` | Optional squashfs cache path, absolute or relative to `/mnt/code` |
| `DISTRIBUTION_VERSION_CODE`, `DISTRIBUTION_VERSION_NAME` | Injected into every release variant by `distribution.init.gradle`; empty means keep the project's value. A non-empty `DISTRIBUTION_VERSION_CODE` must equal the bundle's `versionCode` |
| `DISTRIBUTION_IDENTIFIER` | Optional; when non-empty, must equal the bundle's package, so a mismatch fails here instead of in the sign job |
| `DISTRIBUTION_FRAMEWORK` | `flutter` adds the Flutter version to `result.json`; `react-native` runs the install command in the root and the build command in `<root>/android` (RFC §8.2, §8.3), and requires that directory |

`build.sh`, in order:

1. Deletes temporary files a killed earlier run left behind: `.app-unsigned.aab.*`, `.mapping.txt.*` and `.result.json.*` in the output directories, and `.<cache name>.*` older than an hour next to the cache file (a younger one may belong to a concurrent build of the same target).
2. Restores the cache with `unsquashfs` into `/mnt/code/.distribution` when the file exists. An unreadable cache is reported and skipped. A cache file on the workspace's own mount (the remote-device form, pulled by the sidecar) is then deleted, so it neither fills the disk during the build nor goes back up unchanged in the post-job upload after a failure; one on another mount (a volume, the local-device form) is kept. `GRADLE_USER_HOME`, `PUB_CACHE`, `npm_config_cache` and `ANDROID_USER_HOME` live in `/mnt/code/.distribution`.
3. Runs the install command in the root.
4. Writes `$GRADLE_USER_HOME/gradle.properties`, which overrides the project's:
   - `org.gradle.java.home` is JDK 17 when the wrapper's Gradle is older than 8.5 (the first Gradle that runs on Java 21) and JDK 21 otherwise or when no wrapper is found. The wrapper is looked up at `<root>/gradle/wrapper` then `<root>/android/gradle/wrapper`; `distributionUrl` may be written with spaces or `:` around the separator. `JAVA_HOME` and `PATH` follow the same JDK.
   - `org.gradle.java.installations.paths` lists both JDKs, because Gradle does not search `/opt/java` and a `jvmToolchain(17)` request (React Native's Gradle plugin) would otherwise download a JDK on every build.
   - `org.gradle.jvmargs` is `-Xmx` half the job's memory limit plus `-XX:MaxMetaspaceSize=1g`. The limit is the smallest `memory.max` from the job's cgroup up to the root, found through `/proc/self/cgroup`, so it is right with or without a cgroup namespace; 8 GiB is assumed when there is none.
   - `kotlin.compiler.execution.strategy=in-process` compiles Kotlin inside that one bounded heap. Otherwise the Kotlin daemon inherits the Gradle daemon's `-Xmx` and metaspace, and the two can claim the whole container before aapt2, R8, Dart or clang start.
5. Copies `distribution.init.gradle` into `$GRADLE_USER_HOME/init.d`. It clears signing configs on release build types, `defaultConfig` and flavors, and injects the version.
6. Runs `bundle exec fastlane android build` from `/usr/local/distribution`, never from the checkout, so a `fastlane/Fastfile` in the checkout is never evaluated.
7. Finds the bundle and copies it under a temporary name in `DISTRIBUTION_OUTPUT_DIR`. Every check runs on that copy, not on the checkout while Gradle daemons are still alive: it refuses a `META-INF/*.SF|RSA|DSA|EC` entry (`META-INF` in any case, as `JarFile` reads it), reads the package and version with bundletool, and compares them with `DISTRIBUTION_IDENTIFIER` and `DISTRIBUTION_VERSION_CODE`.
8. Writes `mapping.txt` (the variant's R8 mapping, else empty) and `result.json` under temporary names, then renames them in this order: `mapping.txt`, the `result.json` copy in `DISTRIBUTION_OUTPUT_DIR`, `/mnt/code/output/result.json`, and `app-unsigned.aab` last. Nothing is renamed until every check has passed, so a failed build publishes nothing, and an upload after a kill never sees a partial file or a bundle without its `result.json`.
9. Stops the Gradle daemons with `gradlew --stop` and waits up to 60 seconds for every Gradle and Kotlin daemon to exit, then kills any that remain, so nothing writes to the caches while they are read. It lists the files first and then saves `gradle/caches`, `gradle/wrapper`, `pub` and `npm` to the cache path with `mksquashfs` (zstd), skipping lock files and anything named `*.jks`, `*.keystore`, `*.p12` or `*.pfx`. `init.d`, `gradle.properties` and `ANDROID_USER_HOME` are never saved. A failure here never fails the build.

All output passes through `stdbuf -oL fold -b -w 16384`: lines leave the container as they are written (plain `fold` buffers about 4 KiB, which held back a whole cold build's log in CI and loses the failure tail on SIGKILL), no line is longer than 16 KiB, and the exit status is preserved.

On SIGTERM (cancel, or the Kubernetes deadline) `build.sh` sends SIGTERM to the build's process group and exits 143 at once (SIGINT: 130), leaving the grace period to the sidecar's post-job uploads. Temporary files of a killed run are removed by the next run's step 1.

```json
{"artifact": "app-unsigned.aab", "type": "aab", "identifier": "io.example.app", "versionName": "1.2.3", "versionCode": 42, "toolchain": {"image": "appwrite/distribution-android:1.0.0", "fastlane": "2.242.0", "java": "21.0.13.1+2-LTS", "gradle": "8.14", "flutter": "3.41.2"}}
```

`versionCode` is a JSON integer. `java` is the JDK's `JAVA_RUNTIME_VERSION`. `gradle` is null when no wrapper is found. `flutter` is null unless the framework is Flutter, and is read from `$FLUTTER_ROOT/bin/cache/flutter.version.json` rather than by starting the Flutter tool.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | Bundle and `result.json` written |
| 1 | The install command or the fastlane lane failed with status 1, 2, 3 or 4 (remapped so a project's own status never reads as one of the codes below) |
| 2 | Invalid input or an unusable bundle: a required variable is missing; the source or root is missing or escapes the checkout; React Native without `<root>/android`; the output glob does not match exactly one file; the bundle is not a readable zip, has no readable manifest, or has a non-integer `versionCode`; the package or `versionCode` differs from `DISTRIBUTION_IDENTIFIER` or `DISTRIBUTION_VERSION_CODE` |
| 3 | `build_already_signed`: the build produced a signed bundle |
| 4 | `build_keystore_required`: the build failed and a Gradle failure report (`What went wrong`, or the build-file line its `Where` points at) mentions a keystore, `key.properties`, `storeFile`, a store or key password or alias, a signing config or a `.jks` file. A script compilation error never counts, since Gradle echoes the offending source line, and neither do the `Execution failed for task` and `A problem occurred` header lines, which name tasks and projects |
| 130, 143 | Terminated by SIGINT or SIGTERM |
| other | The exit status of the install command or the fastlane lane (for example 137 when a child was killed) |

## Decisions

- **Runs as root.** The orchestrator's job documentation does not describe a non-root workspace, and AGP's SDK auto-download writes into `/opt/android-sdk`.
- **SDK licences are accepted at image build** (`sdkmanager --licenses`), on the publisher's behalf, so AGP can download missing packages. This accepts every licence the SDK manager offers, preview licences included; it is on the PR's sign-off list.
- **fastlane stays** to keep the `toolchain.fastlane` contract and the proven lane, at the cost of a Ruby runtime.
- **Vulnerability gate:** OS packages with a fixable CRITICAL fail CI; library findings (Gradle, Flutter, Node, gems, bundletool) are reported only.

## Unproven

- React Native: only the directory layout (install in the root, build in `android/`) runs in CI. A real React Native release build, its CMake and NDK use, and its memory on 4 vCPU / 8 GiB are part of spike S6.
- C++ fan-out: `ninja` sizes its jobs from the host's core count, not the cgroup CPU quota, so a React Native build on a large Kubernetes node can start dozens of clang processes inside a 4 vCPU / 8 GiB job. Measure in S6.
- yarn and pnpm: corepack downloads the version a project's `packageManager` names at build time; it is not cached.

## Tests

`make test-android` (CI only) runs `tests/android.sh` against `ANDROID_IMAGE`, every job started as the orchestrator starts it (`/bin/sh -c /usr/local/distribution/build.sh`):

- a cold `flutter create` build with version `4242` / `9.8.7-smoke` under an 8 GiB memory limit, checking `result.json` against fixed expectations (JDK `21.0.13.1+2-LTS`), the manifest, a non-empty `mapping.txt` and the cache contents;
- a warm build restored from the saved cache with the project's `build`, `.dart_tool` and `android/.gradle` removed;
- a signed bundle with a lower-case `meta-inf/` entry (exit 3), restored from an unreadable cache that is skipped and removed;
- an unguarded `key.properties` read at configuration time (exit 4), and a script compilation error whose source line mentions `key.properties` (exit 1);
- a Gradle 8.4 wrapper written with spaces (JDK 17 selected);
- the React Native layout;
- SIGTERM through `/bin/sh -c`: the log is visible while the build runs, and the job exits 143 within 20 seconds.

## Updating a pin

Change the `ARG` URL and SHA-256 together, from the source in the table. For SDK packages change both the `sdkmanager --install` list and the expected `path=revision` list. For fastlane change `ARG FASTLANE_VERSION` and `fastlane_version` in `build.sh` together (the image build fails when they differ), and regenerate the lock with:

```sh
docker run --rm -v "$PWD/images/android:/w" -w /w ruby:3.3-slim bundle lock --update fastlane --add-platform x86_64-linux
```
