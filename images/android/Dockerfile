FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2

ARG VERSION=dev

ARG JAVA_17_URL=https://github.com/adoptium/temurin17-binaries/releases/download/jdk-17.0.21.1%2B2/OpenJDK17U-jdk_x64_linux_hotspot_17.0.21.1_2.tar.gz
ARG JAVA_17_SHA256=0b0ac5eab6db6abaec68c9913bcddb31321e5da411c45073d520d9c125e6f7b4
ARG JAVA_21_URL=https://github.com/adoptium/temurin21-binaries/releases/download/jdk-21.0.13.1%2B2/OpenJDK21U-jdk_x64_linux_hotspot_21.0.13.1_2.tar.gz
ARG JAVA_21_SHA256=bb8a93f9ae6e7a693b2b721e85dbeff26ae1a77447985a8d7afcdd9c7cf6222e
ARG CMDLINE_TOOLS_URL=https://dl.google.com/android/repository/commandlinetools-linux-15859902_latest.zip
ARG CMDLINE_TOOLS_SHA256=4e4c464f145a7512b57d088ac6c278c03c9eea610886b35a5e0804e74eedf583
ARG NODE_URL=https://nodejs.org/dist/v26.13.1/node-v26.13.1-linux-x64.tar.xz
ARG NODE_SHA256=5c063e46bab0fbc1c17cc445d7ff129cbdcd0471be6ed83b6ab864f8143bc864
ARG FLUTTER_URL=https://storage.googleapis.com/flutter_infra_release/releases/stable/linux/flutter_linux_3.41.2-stable.tar.xz
ARG FLUTTER_SHA256=4a04f8a6152986d14fc137ffaf98106ca743c0f9ab66f1bc2f20ee84eb573e5c
ARG BUNDLETOOL_URL=https://github.com/google/bundletool/releases/download/1.18.3/bundletool-all-1.18.3.jar
ARG BUNDLETOOL_SHA256=a099cfa1543f55593bc2ed16a70a7c67fe54b1747bb7301f37fdfd6d91028e29

