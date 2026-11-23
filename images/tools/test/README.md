# Test certificate authority: TEST ONLY

**Never trust these files in production.** Both keys are public on purpose.

These files exist only in the `test` stage of the tools image (`docker build --target test`), which is never published. The release image has no `/usr/local/distribution/test`, trusts only Apple's roots and ignores `DISTRIBUTION_TRUST`.

| File | Purpose |
|---|---|
| `ca.pem` | Certificate of `Appwrite Distribution Test Only CA` (RSA 2048, valid until 2046). The `test` stage appends it to `/usr/local/distribution/certificates/trust.pem` |
| `ca.key` | Its private key (PKCS#8 PEM, unencrypted). `mint-ipa.py` issues the code signing certificates with it |
| `profile-ca.pem` | A test intermediate issued by the test CA with the subject of Apple's real profile intermediate, `C=US,O=Apple Inc.,OU=Certification Authority,CN=Apple iPhone Certification Authority` (valid until 2046). Its key was discarded |
| `profile-signer.pem` | A test certificate issued by `profile-ca.pem` with the subject of Apple's real profile signer, `C=US,O=Apple Inc.,CN=Apple iPhone OS Provisioning Profile Signing` (valid until 2046) |
| `profile-signer.key` | Its private key (PKCS#8 PEM, unencrypted) |
| `mint-ipa.py` | Mints a synthetic signed `.ipa` chained to this CA |

`inspect-ipa.py` accepts a provisioning profile only when its single CMS signer has exactly those subject and issuer names, so a profile signed by any other certificate, the test CA included, exits 3.

**Server mock Apple store:** sign provisioning profiles with `profile-signer.pem` and `profile-signer.key` from the pinned commit, and include `profile-ca.pem` in the CMS (`openssl cms -sign -nodetach -binary -signer profile-signer.pem -inkey profile-signer.key -certfile profile-ca.pem`, or `openssl_cms_sign()` with `profile-ca.pem` as the untrusted certificates file). Signing with `ca.key` directly no longer verifies. Code signing certificates may still be issued by `ca.key`.

Every PEM file starts with `TEST ONLY` lines before the `-----BEGIN` marker. OpenSSL and PHP's `openssl_*` functions skip text outside the markers.

## `mint-ipa.py`

Python standard library plus the `openssl` CLI (OpenSSL 3 or later). It writes:

- `Payload/<name>.app/Info.plist`, a binary property list;
- `Payload/<name>.app/embedded.mobileprovision`, an attached CMS signature over an XML profile, signed by `profile-signer.pem` with `profile-ca.pem` included. `DeveloperCertificates` holds the code signing certificate, `TeamIdentifier` and `ApplicationIdentifierPrefix` the profile team, `Entitlements.application-identifier` the profile team and identifier;
- `Payload/<name>.app/<name>`, a thin arm64 Mach-O (optionally inside a universal header) whose `LC_CODE_SIGNATURE` points to an embedded signature superblob with a SHA-256 CodeDirectory (version `0x20400`, carrying the identifier and the team, sealing `Info.plist`, the requirements, `CodeResources`, the entitlements and the DER entitlements in special slots 1, 2, 3, 5 and 7), an empty requirements set, an entitlements blob, a DER entitlements blob and a CMS blob (`0xFADE0B01`) holding a detached signature over the CodeDirectory by a distribution certificate issued by the CA;
- `Payload/<name>.app/_CodeSignature/CodeResources`.

Fresh code signing keys are generated on every run, so two runs never produce the same file.

```sh
docker run --rm -v "$directory:/out" appwrite/distribution-tools:test \
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
| `--profile-identifier` | `--identifier` | Bundle part of the profile's `application-identifier`, may end in `*`. One that does not cover `--identifier` makes `inspect.sh` exit 3 |
| `--code-identifier` | `--identifier` | CodeDirectory identifier. A different value makes `inspect.sh` exit 3 |
| `--name` | `App` | Executable and `.app` name |
| `--adhoc` | off | Ad hoc signature: CodeDirectory flag `0x2`, no team, an empty CMS blob. `inspect.sh` reports `signed: false` |
| `--no-profile` | off | Omits `embedded.mobileprovision`. `inspect.sh` reports `signed: false` |
| `--forge-profile` | off | Signs the profile with the code signing certificate instead of the profile signer. `inspect.sh` exits 3 |
| `--universal` | off | Wraps the executable in a universal (fat) header with one arm64 slice |
| `--expires` | one year from now | Profile `ExpirationDate`, ISO 8601 (`2027-06-30T12:00:00Z`) |
| `--ca-cert`, `--ca-key` | `ca.pem`, `ca.key` next to the script | CA that issues the code signing certificate |
| `--profile-cert`, `--profile-key`, `--profile-chain` | `profile-signer.pem`, `profile-signer.key`, `profile-ca.pem` next to the script | Profile signer and the intermediate included in the profile |
| `--leaf-out` | none | Writes the code signing certificate (PEM). The SHA-256 of its DER is `inspect.sh`'s `fingerprint` |
| `--out` | required | Where the `.ipa` goes; written under a temporary name and renamed |

## Regenerating

Only if a file is compromised beyond being public, or before it expires in 2046. The server's mock Apple store must then move to the new commit.

The CA:

```sh
openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 7305 -keyout key.pem -out certificate.pem \
  -subj '/C=US/O=Appwrite Distribution TEST ONLY/CN=Appwrite Distribution Test Only CA' \
  -addext 'basicConstraints=critical,CA:TRUE' -addext 'keyUsage=critical,keyCertSign,cRLSign' \
  -addext 'subjectKeyIdentifier=hash'
```

Then prepend the `TEST ONLY` lines to each file and save them as `ca.pem` and `ca.key` (`openssl pkey -in key.pem` for PKCS#8).

The profile signer, with OpenSSL 3 (the subject order matters: the names must print exactly as above with `-nameopt RFC2253`):

```sh
openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 7300 -keyout profile-ca.key -out profile-ca.crt \
  -subj '/CN=Apple iPhone Certification Authority/OU=Certification Authority/O=Apple Inc./C=US' \
  -CA ca.pem -CAkey ca.key \
  -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' -addext 'keyUsage=critical,keyCertSign,cRLSign'
openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 7299 -keyout signer.key -out profile-signer.crt \
  -subj '/CN=Apple iPhone OS Provisioning Profile Signing/O=Apple Inc./C=US' \
  -CA profile-ca.crt -CAkey profile-ca.key \
  -addext 'basicConstraints=critical,CA:FALSE' -addext 'keyUsage=critical,digitalSignature'
```

Prepend the `TEST ONLY` lines and save `profile-ca.crt` as `profile-ca.pem`, `profile-signer.crt` as `profile-signer.pem` and `openssl pkey -in signer.key` as `profile-signer.key`; discard `profile-ca.key`. The `test` stage build runs `openssl verify` over the chain.
