#!/bin/sh
# Places the pinned cua-driver release in OUTDIR under the names the server
# serves (internal/app/api/install.go). Every file must match
# scripts/cua-driver.sha256; one mismatch fails the build.
set -eu

OUT=${1:?usage: fetch-cua-driver.sh OUTDIR}
LOCK=$(dirname "$0")/cua-driver.sha256
VERSION=$(awk '$1=="version"{print $2}' "$LOCK")
[ -n "$VERSION" ] || { echo "no version in $LOCK" >&2; exit 1; }
TAG="cua-driver-rs-v$VERSION"
RELEASE="https://github.com/trycua/cua/releases/download/$TAG"

mkdir -p "$OUT"
fetch() {
  ASSET=$1 URL=$2 NAME=$3
  EXPECTED=$(awk -v asset="$ASSET" '$2==asset{print $1}' "$LOCK")
  [ -n "$EXPECTED" ] || { echo "$ASSET is not pinned in $LOCK" >&2; exit 1; }
  wget -q -O "$OUT/$NAME.partial" "$URL"
  ACTUAL=$(sha256sum "$OUT/$NAME.partial" | awk '{print $1}')
  if [ "$ACTUAL" != "$EXPECTED" ]; then
    rm -f "$OUT/$NAME.partial"
    echo "$ASSET: SHA-256 $ACTUAL, pinned $EXPECTED" >&2
    exit 1
  fi
  mv "$OUT/$NAME.partial" "$OUT/$NAME"
}

fetch "cua-driver-rs-$VERSION-darwin-universal.tar.gz" "$RELEASE/cua-driver-rs-$VERSION-darwin-universal.tar.gz" cua-driver-darwin-universal.tar.gz
fetch "cua-driver-rs-$VERSION-windows-x86_64.zip" "$RELEASE/cua-driver-rs-$VERSION-windows-x86_64.zip" cua-driver-windows-x86_64.zip
fetch "cua-driver-rs-$VERSION-windows-arm64.zip" "$RELEASE/cua-driver-rs-$VERSION-windows-arm64.zip" cua-driver-windows-arm64.zip
fetch LICENSE.md "https://raw.githubusercontent.com/trycua/cua/$TAG/LICENSE.md" LICENSE
printf '%s\n' "$VERSION" > "$OUT/VERSION"
echo "cua-driver $VERSION placed in $OUT"
