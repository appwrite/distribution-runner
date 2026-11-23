# vm

Recipe for the golden macOS image that every iOS build VM is cloned from (RFC §9.3–9.5, Appendix J). The agent clones it with lume for each lease, copies the inputs in, runs `/usr/local/distribution/build.sh`, copies the outputs out and destroys the clone (`agent/vm/README.md`).

**Unproven pending spike S7.** This recipe has never been run, and no iOS build has gone through `build.sh` on a real Mac with a real Apple distribution identity.

| File | Purpose |
|---|---|
| `versions.env` | Every pin: image name, macOS, Xcode, Ruby (through ruby-build), libyaml, fastlane, CocoaPods, Node, Flutter, Apple WWDR intermediates |
| `labels` | Runner labels the image advertises; `provision.sh` refuses to run when they disagree with `versions.env` |
| `provision.sh` | Run once over SSH, as the guest user, on a fresh VM with Xcode installed (bash 3.2) |

## Image layout

| Path | Contents | Owner |
|---|---|---|
| `/usr/local/distribution/build.sh` | `scripts/build-ios.sh` | root |
| `/usr/local/distribution/fastlane/Fastfile` | `lanes/Fastfile` | root |
| `/usr/local/distribution/{Gemfile,Gemfile.lock,.bundle,vendor}` | fastlane 2.242.0, deployment bundle | root |
| `/usr/local/distribution/ruby` | Ruby 3.3.14 (ruby-build, its own OpenSSL, libyaml), bundler, CocoaPods | root |
| `/usr/local/distribution/node` | Node | root |
| `/usr/local/distribution/flutter` | Flutter with the iOS artifacts precached | guest user, because Flutter writes its own cache |
| `/usr/local/distribution/{versions.env,labels}` | What `build.sh` reports in `toolchain` | root |

The image also has the Apple WWDR G3–G6 intermediates in the System keychain (so an Apple-issued distribution certificate is a valid identity), automatic updates and Spotlight off, ssh password and keyboard-interactive login off, no Xcode account, no signing identity and no provisioning profile.

## `build.sh` contract

The agent writes, under `DISTRIBUTION_WORK` (default `$HOME/distribution`):

| Input | Contents |
|---|---|
| `input/source.tar.gz` | The checkout as a single-root archive; unpacked into `source/` with one level stripped, then deleted |
| `input/environment` | `export K='v'` lines: the lease environment plus `DISTRIBUTION_PLATFORM`, `DISTRIBUTION_FRAMEWORK`, `DISTRIBUTION_ROOT_DIRECTORY`, `DISTRIBUTION_INSTALL_COMMAND`, `DISTRIBUTION_BUILD_COMMAND`, `DISTRIBUTION_VERSION_NAME`, `DISTRIBUTION_VERSION_CODE`, `DISTRIBUTION_TEAM_ID` (required), `DISTRIBUTION_BUILD_ID`, `CI=true`. `DISTRIBUTION_IDENTIFIER`, `DISTRIBUTION_CONFIGURATION` and `DISTRIBUTION_SCHEME` come through the lease environment |
| `input/signing/certificate.p12`, `input/signing/password` | The identity; both deleted as soon as the identity is in the keychain |
| `input/signing/profiles/<bundleId>.mobileprovision` | One App Store profile per bundle identifier, the app's and each extension's |

and reads back `output/app.ipa`, `output/result.json` and `output/dsyms.zip` after a zero exit.

In order, `build.sh`:

