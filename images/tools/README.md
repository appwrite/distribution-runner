# Tools image

`appwrite/distribution-tools` is the **trusted** Linux job image. It never runs customer code. It signs Android bundles (RFC §8.4) and inspects uploaded `.aab` and `.ipa` files (RFC §14), treating every input as hostile. Both jobs need no network.

Debian trixie slim, pinned by index digest, with `openjdk-21-jdk-headless` (a JDK, for `jarsigner` and `keytool`), `openssl`, `python3`, `unzip`, `ca-certificates`, bundletool 1.18.3 (SHA-256 pinned, wrapped as `bundletool` with `-Xmx512m`) and Apple's root certificates.

The server's CI builds it from a pinned commit:

```sh
docker build -t appwrite/distribution-tools:ci https://github.com/appwrite/distribution-runner.git#<sha>:images/tools
docker build --target test -t appwrite/distribution-tools:test https://github.com/appwrite/distribution-runner.git#<sha>:images/tools
```

| Stage | Contents |
|---|---|
| `base` | Everything below |
| `test` | `base` plus `/usr/local/distribution/test/` (the test-only CA and `mint-ipa.py`, see [test/README.md](test/README.md)); `trust.pem` also trusts the test CA. Never published |
| `release` | `base`. Last stage, so it is the default target and what is published |

| Path | |
|---|---|
| `/usr/local/distribution/sign-android.sh` | Sign job command |
| `/usr/local/distribution/inspect.sh` | Inspect job command; runs `inspect-aab.sh` or `inspect-ipa.py` |
| `/usr/local/distribution/certificates/apple-root-ca.cer`, `apple-root-ca-g3.cer` | Apple Root CA and Apple Root CA - G3 (DER), from apple.com, SHA-256 checked at build time |
| `/usr/local/distribution/certificates/apple.pem` | Both Apple roots (PEM) |
| `/usr/local/distribution/certificates/trust.pem` | Default trust bundle for provisioning profiles: `apple.pem`, plus the test CA in the `test` stage |

The image runs as root by default and works as any user that can write the workspace (`--user`).

## Sign: `sign-android.sh`

| Input | Default | |
|---|---|---|
| `DISTRIBUTION_WORKSPACE` | `/mnt/code` | Job workspace |
| `$workspace/signing/bundle.json` | | One-time signing bundle, deleted before it is parsed |
| `DISTRIBUTION_INPUT` | `$workspace/input/app-unsigned.aab` | Unsigned bundle |
| `DISTRIBUTION_IDENTIFIER` | required | Expected manifest `package` |
| `DISTRIBUTION_VERSION_CODE` | required | Expected `android:versionCode` |
| `DISTRIBUTION_OUTPUT_DIR` | unset | When set and different from `$workspace/output`, `app.aab` and `result.json` are also copied there |

