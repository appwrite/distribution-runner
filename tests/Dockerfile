ARG TOOLS_IMAGE=appwrite/distribution-tools:test
ARG BATS_IMAGE=bats/bats:1.14.0@sha256:5322b877351fda0cc435de8c6116de7d0a2ec79d7c680132a0ef329a633bc66f

FROM ${BATS_IMAGE} AS bats

FROM ${TOOLS_IMAGE}

COPY --from=bats /opt/bats/bin /opt/bats/bin
COPY --from=bats /opt/bats/lib /opt/bats/lib
COPY --from=bats /opt/bats/libexec /opt/bats/libexec

RUN /opt/bats/bin/bats --version \
    && test -d /usr/local/distribution/test \
    && useradd --create-home --uid 10001 --user-group tester

USER 10001:10001
WORKDIR /home/tester

ENTRYPOINT ["/opt/bats/bin/bats", "--timing"]