1. Sources `input/environment`, then sets its own `PATH`, `IFS`, `DISTRIBUTION_HOME=/usr/local/distribution`, `DISTRIBUTION_SOURCE=$DISTRIBUTION_WORK/source`, `GEM_HOME=~/.gem`, `npm_config_prefix=~/.npm-global` and a UTF-8 locale. Unpacks the source and resolves the root directory inside it.
2. Creates `signing.keychain-db` with a random `openssl rand` password, auto-lock after six hours, unlocks it, puts it first on the user search list, imports the PKCS#12 with `-T /usr/bin/codesign` and sets the key partition list. If macOS refuses the PBES2/AES-256 PKCS#12 the server produces (RFC Appendix N, item 3), it re-encodes it once with LibreSSL as PBE-SHA1-3DES and imports that. Exactly one valid code signing identity of `DISTRIBUTION_TEAM_ID` must result.
3. Decodes every profile (`security cms -D`, into the throwaway keychain) and checks it: team, App Store type (no devices), not expired, application identifier covering the bundle identifier, and the identity's certificate among `DeveloperCertificates`. Writes `exportOptions.plist` (`app-store-connect`, manual signing, the team, the identity's SHA-1, the bundle → UUID map) and installs each profile by UUID into both `~/Library/MobileDevice/Provisioning Profiles/` and `~/Library/Developer/Xcode/UserData/Provisioning Profiles/`. Then locks the keychain.
4. Runs the install command in the root with the keychain locked, so dependency installers never see an unlocked identity or the PKCS#12 on disk.
5. Unlocks the keychain and runs `bundle exec fastlane ios build` from `/usr/local/distribution` (`lanes/README.md`), which runs the build command and archives.
6. Exports with `xcodebuild -exportArchive`, reads the built `Info.plist` from the `.ipa`, and checks it against `DISTRIBUTION_IDENTIFIER`, `DISTRIBUTION_VERSION_NAME` and `DISTRIBUTION_VERSION_CODE` (an empty value is not checked).
7. Writes `output/app.ipa`, `output/dsyms.zip` (the archive's `dSYMs`, empty when there are none) and `output/result.json`.
8. On any exit, restores the search list, deletes the keychain, the installed profiles (and their directories when empty) and whatever remains of `input/signing`.

`result.json` is written by python3:

```json
{"artifact": "app.ipa", "type": "ipa", "identifier": "com.example.app", "versionName": "1.4.0", "versionCode": "57", "toolchain": {"image": "distribution-macos-27.0.1-xcode-27.2-r1", "fastlane": "2.242.0", "xcode": "27.2", "flutter": null}}
```

`versionCode` is the `CFBundleVersion` string. `flutter` is null unless `DISTRIBUTION_FRAMEWORK` is `flutter`.

| Exit | Meaning |
|---|---|
| 0 | Success; all three outputs written |
| 1 | The install command, build command or archive failed |
| 2 | Invalid input: a missing file, `DISTRIBUTION_TEAM_ID` unset, an unreadable archive, a root directory outside the checkout, no profile |
| 3 | The built `CFBundleShortVersionString` or `CFBundleVersion` differs from the requested version |
| 4 | Signing material rejected: import failed, not exactly one valid identity of the team, or a profile failed a check |
| 5 | `xcodebuild -exportArchive` failed or did not produce exactly one `.ipa` |
| 6 | The built `CFBundleIdentifier` differs from `DISTRIBUTION_IDENTIFIER` |
| 130, 143 | Interrupted or terminated; cleanup still ran |

## Building the golden image

On the host, with lume 0.7.2 (`LUME_TELEMETRY_ENABLED=false`) and `vm/versions.env` loaded (`. vm/versions.env`):

1. Create and install macOS from a restore image for `MACOS_VERSION` (`lume ipsw` prints Apple's latest supported one). Keep the `.ipsw` and record its SHA-256 next to the image.

   ```sh
   lume create "$IMAGE" --os macOS --ipsw <path-to.ipsw> --cpu 4 --memory 8GB --disk-size 200GB --unattended tahoe
   ```

   The unattended setup creates the admin user `lume` (password `lume`) and enables SSH. lume 0.7.2 ships presets for Sequoia and Tahoe only; whether `tahoe` works for macOS 27 is part of S7.
2. Boot it and find its address:

   ```sh
   lume run "$IMAGE" --display none --vnc disabled &
   lume get "$IMAGE" --format json
   ```

3. Install the agent's public key: `ssh-copy-id -i <agent-key>.pub lume@<address>`. `provision.sh` turns password login off, so do this first.
4. Install Xcode by hand, never signing in to an Apple account inside the VM: download `Xcode_$XCODE_VERSION.xip` on the host from developer.apple.com, `scp` it in, then in the VM:

   ```sh
   xip --expand "Xcode_$XCODE_VERSION.xip"
   sudo mv Xcode.app "/Applications/Xcode-$XCODE_VERSION.app"
   sudo xcode-select --switch "/Applications/Xcode-$XCODE_VERSION.app"
   xcodebuild -downloadPlatform iOS
   rm "Xcode_$XCODE_VERSION.xip"
   ```

5. Provision. `provision.sh` asks for the `lume` password through `sudo`, so it needs a terminal:

   ```sh
   ssh lume@<address> mkdir distribution-runner
   scp -r lanes scripts vm lume@<address>:distribution-runner/
   ssh -t lume@<address> distribution-runner/vm/provision.sh
   ssh lume@<address> rm -rf distribution-runner
   ```

   It accepts the Xcode licence and runs `xcodebuild -runFirstLaunch` (the operator accepts Apple's licence by running it), checks every pinned version, and prints the image name, the labels and the guest's ed25519 host key.
6. Put that host key line in the agent's known hosts file under the alias `$IMAGE`; every clone shares it.
7. `lume stop "$IMAGE"`. The golden image stays stopped: `lume clone` refuses a running source.
8. Start the agent with the golden image `$IMAGE` and the labels from `vm/labels` (one per line).

## Labels

`labels` lists what the image advertises, one per line: `macos-<version>`, `xcode-<version>`, `flutter-<version>`, `node-<version>`, `cocoapods-<version>`. A target's `configuration.xcode` becomes a required `xcode-<version>` label, so a lease only lands on an image with that Xcode.

## Yearly refresh

Apple raises the minimum SDK for App Store uploads every April (the iOS 27 SDK from April 2027). By the end of March each year:

1. Update `versions.env` to the new macOS, Xcode, Flutter, Node and CocoaPods, bump `IMAGE` (a new name, never an in-place rebuild), and update `labels`.
2. Build the new image with the steps above on one host, run a Flutter, a React Native and a native app through it, then roll it out host by host while the old image keeps serving leases that require the old `xcode-` label.
3. Retire the old image once no target requires its label, and record each Xcode's deployment-target floor for preflight (Xcode 27.2 rejects targets below iOS 15.0).

## Pins

| Pin | Value | Source |
|---|---|---|
| macOS | 27.0.1 | build host (`sw_vers`) |
| Xcode | 27.2 (27B52) | build host (`xcodebuild -version`); RFC §9.5 |
| Ruby | 3.3.14 | `cache.ruby-lang.org/pub/ruby/index.txt`, sha256 `d0438ee2…` checked by ruby-build |
| ruby-build | `v20261119` = `b91ffe95bf6350b8fce3170585c442f20b62619f` | GitHub release and tag ref |
| libyaml | 0.2.5 | pyyaml.org tarball, sha256 pinned |
| fastlane | 2.242.0 | rubygems.org latest, released 2026-11-16; `lanes/Gemfile.lock` |
| bundler | 2.5.22 | ships with Ruby 3.3.14; `BUNDLED WITH` |
| CocoaPods | 1.17.0 | rubygems.org latest |
| Node | 26.13.1 (LTS) | `nodejs.org/dist/v26.13.1/SHASUMS256.txt`, same as the android image |
| Flutter | 3.41.2 | `releases_macos.json` (arm64 archive), same as the android image |
| Apple WWDR G3–G6 | sha256 pinned | `apple.com/certificateauthority` |

## Unproven (S7)

- The whole recipe: lume's unattended setup on macOS 27, provisioning, clone and boot times, two concurrent clones per host, and whether clones on the shared NAT can reach each other (password login is off for that reason).
- `security import` of the server's PBES2/AES-256 PKCS#12 on macOS 27, and the LibreSSL re-encode fallback (the re-encode itself is checked on LibreSSL 3.3.6; the import of either form is not).
- `build_app` with manual signing passed only as `CODE_SIGN_STYLE`, `DEVELOPMENT_TEAM` and `CODE_SIGN_IDENTITY`: an app target whose project names no profile may fail to archive under `CODE_SIGN_STYLE=Manual`; apps with extensions, app groups, push and Swift Package resource bundles are the risk.
- The split of `build_app(skip_package_ipa: true)` and `xcodebuild -exportArchive`, an export producing an `.ipa`, and whether export works without network and with no Xcode account.
- `security cms -D -k` importing a profile's embedded certificates into the throwaway keychain rather than the login keychain.
- The real Apple signing path end to end (S3, S4), including `find-identity -v` with only the WWDR intermediates installed.
