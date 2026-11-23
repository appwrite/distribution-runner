#!/bin/sh
# usage: archive-ios.sh <workspace> <scheme> <archive-path>   (run from anywhere; absolute paths)
# VM 1 of _APP_DISTRIBUTION_IOS_SIGNING=split (RFC §9.4, Appendix M), not installed in the golden image yet: archives
# with code signing disabled. DISTRIBUTION_VERSION_NAME and DISTRIBUTION_VERSION_CODE set the version when not empty.
set -eu
if [ "$#" -ne 3 ]; then
  printf 'usage: %s <workspace> <scheme> <archive-path>\n' "$0" >&2
  exit 2
fi
set -- "$@" CODE_SIGNING_ALLOWED=NO CODE_SIGNING_REQUIRED=NO CODE_SIGN_IDENTITY=
if [ -n "${DISTRIBUTION_VERSION_NAME:-}" ]; then
  set -- "$@" "MARKETING_VERSION=$DISTRIBUTION_VERSION_NAME"
fi
if [ -n "${DISTRIBUTION_VERSION_CODE:-}" ]; then
  set -- "$@" "CURRENT_PROJECT_VERSION=$DISTRIBUTION_VERSION_CODE"
fi
workspace=$1
scheme=$2
archive=$3
shift 3
xcodebuild archive \
  -workspace "$workspace" -scheme "$scheme" -configuration Release \
  -destination 'generic/platform=iOS' -archivePath "$archive" "$@"
