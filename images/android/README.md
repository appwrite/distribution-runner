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
| SDK packages | `platform-tools` 37.0.3, `platforms;android-36` r2, `platforms;android-37.0` r2, `build-tools;35.0.0`, `build-tools;37.0.0`, `ndk;27.1.12297006`, `ndk;28.2.13676358` | Installed by `sdkmanager`, which checks each archive against the repository; the build fails if an installed revision differs from these pins |
| Node.js | `26.13.1` (LTS) | nodejs.org `SHASUMS256.txt` |
| Flutter | `3.41.2` stable | `releases_linux.json`, SHA-256 checked; analytics off, Android artifacts precached |
| Ruby | 3.3 (Debian `ruby`) | apt |
| fastlane | `2.242.0` | `Gemfile.lock`, installed with `BUNDLE_FROZEN` |
| bundletool | `1.18.3` | GitHub release asset digest |
| Other | `git`, `python3`, `squashfs-tools`, `unzip`, `zip`, `xz-utils`, `file` | apt |

Why these SDK packages: React Native 0.88.1 asks for build-tools 37.0.0, compile SDK 37 and NDK 27.1.12297006. Flutter 3.41.2 asks for compile SDK 36 and NDK 28.2.13676358, and its template's AGP 8.11 defaults to build-tools 35.0.0. Anything else a project asks for (another NDK, CMake) is downloaded by AGP at build time, which works because the SDK licences are accepted in the image.

Command-line tools 23.0 replaced `sdkmanager` with a shim over a native `android` CLI that ignores `--licenses`; 22.0 is the last release with the Java `sdkmanager`.

## Job contract

The orchestrator runs `/usr/local/distribution/build.sh` with the workspace at `/mnt/code`.

| Variable | Use |
|---|---|
| `DISTRIBUTION_SOURCE` | Checkout, default `/mnt/code/source` |
| `DISTRIBUTION_ROOT_DIRECTORY` | App root inside the checkout, default `.`; must not escape it |
| `DISTRIBUTION_INSTALL_COMMAND` | Optional, run with `sh -c` in the root |
| `DISTRIBUTION_BUILD_COMMAND` | Required, run by the fastlane lane in the root |
| `DISTRIBUTION_OUTPUT_PATH` | Required, glob relative to the root that must match exactly one bundle (`**` allowed) |
| `DISTRIBUTION_OUTPUT_DIR` | Where the bundle, `mapping.txt` and a copy of `result.json` go, default `/mnt/code/output` |
| `DISTRIBUTION_CACHE_ARTIFACT` | Optional squashfs cache path, absolute or relative to `/mnt/code` |
| `DISTRIBUTION_VERSION_CODE`, `DISTRIBUTION_VERSION_NAME` | Injected into every release variant by `distribution.init.gradle`; empty means keep the project's value |
| `DISTRIBUTION_FRAMEWORK` | `flutter` adds the Flutter version to `result.json` |

`build.sh`, in order:

1. Restores the cache with `unsquashfs` into `/mnt/code/.distribution` when the file exists. An unreadable cache is reported and skipped. `GRADLE_USER_HOME`, `PUB_CACHE`, `npm_config_cache` and `ANDROID_USER_HOME` live there.
2. Runs the install command in the root.
3. Writes `$GRADLE_USER_HOME/gradle.properties`, which overrides the project's: `org.gradle.java.home` is JDK 17 when the wrapper's Gradle is older than 8.5 (the first Gradle that runs on Java 21) and JDK 21 otherwise or when no wrapper is found; `org.gradle.jvmargs` is `-Xmx` half the cgroup memory limit (8 GiB assumed when there is none) plus `-XX:MaxMetaspaceSize=1g`. `JAVA_HOME` and `PATH` follow the same JDK. The wrapper is looked up at `<root>/gradle/wrapper` then `<root>/android/gradle/wrapper`.
4. Copies `distribution.init.gradle` into `$GRADLE_USER_HOME/init.d`. It clears signing configs on release build types, `defaultConfig` and flavors, and injects the version.
5. Runs `bundle exec fastlane android build` from `/usr/local/distribution`, never from the checkout, so a `fastlane/Fastfile` in the checkout is never evaluated.
6. Finds the bundle, refuses it if it has a `META-INF/*.SF|RSA|DSA|EC` entry, reads the package and version back with bundletool.
7. Writes `app-unsigned.aab` and `mapping.txt` (the variant's R8 mapping, else empty) to `DISTRIBUTION_OUTPUT_DIR`, then `result.json` to `/mnt/code/output/result.json` and a copy to `DISTRIBUTION_OUTPUT_DIR`. Every file is written under a temporary name and renamed, so an upload after a kill never sees a partial file.
8. Stops the Gradle daemons and saves `gradle/caches`, `gradle/wrapper`, `pub` and `npm` to the cache path with `mksquashfs` (zstd), skipping lock files and anything named `*.jks`, `*.keystore`, `*.p12` or `*.pfx`. `init.d`, `gradle.properties` and `ANDROID_USER_HOME` are never saved. A failure here never fails the build.

All output passes through `fold -b -w 16384`, and the exit status is preserved.

```json
{"artifact": "app-unsigned.aab", "type": "aab", "identifier": "io.example.app", "versionName": "1.2.3", "versionCode": 42, "toolchain": {"image": "appwrite/distribution-android:1.0.0", "fastlane": "2.242.0", "java": "21.0.13.1+2", "gradle": "8.14", "flutter": "3.41.2"}}
```

`versionCode` is a JSON integer. `gradle` is null when no wrapper is found and `flutter` is null unless the framework is Flutter.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | Bundle and `result.json` written |
| 2 | Invalid input: a required variable is missing, the source or root is missing or escapes the checkout, the output glob does not match exactly one file, or the bundle is not a readable zip |
| 3 | `build_already_signed`: the build produced a signed bundle |
| 4 | `build_keystore_required`: the build failed and the Gradle failure report (`What went wrong`, or the build-file line it points at) mentions a keystore, `key.properties`, `storeFile`, a store or key password or alias, a signing config or a `.jks` file |
| other | The exit status of the install command or the fastlane lane |

## Decisions

- **Runs as root.** The orchestrator's job documentation does not describe a non-root workspace, and AGP's SDK auto-download writes into `/opt/android-sdk`.
- **SDK licences are accepted at image build** (`sdkmanager --licenses`), on the publisher's behalf, so AGP can download missing packages.
- **fastlane stays** to keep the `toolchain.fastlane` contract and the proven lane, at the cost of a Ruby runtime.
- **Vulnerability gate:** OS packages with a fixable CRITICAL fail CI; library findings (Gradle, Flutter, Node, gems, bundletool) are reported only.

## Tests

`make test-android` (CI only) runs `tests/android.sh` against `ANDROID_IMAGE`: a cold `flutter create` build with version `4242` / `9.8.7-smoke` under an 8 GiB memory limit, a warm build restored from the saved cache, a signed bundle (exit 3) and an unguarded `key.properties` read (exit 4).

## Updating a pin

Change the `ARG` URL and SHA-256 together, from the source in the table. For SDK packages change both the `sdkmanager --install` list and the expected `path=revision` list. Regenerate the lock with:

```sh
docker run --rm -v "$PWD/images/android:/w" -w /w ruby:3.3-slim bundle lock --update fastlane --add-platform x86_64-linux
```
