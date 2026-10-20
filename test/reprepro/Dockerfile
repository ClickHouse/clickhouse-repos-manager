# reprepro 5.5.1-1 built from source on Jammy, as the release runners do it
# (clickhouse-private tests/ci/terraform/worker/prepare-ci-ami.sh), with gpg
# to sign and python3 to serve the repositories to the apt clients.
FROM ubuntu:22.04
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
ARG REPREPRO=reprepro-debian-5.5.1-1
ARG SHA256=d031fca51c3d4184dc3f3fba80348341f54fae130d17e99fa0b02520cc8da02e
# hadolint ignore=DL3008
RUN apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
        build-essential ca-certificates curl db-util debhelper fakeroot gnupg gpg-agent \
        libarchive-dev libbz2-dev libdb-dev libgpgme-dev liblzma-dev shunit2 zlib1g-dev python3 >/dev/null \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /build/src
RUN curl -fsSL -o ../src.tar.gz https://salsa.debian.org/debian/reprepro/-/archive/$REPREPRO/reprepro-$REPREPRO.tar.gz \
    && echo "$SHA256  ../src.tar.gz" | sha256sum -c - \
    && tar xzf ../src.tar.gz --strip-components=1 \
    && DEB_BUILD_OPTIONS=nocheck dpkg-buildpackage -b --no-sign >/dev/null \
    && dpkg -i ../reprepro_*.deb >/dev/null \
    && reprepro --version
WORKDIR /
RUN rm -rf /build
