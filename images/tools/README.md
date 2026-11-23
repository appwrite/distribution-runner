# Tools image

`appwrite/distribution-tools` is the **trusted** Linux job image. It never runs customer code. It signs Android bundles (RFC §8.4) and inspects uploaded `.aab` and `.ipa` files (RFC §14), treating every input as hostile. Both jobs need no network.

Debian trixie slim, pinned by index digest, with `openjdk-21-jdk-headless` (a JDK, for `jarsigner` and `keytool`), `openssl`, `python3`, `ca-certificates`, coreutils `timeout`, bundletool 1.18.3 (SHA-256 pinned, wrapped as `bundletool` with `-Xmx512m`) and Apple's root certificates. `unzip` is not installed: every archive read goes through Python's `zipfile` after the checks below.

The server's CI builds it from a pinned commit:

```sh
docker build -t appwrite/distribution-tools:ci https://github.com/appwrite/distribution-runner.git#<sha>:images/tools
docker build --target test -t appwrite/distribution-tools:test https://github.com/appwrite/distribution-runner.git#<sha>:images/tools
```

The first command passes no `--target`, so it relies on `release` staying the last stage. `make tools` passes `--target release` explicitly.

| Stage | Contents |
|---|---|
| `base` | Everything below |
| `test` | `base` plus `/usr/local/distribution/test/` (the test-only CA, the test profile signer and `mint-ipa.py`, see [test/README.md](test/README.md)); `trust.pem` also trusts the test CA, and `DISTRIBUTION_TRUST` is honoured. Never published |
| `release` | `base`. Last stage, so it is the default target and what is published |

| Path | |
|---|---|
| `/usr/local/distribution/sign-android.sh` | Sign job command |
| `/usr/local/distribution/inspect.sh` | Inspect job command; runs `inspect-aab.sh` or `inspect-ipa.py` |
| `/usr/local/distribution/archive.py` | Zip checks shared by both jobs, run before anything else parses the archive |
| `/usr/local/distribution/manifest.py` | Reads the identity from one `bundletool dump manifest` |
| `/usr/local/distribution/signature.py` | Checks `jarsigner -verify -verbose -certs` output |
| `/usr/local/distribution/bundle.py` | Loads and deletes the one-time signing bundle |
| `/usr/local/distribution/certificates/apple-root-ca.cer`, `apple-root-ca-g3.cer` | Apple Root CA and Apple Root CA - G3 (DER), from apple.com, SHA-256 checked at build time |
| `/usr/local/distribution/certificates/apple.pem` | Both Apple roots (PEM) |
| `/usr/local/distribution/certificates/trust.pem` | Trust bundle for provisioning profiles: `apple.pem`, plus the test CA in the `test` stage |

The image runs as root by default and works as any user that can write the workspace (`--user`).

## Archive ceilings

Both jobs run `archive.py` first. It reads the end of central directory record (zip64 aware), refuses a central directory larger than its ceiling or not where the record says, then walks the directory headers itself, counting entries and stopping at the entry ceiling, before Python's `zipfile` builds anything. An end record whose entry count disagrees with the directory, duplicate names, encrypted entries, and compression other than stored or deflated are refused.

| Variable | Default | |
|---|---|---|
| `DISTRIBUTION_MAX_ENTRIES` | `100000` | Entry count, counted from the central directory itself |
| `DISTRIBUTION_MAX_DIRECTORY_BYTES` | `67108864` | Central directory size |
| `DISTRIBUTION_MAX_UNCOMPRESSED_BYTES` | `16000000000` | Sum of declared uncompressed sizes |
| `DISTRIBUTION_MAX_MANIFEST_BYTES` | `8388608` | Declared size of `base/manifest/AndroidManifest.xml` (bundles only) |
| `DISTRIBUTION_MAX_JAVA_SECONDS` | `600` | Wall clock for each `bundletool`, `keytool` and `jarsigner` run |

A value that is not a positive integer is a usage error (sign 7, inspect 4).

Declared sizes do not bound what Java inflates: `jarsigner` and `keytool -jarfile` inflate each entry to its real end, so a deflate bomb that declares small sizes costs CPU until it ends. Every Java run is therefore under `timeout` and exits 8 when it runs longer than `DISTRIBUTION_MAX_JAVA_SECONDS` (TERM, then KILL 10 seconds later). A Python pass that inflated every entry to check its end of stream would cost a full extra inflate of up to 16 GB on every job, so it is not done.

