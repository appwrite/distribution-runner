#!/bin/sh
set -eu

identifier='io.appwrite.distribution.fixture'
version_name='1.0.0'
version_code='1'
min_sdk='24'
target_sdk='36'

aapt2_version='9.5.2-16666295'
aapt2_url="https://dl.google.com/dl/android/maven2/com/android/tools/build/aapt2/$aapt2_version/aapt2-$aapt2_version-linux.jar"
aapt2_sha256='fa1014cb1431622d292ef55ba870e75882ea19987bec541698b912f446a2d08f'

platform_archive='platform-36_r02.zip'
platform_url="https://dl.google.com/android/repository/$platform_archive"
platform_sha1='2c1a80dd4d9f7d0e6dd336ec603d9b5c55a6f576'
platform_sha256='37607369a28c5b640b3a7998868d45898ebcb777565a0e85f9acf36f29631d2e'

bundletool_version='1.18.3'
bundletool_url="https://github.com/google/bundletool/releases/download/$bundletool_version/bundletool-all-$bundletool_version.jar"
bundletool_sha256='a099cfa1543f55593bc2ed16a70a7c67fe54b1747bb7301f37fdfd6d91028e29'

[ "$(uname -m)" = 'x86_64' ] || { echo "generate.sh must run on linux/amd64, not $(uname -m)" >&2; exit 1; }

here=$(pwd)
work="$here/.work"
rm -rf "$work"
mkdir -p "$work/download" "$work/linked" "$work/module/manifest"
trap 'rm -rf "$work"' EXIT

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends openjdk-21-jdk-headless ca-certificates curl unzip zip >/dev/null

fetch() {
  curl -fsSL --retry 3 -o "$work/download/$1" "$2"
  echo "$3  $work/download/$1" | sha256sum -c -
}

fetch aapt2.jar "$aapt2_url" "$aapt2_sha256"
fetch "$platform_archive" "$platform_url" "$platform_sha256"
echo "$platform_sha1  $work/download/$platform_archive" | sha1sum -c -
fetch bundletool.jar "$bundletool_url" "$bundletool_sha256"

unzip -q -o "$work/download/aapt2.jar" aapt2 -d "$work/download"
chmod +x "$work/download/aapt2"
unzip -q -o -j "$work/download/$platform_archive" android-36/android.jar -d "$work/download"

aapt2="$work/download/aapt2"
bundletool() { java -jar "$work/download/bundletool.jar" "$@"; }

cat > "$work/AndroidManifest.xml" <<XML
<?xml version="1.0" encoding="utf-8"?>
<manifest xmlns:android="http://schemas.android.com/apk/res/android"
    package="$identifier"
    android:versionCode="$version_code"
    android:versionName="$version_name">
    <uses-sdk android:minSdkVersion="$min_sdk" android:targetSdkVersion="$target_sdk" />
    <application android:hasCode="false" />
</manifest>
XML

"$aapt2" link --proto-format \
  -I "$work/download/android.jar" \
  --manifest "$work/AndroidManifest.xml" \
  -o "$work/linked.zip"

unzip -q "$work/linked.zip" -d "$work/linked"
cp "$work/linked/AndroidManifest.xml" "$work/module/manifest/AndroidManifest.xml"
cp "$work/linked/resources.pb" "$work/module/resources.pb"
find "$work/module" -exec touch -h -d '2008-01-01 00:00:00' {} +
(cd "$work/module" && zip -q -X -D -r "$work/base.zip" manifest resources.pb)

bundletool build-bundle --modules="$work/base.zip" --output="$work/first.aab"
bundletool build-bundle --modules="$work/base.zip" --output="$work/second.aab"
cmp "$work/first.aab" "$work/second.aab" || { echo 'bundletool output is not deterministic' >&2; exit 1; }
bundle="$work/first.aab"

if unzip -Z1 "$bundle" | grep -qE '^META-INF/[^/]+\.(SF|RSA|DSA|EC)$'; then
  echo 'generated bundle is signed' >&2
  exit 1
fi

bundletool validate --bundle "$bundle" >/dev/null

expect() {
  actual=$(bundletool dump manifest --bundle "$bundle" --xpath "$1")
  [ "$actual" = "$2" ] || { echo "$1 is '$actual', expected '$2'" >&2; exit 1; }
  echo "ok: $1 = $actual"
}

expect '/manifest/@package' "$identifier"
expect '/manifest/@android:versionCode' "$version_code"
expect '/manifest/@android:versionName' "$version_name"
expect '/manifest/uses-sdk/@android:minSdkVersion' "$min_sdk"
expect '/manifest/uses-sdk/@android:targetSdkVersion' "$target_sdk"
expect '/manifest/application/@android:hasCode' 'false'

java_version=$(java -XshowSettings:properties -version 2>&1 | sed -n 's/^ *java\.runtime\.version = //p')
bundle_sha256=$(sha256sum "$bundle" | cut -d' ' -f1)

cp "$bundle" "$here/app-unsigned.aab"
cat > "$here/provenance.json" <<JSON
{
  "bundle": {
    "file": "app-unsigned.aab",
    "sha256": "$bundle_sha256",
    "identifier": "$identifier",
    "versionName": "$version_name",
    "versionCode": $version_code,
    "minSdk": $min_sdk,
    "targetSdk": $target_sdk
  },
  "generator": {
    "script": "images/fixture/generate.sh",
    "platform": "linux/amd64",
    "java": "$java_version"
  },
  "inputs": {
    "aapt2": {
      "version": "$aapt2_version",
      "url": "$aapt2_url",
      "sha256": "$aapt2_sha256"
    },
    "platform": {
      "package": "platforms;android-36",
      "url": "$platform_url",
      "sha1": "$platform_sha1",
      "sha256": "$platform_sha256"
    },
    "bundletool": {
      "version": "$bundletool_version",
      "url": "$bundletool_url",
      "sha256": "$bundletool_sha256"
    }
  }
}
JSON
chmod 0644 "$here/app-unsigned.aab" "$here/provenance.json"

echo "wrote app-unsigned.aab ($bundle_sha256) and provenance.json"
