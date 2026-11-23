FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 AS base

ENV LANG=C.UTF-8 \
    LC_ALL=C.UTF-8

SHELL ["/bin/bash", "-eu", "-o", "pipefail", "-c"]

RUN apt-get update \
    && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        openjdk-21-jdk-headless \
        openssl \
        python3 \
    && rm -rf /var/lib/apt/lists/* \
    && java -version 2>&1 | head -1 | grep -qF '"21.' \
    && command -v jarsigner keytool timeout \
    && python3 -I -c 'import argparse, base64, hashlib, json, plistlib, struct, subprocess, xml.etree.ElementTree, zipfile'

ADD --checksum=sha256:a099cfa1543f55593bc2ed16a70a7c67fe54b1747bb7301f37fdfd6d91028e29 --chmod=0644 \
    https://github.com/google/bundletool/releases/download/1.18.3/bundletool-all-1.18.3.jar \
    /usr/local/share/bundletool/bundletool-all.jar
COPY --chmod=0755 bundletool /usr/local/bin/bundletool
RUN chmod 0755 /usr/local/share/bundletool \
    && test "$(bundletool version)" = 1.18.3

COPY certificates/apple-root-ca.cer certificates/apple-root-ca-g3.cer /usr/local/distribution/certificates/
WORKDIR /usr/local/distribution/certificates
RUN chmod 0755 /usr/local/distribution /usr/local/distribution/certificates \
    && chmod 0644 apple-root-ca.cer apple-root-ca-g3.cer \
    && printf '%s  %s\n' \
        b0b1730ecbc7ff4505142c49f1295e6eda6bcaed7e2c68c5be91b5a11001f024 apple-root-ca.cer \
        63343abfb89a6a03ebb57e9b3f5fa7be7c4f5c756f3017b3a8c488c3653e9179 apple-root-ca-g3.cer \
        | sha256sum -c - \
    && for certificate in apple-root-ca.cer apple-root-ca-g3.cer; do \
        openssl x509 -inform der -in "$certificate"; \
    done > apple.pem \
    && cp apple.pem trust.pem \
    && chmod 0644 apple.pem trust.pem

COPY --chmod=0755 sign-android.sh inspect.sh inspect-aab.sh inspect-ipa.py archive.py bundle.py manifest.py signature.py \
    /usr/local/distribution/
WORKDIR /mnt/code

FROM base AS test

COPY test/ca.pem test/ca.key test/profile-ca.pem test/profile-signer.pem test/profile-signer.key test/mint-ipa.py test/README.md \
    /usr/local/distribution/test/
WORKDIR /usr/local/distribution/test
RUN chmod 0755 . mint-ipa.py \
    && chmod 0644 ca.pem ca.key profile-ca.pem profile-signer.pem profile-signer.key README.md \
    && openssl verify -CAfile ca.pem -untrusted profile-ca.pem profile-signer.pem \
    && cat ../certificates/apple.pem ca.pem > ../certificates/trust.pem
WORKDIR /mnt/code

FROM base AS release
