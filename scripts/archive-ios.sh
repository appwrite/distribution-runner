#!/bin/sh
# usage: archive-ios.sh <workspace> <scheme> <archive-path>   (run from anywhere; absolute paths)
# Archives with code signing disabled; version comes from the environment.
set -eu
xcodebuild archive \
  -workspace "$1" -scheme "$2" -configuration Release \
  -destination 'generic/platform=iOS' -archivePath "$3" \
  CODE_SIGNING_ALLOWED=NO CODE_SIGNING_REQUIRED=NO CODE_SIGN_IDENTITY="" \
  MARKETING_VERSION="$DISTRIBUTION_VERSION_NAME" CURRENT_PROJECT_VERSION="$DISTRIBUTION_VERSION_CODE"