ENV DISTRIBUTION_IMAGE=appwrite/distribution-android:${VERSION} \
    FASTLANE_VERSION=2.242.0 \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    JAVA_17_HOME=/opt/java/17 \
    JAVA_21_HOME=/opt/java/21 \
    JAVA_HOME=/opt/java/21 \
    ANDROID_HOME=/opt/android-sdk \
    ANDROID_SDK_ROOT=/opt/android-sdk \
    FLUTTER_ROOT=/opt/flutter \
    PATH=/opt/java/21/bin:/opt/android-sdk/cmdline-tools/latest/bin:/opt/android-sdk/platform-tools:/opt/node/bin:/opt/flutter/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN apt-get update \
    && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        file \
        git \
        libatomic1 \
        python3 \
        ruby \
        squashfs-tools \
        unzip \
        xz-utils \
        zip \
    && rm -rf /var/lib/apt/lists/* \
    && git config --system --add safe.directory '*'

RUN curl -fsSL --retry 3 -o /tmp/java-17.tar.gz "$JAVA_17_URL" \
    && echo "$JAVA_17_SHA256  /tmp/java-17.tar.gz" | sha256sum -c - \
    && curl -fsSL --retry 3 -o /tmp/java-21.tar.gz "$JAVA_21_URL" \
    && echo "$JAVA_21_SHA256  /tmp/java-21.tar.gz" | sha256sum -c - \
    && mkdir -p "$JAVA_17_HOME" "$JAVA_21_HOME" \
    && tar -xzf /tmp/java-17.tar.gz -C "$JAVA_17_HOME" --strip-components=1 \
    && tar -xzf /tmp/java-21.tar.gz -C "$JAVA_21_HOME" --strip-components=1 \
    && rm -f /tmp/java-17.tar.gz /tmp/java-21.tar.gz "$JAVA_17_HOME/lib/src.zip" "$JAVA_21_HOME/lib/src.zip" \
    && "$JAVA_17_HOME/bin/java" -version \
    && "$JAVA_21_HOME/bin/java" -version

RUN curl -fsSL --retry 3 -o /tmp/cmdline-tools.zip "$CMDLINE_TOOLS_URL" \
    && echo "$CMDLINE_TOOLS_SHA256  /tmp/cmdline-tools.zip" | sha256sum -c - \
    && mkdir -p "$ANDROID_HOME/cmdline-tools" \
    && unzip -q /tmp/cmdline-tools.zip -d /tmp/cmdline-tools \
    && mv /tmp/cmdline-tools/cmdline-tools "$ANDROID_HOME/cmdline-tools/latest" \
    && rm -rf /tmp/cmdline-tools /tmp/cmdline-tools.zip \
    && (yes || true) | sdkmanager --licenses > /dev/null \
    && test -s "$ANDROID_HOME/licenses/android-sdk-license" \
    && sdkmanager --install \
        'platform-tools' \
        'platforms;android-36' \
        'platforms;android-37.0' \
        'build-tools;35.0.0' \
        'build-tools;37.0.0' \
        'ndk;27.1.12297006' \
        'ndk;28.2.13676358' \
        > /dev/null \
    && sdkmanager --list_installed \
        | awk -F '|' 'NF == 4 { gsub(/ /, "", $1); gsub(/ /, "", $2); print $1 "=" $2 }' \
        | sort > /tmp/installed \
    && printf '%s\n' \
        'build-tools;35.0.0=35.0.0' \
        'build-tools;37.0.0=37.0.0' \
        'ndk;27.1.12297006=27.1.12297006' \
        'ndk;28.2.13676358=28.2.13676358' \
        'platform-tools=37.0.3' \
        'platforms;android-36=2' \
        'platforms;android-37.0=2' \
        | sort > /tmp/expected \
    && comm -23 /tmp/expected /tmp/installed > /tmp/missing \
    && if [ -s /tmp/missing ]; then echo 'sdk packages differ from the pins:'; cat /tmp/missing /tmp/installed; exit 1; fi \
    && rm -f /tmp/expected /tmp/installed /tmp/missing

RUN curl -fsSL --retry 3 -o /tmp/node.tar.xz "$NODE_URL" \
    && echo "$NODE_SHA256  /tmp/node.tar.xz" | sha256sum -c - \
    && mkdir -p /opt/node \
    && tar -xJf /tmp/node.tar.xz -C /opt/node --strip-components=1 \
    && rm -f /tmp/node.tar.xz \
    && node --version \
    && npm --version

RUN curl -fsSL --retry 3 -o /tmp/flutter.tar.xz "$FLUTTER_URL" \
    && echo "$FLUTTER_SHA256  /tmp/flutter.tar.xz" | sha256sum -c - \
    && tar -xJf /tmp/flutter.tar.xz -C /opt \
    && rm -f /tmp/flutter.tar.xz \
    && flutter config --no-analytics \
    && dart --disable-analytics \
    && flutter precache --android \
    && flutter --version

RUN mkdir -p /opt/bundletool \
    && curl -fsSL --retry 3 -o /opt/bundletool/bundletool-all.jar "$BUNDLETOOL_URL" \
    && echo "$BUNDLETOOL_SHA256  /opt/bundletool/bundletool-all.jar" | sha256sum -c - \
    && printf '#!/bin/sh\nexec java -jar /opt/bundletool/bundletool-all.jar "$@"\n' > /usr/local/bin/bundletool \
    && chmod 0755 /usr/local/bin/bundletool \
    && bundletool version

WORKDIR /usr/local/distribution

COPY Gemfile Gemfile.lock ./

RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential ruby-dev \
    && ln -s /usr/bin/bundle3.3 /usr/local/bin/bundle \
    && BUNDLE_FROZEN=true BUNDLE_SILENCE_ROOT_WARNING=1 bundle install --jobs 4 --retry 3 \
    && apt-get purge -y --auto-remove build-essential ruby-dev \
    && rm -rf /var/lib/apt/lists/* /root/.bundle /root/.cache \
    && FASTLANE_SKIP_UPDATE_CHECK=1 FASTLANE_OPT_OUT_USAGE=1 bundle exec fastlane --version | grep -F "fastlane $FASTLANE_VERSION"

COPY fastlane/Fastfile fastlane/Fastfile
COPY distribution.init.gradle build.sh ./

CMD ["/usr/local/distribution/build.sh"]
