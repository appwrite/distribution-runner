# lanes

The fixed fastlane lane for iOS builds (RFC §9.3). `vm/provision.sh` installs it into the golden image:

| Repository | Golden image |
|---|---|
| `lanes/Fastfile` | `/usr/local/distribution/fastlane/Fastfile` |
| `lanes/Gemfile`, `lanes/Gemfile.lock` | `/usr/local/distribution/{Gemfile,Gemfile.lock}`, installed with `bundle config set --local deployment true` into `/usr/local/distribution/vendor/bundle` |

**Unproven pending spike S7:** no build has run through this lane on a real Mac with a real distribution identity.

## Why it is shaped this way

- fastlane has no flag for the Fastfile path; it resolves `./fastlane/Fastfile` (and `Appfile`, `.env`) from its working directory. `build.sh` therefore starts it from `/usr/local/distribution`, which the image owns, with `BUNDLE_GEMFILE=/usr/local/distribution/Gemfile`, `FASTLANE_SKIP_UPDATE_CHECK=1`, `FASTLANE_OPT_OUT_USAGE=1` and `FASTLANE_HIDE_CHANGELOG=1`. Customers cannot supply a Fastfile or a lane name.
- The install command runs first and may install gems and tools of its own, so `build.sh` starts `/usr/local/distribution/ruby/bin/bundle` by absolute path with a `PATH` of image directories only, `GEM_HOME` and `GEM_PATH` unset and `BUNDLE_APP_CONFIG=/usr/local/distribution/.bundle`. The lane hands the customer `PATH` and `GEM_HOME` (`DISTRIBUTION_BUILD_PATH`, `DISTRIBUTION_BUILD_GEM_HOME`) back to the build command alone and restores its own afterwards.
- The lane never changes directory into the checkout. The build command runs through `sh(command:, chdir:)` (fastlane 2.242.0), which leaves fastlane's own working directory alone, inside `Bundler.with_unbundled_env` so a project's own `bundle exec pod install` sees its own Gemfile. `build_app` only receives absolute paths.
- `build_app` (gym) still changes into the Xcode project's directory to read a `Gymfile`, `fastlane/Gymfile` or `.fastlane/Gymfile` there, and evaluates it as Ruby (`Gym::DetectValues.set_additional_default_values`, fastlane 2.242.0). Absolute paths do not prevent that, so the lane deletes those files from the throwaway checkout before calling `build_app`. A process the install or build command left running could write one back in between; it already runs as the same user in the same throwaway VM, so the lane cannot be a boundary against it, and the host verifies the IPA either way.
- gym 2.242.0 accepts `export_method` only from `app-store validation ad-hoc package enterprise development developer-id mac-application` (`gym/lib/gym/options.rb`) and rejects `app-store-connect`. The lane therefore archives only (`skip_package_ipa: true`), and `build.sh` exports with `xcodebuild -exportArchive` and its own `exportOptions.plist` (`method: app-store-connect`, the per-bundle profile map, `signingCertificate` as the identity's SHA-1).
- Signing goes through `xcargs` as `CODE_SIGN_STYLE=Manual`, `DEVELOPMENT_TEAM` and `CODE_SIGN_IDENTITY` only. A global `PROVISIONING_PROFILE_SPECIFIER` would also apply to CocoaPods targets and break them, so the profile mapping lives in the export options alone.

## Inputs

All from the environment `build.sh` prepares:

| Variable | Use |
|---|---|
| `DISTRIBUTION_SOURCE` | Absolute checkout path |
| `DISTRIBUTION_ROOT_DIRECTORY` | Root inside the checkout; must resolve (symlinks included) inside it |
| `DISTRIBUTION_BUILD_COMMAND` | Optional; run in the root before archiving (for example `flutter build ios --config-only --release`) |
| `DISTRIBUTION_CONFIGURATION` | Optional JSON object: `workspace` or `project` (relative to the root), `scheme`, `configuration` (default `Release`) |
| `DISTRIBUTION_SCHEME` | Scheme when the configuration has none; Flutter defaults to `Runner` |
| `DISTRIBUTION_FRAMEWORK` | `flutter` adds `FLUTTER_BUILD_NAME` and `FLUTTER_BUILD_NUMBER` |
| `DISTRIBUTION_VERSION_NAME`, `DISTRIBUTION_VERSION_CODE` | `MARKETING_VERSION` and `CURRENT_PROJECT_VERSION` (and the Flutter pair); empty means unchanged |
| `DISTRIBUTION_TEAM_ID`, `DISTRIBUTION_SIGNING_IDENTITY` | Manual signing settings |
| `DISTRIBUTION_ARCHIVE_PATH` | Absolute `.xcarchive` path; derived data and logs go next to it |

Without a configured `workspace` or `project`, the lane takes the single `*.xcworkspace` in the root, then in `ios/`, then the single `*.xcodeproj` in the root, then in `ios/`. More than one match is an error.

## Regenerating the lock

```sh
docker run --rm -v "$PWD/lanes:/w" -w /w ruby:3.3-slim@sha256:bd9d41c0a4be443c6f750b5af47080e5dbea9cfaf2eb135d86b8d021ab04babf \
  sh -c 'bundle lock --add-platform arm64-darwin x86_64-darwin \
    && bundle lock --remove-platform aarch64-linux-gnu aarch64-linux-musl arm-linux-gnu arm-linux-musl x86_64-linux-gnu x86_64-linux-musl'
```

The lock keeps `arm64-darwin`, `x86_64-darwin` and `ruby`, and `BUNDLED WITH` matches the bundler that Ruby 3.3.14 ships (2.5.22), which `provision.sh` installs.

## Split signing

`scripts/archive-ios.sh` is the archive step of `_APP_DISTRIBUTION_IOS_SIGNING=split` (RFC §9.4): VM 1 archives with code signing disabled and a second VM signs. It is for that future mode only and is not installed in the golden image yet (`vm/README.md`).
