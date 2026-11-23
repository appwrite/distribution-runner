# Test certificate authority: TEST ONLY

**Never trust these files in production.** `ca.key` is public on purpose.

These files exist only in the `test` stage of the tools image (`docker build --target test`), which is never published. The release image has no `/usr/local/distribution/test` and trusts only Apple's roots.

| File | Purpose |
|---|---|
| `ca.pem` | Certificate of `Appwrite Distribution Test Only CA` (RSA 2048, valid until 2046). The `test` stage appends it to `/usr/local/distribution/certificates/trust.pem` |
| `ca.key` | Its private key (PKCS#8 PEM, unencrypted). The server's mock Apple store reads it from this path at a pinned commit to sign provisioning profiles |
| `mint-ipa.py` | Mints a synthetic signed `.ipa` chained to this CA |

Both PEM files start with a `TEST ONLY` line before the `-----BEGIN` marker. OpenSSL and PHP's `openssl_*` functions skip text outside the markers.

## `mint-ipa.py`

Python standard library plus the `openssl` CLI (OpenSSL 3 or later). It writes:

- `Payload/<name>.app/Info.plist`, a binary property list;
- `Payload/<name>.app/embedded.mobileprovision`, an attached CMS signature over an XML profile, signed by a profile-signing certificate issued by the CA. `DeveloperCertificates` holds the code signing certificate, `TeamIdentifier` the profile team;
- `Payload/<name>.app/<name>`, a thin arm64 Mach-O whose `LC_CODE_SIGNATURE` points to an embedded signature superblob with a SHA-256 CodeDirectory (version `0x20400`, carrying the team), an empty requirements set, an entitlements blob and a CMS blob (`0xFADE0B01`) holding a detached signature over the CodeDirectory by a distribution certificate issued by the CA;
- `Payload/<name>.app/_CodeSignature/CodeResources`.

Fresh keys are generated on every run, so two runs never produce the same file.

```sh
docker run --rm -v "$out:/out" appwrite/distribution-tools:test \
  python3 -I /usr/local/distribution/test/mint-ipa.py \
  --identifier io.appwrite.example --version-name 1.0 --version-code 1 --team ABCDE12345 \
  --leaf-out /out/leaf.pem --out /out/app.ipa
```

| Flag | Default | Effect |
|---|---|---|
| `--identifier` | required | `CFBundleIdentifier`; also the CodeDirectory identifier and the entitlements' `application-identifier` suffix |
| `--version-name` | `1.0` | `CFBundleShortVersionString` |
| `--version-code` | `1` | `CFBundleVersion` |
| `--team` | `ABCDE12345` | Team in the CodeDirectory, the code signing certificate's `OU` and the binary's entitlements |
| `--profile-team` | `--team` | `TeamIdentifier` of the profile. A different value makes `inspect.sh` exit 3 |
| `--name` | `App` | Executable and `.app` name |
| `--adhoc` | off | Ad hoc signature: CodeDirectory flag `0x2`, no team, an empty CMS blob. `inspect.sh` reports `signed: false` |
| `--no-profile` | off | Omits `embedded.mobileprovision`. `inspect.sh` reports `signed: false` |
| `--expires` | one year from now | Profile `ExpirationDate`, ISO 8601 (`2027-06-30T12:00:00Z`) |
| `--ca-cert`, `--ca-key` | `ca.pem`, `ca.key` next to the script | Issuing CA |
| `--leaf-out` | none | Writes the code signing certificate (PEM). The SHA-256 of its DER is `inspect.sh`'s `fingerprint` |
| `--out` | required | Where the `.ipa` goes; written under a temporary name and renamed |

## Regenerating the CA

Only if it is compromised beyond being public, or before it expires in 2046. The server's mock Apple store must then move to the new commit.

```sh
openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 7305 -keyout key.pem -out certificate.pem \
  -subj '/C=US/O=Appwrite Distribution TEST ONLY/CN=Appwrite Distribution Test Only CA' \
  -addext 'basicConstraints=critical,CA:TRUE' -addext 'keyUsage=critical,keyCertSign,cRLSign' \
  -addext 'subjectKeyIdentifier=hash'
```

Then prepend the `TEST ONLY` lines to each file and save them as `ca.pem` and `ca.key` (`openssl pkey -in key.pem` for PKCS#8).