Every child process runs in the background and the script waits for it, so `SIGTERM` and `SIGINT` are handled at once even while Java runs: the child is stopped and the script exits 143 or 130.

## Sign: `sign-android.sh`

| Input | Default | |
|---|---|---|
| `DISTRIBUTION_WORKSPACE` | `/mnt/code` | Job workspace |
| `$workspace/signing/bundle.json` | | One-time signing bundle, deleted before it is parsed |
| `DISTRIBUTION_INPUT` | `$workspace/input/app-unsigned.aab` | Unsigned bundle |
| `DISTRIBUTION_IDENTIFIER` | required | Expected manifest `package` |
| `DISTRIBUTION_VERSION_CODE` | required | Expected `android:versionCode`: a positive integer without leading zeros |
| `DISTRIBUTION_OUTPUT_DIR` | unset | When set and different from `$workspace/output`, `app.aab` and `result.json` are also copied there |
| `DISTRIBUTION_MAX_*` | see above | Archive ceilings and the Java time limit |

`bundle.json` has exactly the keys `keystore` (base64 PKCS#12 or JKS, no line breaks), `storePassword`, `keyAlias` and `keyPassword`, all non-empty strings; passwords and the alias are single lines, and the alias does not start with `-`. A PKCS#12 keystore has one password, so `keyPassword` must equal `storePassword`; a JKS keystore may use a different key password.

Steps:

1. Read `bundle.json`, delete it, then validate it. The keystore, both passwords, the alias and the exported certificate go to `0600` files in a private directory on tmpfs (`/dev/shm/signing.*`; only if that cannot be created, a private directory under `$TMPDIR`). It is removed on exit, and because it is on tmpfs it never reaches the container's disk layer, so a SIGKILL, an OOM kill or a timeout kill leaves nothing that `docker cp` could read from the stopped container. Passwords reach `keytool` and `jarsigner` only through `-storepass:file` and `-keypass:file`, never the environment, the command line or the output.
2. Run the archive checks above and refuse an input that already carries a signature entry: a top-level `META-INF/` entry, compared case-insensitively, ending in `.SF`, `.RSA`, `.DSA` or `.EC`, or starting with `SIG-`.
3. Run `bundletool dump manifest` once, parse its XML in `manifest.py`, and refuse a `package` or `versionCode` other than the expected one.
4. Export the keystore certificate with `keytool -exportcert` (a wrong store password or an unknown alias fails here) and pick the algorithm from its public key with `openssl` (RSA → `SHA256withRSA`, EC → `SHA256withECDSA`). Sign with `jarsigner -sigalg <that> -digestalg SHA-256`. No `-storetype`: the JDK detects PKCS#12 and JKS itself.
5. Verify by parsing `jarsigner -verify -verbose -certs` in `signature.py`: `jar verified.`, exactly one signer, digest `SHA-256`, the pinned signature algorithm, and no unsigned entries.
6. Check that the bundle's signer certificate (`keytool -printcert -jarfile`) is the keystore certificate, then write the outputs. `$workspace/output` is made `0755` so an uploader running as another user can read it.

| Output | |
|---|---|
| `$workspace/output/app.aab` | Signed bundle, renamed into place only after it verifies |
| `$workspace/output/result.json` | Also printed to stdout |

```json
{"artifact": "app.aab", "type": "aab", "digest": "<sha256 of app.aab>", "fingerprint": "<sha256 of the signer certificate>", "identifier": "io.appwrite.example", "versionName": "1.0.0", "versionCode": 57, "minSdk": 24, "targetSdk": 36, "expiresAt": "2056-11-23T10:00:00Z"}
```

`versionCode`, `minSdk` and `targetSdk` are JSON integers (`minSdk` and `targetSdk` are `null` when the manifest has no `uses-sdk`). `versionName` is `null` when the manifest has none, as in the inspect result. `fingerprint` and `digest` are lowercase hex. `expiresAt` is the signing certificate's `notAfter` in UTC; for a JKS identity it fills the fingerprint and expiry the server cannot read itself (RFC §10.3).

| Exit | When |
|---|---|
| 0 | Signed, verified, outputs written |
| 3 | Refused: package or version code mismatch, input not a readable zip, archive ceilings exceeded, end record that disagrees with the central directory, duplicate entry names, encrypted entries, unsupported compression, unreadable manifest, non-numeric SDK level |
| 4 | Refused: the input already carries a signature |
| 5 | Signing failed: wrong store or key password, unknown alias, unreadable certificate, unsupported key algorithm, `jarsigner` error |
| 6 | Verification failed: `jar verified.` missing, not exactly one signer, a digest or signature algorithm other than the pinned one, unsigned entries, an unreadable signer certificate, or a signer other than the keystore certificate |
| 7 | `bundle.json` missing or malformed, `DISTRIBUTION_IDENTIFIER` or `DISTRIBUTION_VERSION_CODE` missing or malformed, a `DISTRIBUTION_MAX_*` value that is not a positive integer, or the input bundle missing. The bundle is deleted in every case |
| 8 | A `bundletool`, `keytool` or `jarsigner` run took longer than `DISTRIBUTION_MAX_JAVA_SECONDS` |
| 130, 143 | Interrupted (`SIGINT`, `SIGTERM`) |

## Inspect: `inspect.sh [file]`

| Input | Default | |
|---|---|---|
| positional argument | | Artifact path; wins over everything below |
| `DISTRIBUTION_INPUT` | `$workspace/input/artifact` | Artifact path |
| `DISTRIBUTION_ARTIFACT_TYPE` | required | `aab` or `ipa` |
| `DISTRIBUTION_TRUST` | ignored | Honoured only in the `test` stage (when `/usr/local/distribution/test` exists): a PEM bundle to verify provisioning profiles against instead of `trust.pem`. The release image always uses `trust.pem` |
| `DISTRIBUTION_MAX_*` | see above | Archive ceilings and the Java time limit |

Nothing is extracted to disk. Every single-entry read in `inspect-ipa.py` is bounded (1 MiB for `Info.plist` and the profile, 1 MiB of load commands, 64 MiB of code signature), and the executable is streamed, never read whole.

- **Bundle** (`inspect-aab.sh`): the manifest through one `bundletool dump manifest`. A bundle with signature entries must pass `jarsigner -verify` with the same parser as the sign job: `jar verified.`, exactly one signer, a SHA-256, SHA-384 or SHA-512 digest, a SHA-2 or EdDSA signature algorithm, and no unsigned entries. Its `fingerprint` is the signer certificate's SHA-256.
- **IPA** (`inspect-ipa.py`): `Info.plist` (binary or XML) from the single `Payload/*.app`, then the first slice of the executable's `LC_CODE_SIGNATURE`:
  - the CodeDirectory gives the ad hoc flag, the identifier and the team; the entitlements blob gives `entitlements`;
  - a signature that is not ad hoc must carry a CMS blob with exactly one signer that verifies over the CodeDirectory (`openssl cms -verify -noverify`, signature only); `fingerprint` is the SHA-256 of that signer certificate;
  - a signature that is not ad hoc must seal what is reported: the CodeDirectory identifier equals `CFBundleIdentifier`, special slot 1 holds the hash of `Info.plist`, slot 5 the hash of the entitlements blob and slot 7 the hash of the DER entitlements blob (each with the CodeDirectory's own hash type; a sealed blob that is missing, or a blob that is not sealed, is refused);
  - `embedded.mobileprovision` must verify with `openssl cms -verify -CAfile trust.pem -purpose any`, have exactly one signer, and that signer must be Apple's profile signing certificate: subject `C=US,O=Apple Inc.,CN=Apple iPhone OS Provisioning Profile Signing` and issuer `C=US,O=Apple Inc.,OU=Certification Authority,CN=Apple iPhone Certification Authority` (RFC 2253, checked against real Apple profiles). A profile signed by any other certificate that chains to Apple Root CA, such as a developer's own distribution certificate, is refused. `profileExpiresAt` and `teamId` come from it;
  - for a signature that is not ad hoc, the profile's `TeamIdentifier` must equal the CodeDirectory team, the signer certificate must be one of the profile's `DeveloperCertificates`, and the profile's `Entitlements.application-identifier` must be `<prefix>.<pattern>` where the prefix is the team or one of `ApplicationIdentifierPrefix`, and the pattern is `CFBundleIdentifier` or a wildcard (`*`, `io.appwrite.*`) that covers it. A signed `application-identifier` entitlement must equal `<prefix>.<CFBundleIdentifier>`;
  - `signed` is true only when `LC_CODE_SIGNATURE` is present, it is not ad hoc, and a profile is present.

| Output | |
|---|---|
| stdout and `$workspace/output/result.json` | One JSON object |

```json
{"digest": "<sha256>", "size": 123456, "type": "ipa", "identifier": "io.appwrite.example", "versionName": "1.0", "versionCode": "42", "signed": true, "fingerprint": "<sha256>", "minSdk": null, "targetSdk": null, "entitlements": {"application-identifier": "ABCDE12345.io.appwrite.example"}, "profileExpiresAt": "2027-06-30T12:00:00Z", "teamId": "ABCDE12345"}
```

For a bundle, `versionCode`, `minSdk` and `targetSdk` are integers and `entitlements`, `profileExpiresAt` and `teamId` are `null`. For an IPA, `versionCode` is the `CFBundleVersion` string and `minSdk` and `targetSdk` are `null`. `size` is the file size in bytes; the server compares it with the uploaded size.

| Exit | When |
|---|---|
| 0 | Inspected |
| 2 | Malformed or over a ceiling: not a zip, too many entries, central directory too large or misplaced, end record that disagrees with the central directory, too large, duplicate names, encrypted entries, unsupported compression, corrupt entry data, missing manifest, `Info.plist` or executable, not a Mach-O file, truncated executable, or any other unreadable input |
| 3 | Invalid: a bundle signature that does not verify, covers only some entries, has more than one signer or a weak algorithm; an IPA profile that does not verify against the trust bundle or is not signed by Apple's profile signing certificate, a CMS signature that does not verify over the CodeDirectory, a CodeDirectory that does not seal the reported identifier, `Info.plist` or entitlements, a team mismatch, a signer missing from the profile, or a profile application identifier that does not cover the bundle |
| 4 | `DISTRIBUTION_ARTIFACT_TYPE` not `aab` or `ipa`, the input missing, or a `DISTRIBUTION_MAX_*` value that is not a positive integer |
| 8 | A `bundletool`, `keytool` or `jarsigner` run took longer than `DISTRIBUTION_MAX_JAVA_SECONDS` |
| 130, 143 | Interrupted (`SIGINT`, `SIGTERM`) |

## Contract changes for the server

- New exit code 8 (timeout) in both jobs; exit 1 no longer occurs for malformed input.
- Inspect exit 3 now also covers a bundle with more than one signer or a weak algorithm, and an IPA whose profile signer is not Apple's, whose signature does not seal its `Info.plist`, identifier or entitlements, or whose profile application identifier does not cover it.
- The ceilings are `DISTRIBUTION_MAX_*` (previously `MAX_ENTRIES`, `MAX_UNCOMPRESSED_BYTES`, `MAX_MANIFEST_BYTES`); `DISTRIBUTION_MAX_DIRECTORY_BYTES` and `DISTRIBUTION_MAX_JAVA_SECONDS` are new.
- `DISTRIBUTION_TRUST` is ignored by the release image.
- The sign `result.json` reports `versionName: null` instead of `""` when the manifest has none.
- A `DISTRIBUTION_VERSION_CODE` of `0` or with a leading zero exits 7.
- The test stage's profiles must be signed by the committed test profile signer (see [test/README.md](test/README.md)); signing them with the test CA key directly now exits 3.

## Publishing

`.github/workflows/images.yml` builds, scans and tests on every pull request that touches the image, and publishes only for a tag whose commit is on `main`. The amd64 image published is the one `make tools` built and `make scan-tools` and `make test-tools` ran against; it is pushed as `<version>-amd64`. The arm64 image is built once under QEMU, Trivy-scanned and smoke-tested (`inspect.sh` on the committed fixture bundle), and pushed as `<version>-arm64`. `<version>` is then an index of exactly those two pushed images, nothing is rebuilt. The full bats suite does not run on arm64: under QEMU on the hosted runners its well over a hundred JVM starts would push the job past its time limit.

## Open items

- **Non-root.** The job runs as root because the orchestrator's workspace ownership is not confirmed. Every parser treats its input as hostile (bounded reads, the archive ceilings, no extraction, Java under a time limit), and the bats suite already runs the scripts as uid 10001. Once the orchestrator's workspace ownership is confirmed, add a fixed non-root user (for example `USER 10001:10001`) to `release` and document the ownership the orchestrator must provide.
