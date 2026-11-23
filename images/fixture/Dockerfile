FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2

RUN apt-get update \
    && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends python3-minimal libpython3-stdlib \
    && rm -rf /var/lib/apt/lists/* \
    && python3 -I -c 'import argparse, json, zipfile, zlib'

COPY --chmod=0755 build.sh /usr/local/distribution/build.sh
COPY --chmod=0644 manifest.py app-unsigned.aab result.json /usr/local/distribution/

ARG VERSION=dev
ENV DISTRIBUTION_IMAGE=appwrite/distribution-fixture:${VERSION}

CMD ["/usr/local/distribution/build.sh"]