`bundle.json` has exactly the keys `keystore` (base64 PKCS#12 or JKS, no line breaks), `storePassword`, `keyAlias` and `keyPassword`, all non-empty strings; passwords and the alias are single lines, and the alias does not start with `-`. A PKCS#12 keystore has one password, so `keyPassword` must equal `storePassword`; a JKS keystore may use a different key password.

Steps:

1. Read `bundle.json`, delete it, then validate it. The keystore, both passwords and the alias go to `0600` files in a private temporary directory that is removed on exit. Passwords reach `keytool` and `jarsigner` only through `-storepass:file` and `-keypass:file`, never the environment, the command line or the output.
2. Refuse an input that is not a zip archive, exceeds the archive ceilings, or already carries a signature entry (`META-INF/*.SF`, `.RSA`, `.DSA`, `.EC`, `SIG-*`).
3. Read `package`, `versionCode`, `versionName`, `minSdkVersion` and `targetSdkVersion` with `bundletool dump manifest --xpath` and refuse a package or version code other than the expected one.
4. Detect the key algorithm with `keytool -list -v` (RSA → `SHA256withRSA`, EC → `SHA256withECDSA`) and sign with `jarsigner -sigalg <that> -digestalg SHA-256`. No `-storetype`: the JDK detects PKCS#12 and JKS itself.
5. Verify by parsing `jarsigner -verify -verbose -certs`: `jar verified.`, exactly one signer, digest `SHA-256`, the pinned signature algorithm, and no unsigned entries.
6. Check that the bundle's signer certificate (`keytool -printcert -jarfile`) is the keystore certificate (`keytool -exportcert`), then write the outputs.

| Output | |
|---|---|
| `$workspace/output/app.aab` | Signed bundle, renamed into place only after it verifies |
| `$workspace/output/result.json` | Also printed to stdout |

```json
{"artifact": "app.aab", "type": "aab", "digest": "<sha256 of app.aab>", "fingerprint": "<sha256 of the signer certificate>", "identifier": "io.appwrite.example", "versionName": "1.0.0", "versionCode": 57, "minSdk": 24, "targetSdk": 36, "expiresAt": "2056-11-23T10:00:00Z"}
```

`versionCode`, `minSdk` and `targetSdk` are JSON integers (`minSdk` and `targetSdk` are `null` when the manifest has no `uses-sdk`). `fingerprint` and `digest` are lowercase hex. `expiresAt` is the signing certificate's `notAfter` in UTC; for a JKS identity it fills the fingerprint and expiry the server cannot read itself (RFC §10.3).

| Exit | When |
|---|---|
| 0 | Signed, verified, outputs written |
| 3 | Refused: package or version code mismatch, input not a readable zip, archive ceilings exceeded, duplicate entry names, unreadable manifest, non-numeric SDK level |
| 4 | Refused: the input already carries a signature |
| 5 | Signing failed: wrong store or key password, unknown alias, unsupported key algorithm, `jarsigner` error |
| 6 | Verification failed: the signed bundle does not verify as required, or its signer is not the keystore certificate |
| 7 | `bundle.json` missing or malformed, `DISTRIBUTION_IDENTIFIER` or `DISTRIBUTION_VERSION_CODE` missing or malformed, or the input bundle missing. The bundle is deleted in every case |
| 130, 143 | Interrupted (`SIGINT`, `SIGTERM`) |

## Inspect: `inspect.sh [file]`

| Input | Default | |
|---|---|---|
| positional argument | | Artifact path; wins over everything below |
| `DISTRIBUTION_INPUT` | `$workspace/input/artifact` | Artifact path |
| `DISTRIBUTION_ARTIFACT_TYPE` | required | `aab` or `ipa` |
| `DISTRIBUTION_TRUST` | `/usr/local/distribution/certificates/trust.pem` | PEM bundle a provisioning profile must chain to |
| `MAX_ENTRIES` | `100000` | Entry-count ceiling, checked from the end of central directory record before the directory is parsed |
| `MAX_UNCOMPRESSED_BYTES` | `16000000000` | Ceiling on the sum of declared uncompressed sizes |
| `MAX_MANIFEST_BYTES` | `8388608` | Ceiling on the declared size of `base/manifest/AndroidManifest.xml` |

Nothing is extracted to disk. Duplicate entry names are rejected. Every single-entry read in `inspect-ipa.py` is bounded (1 MiB for `Info.plist` and the profile, 1 MiB of load commands, 64 MiB of code signature), and the executable is streamed, never read whole.

- **Bundle** (`inspect-aab.sh`): the manifest through `bundletool dump manifest --xpath`. A bundle with signature entries must pass `jarsigner -verify` with no unsigned entries; its `fingerprint` is the signer certificate's SHA-256.
- **IPA** (`inspect-ipa.py`): `Info.plist` (binary or XML) from the single `Payload/*.app`, then the first slice of the executable's `LC_CODE_SIGNATURE`:
  - the CodeDirectory gives the ad hoc flag and the team; the entitlements blob gives `entitlements`;
  - a signature that is not ad hoc must carry a CMS blob that verifies over the CodeDirectory (`openssl cms -verify -noverify`, signature only); `fingerprint` is the SHA-256 of its signer certificate;
  - `embedded.mobileprovision` must verify with `openssl cms -verify -CAfile $DISTRIBUTION_TRUST -purpose any`; `profileExpiresAt` and `teamId` come from it;
  - for a signature that is not ad hoc, the profile's `TeamIdentifier` must equal the CodeDirectory team, and the signer certificate must be one of the profile's `DeveloperCertificates`;
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
| 2 | Malformed or over a ceiling: not a zip, too many entries, too large, duplicate names, missing manifest, `Info.plist` or executable, not a Mach-O file |
| 3 | Invalid: a bundle signature that does not verify or covers only some entries; an IPA profile that does not verify against the trust bundle, a CMS signature that does not verify over the CodeDirectory, a team mismatch, a signer missing from the profile |
| 4 | `DISTRIBUTION_ARTIFACT_TYPE` not `aab` or `ipa`, or the input missing |
