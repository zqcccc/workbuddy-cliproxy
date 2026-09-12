#!/usr/bin/env bash
# Build the workbuddy plugin for one target platform, once per realm.
#
# The host derives a plugin's id from its file name, and a plugin can register
# only one auth provider identifier. So CN and Global ship as two files and
# show up as two providers, each with its own OAuth button:
# workbuddy.so (cn) and workbuddy-global.so (global).
#
# Output layout (everything under dist/):
#   dist/workbuddy-<goos>-<goarch>.so          provider=workbuddy        region=cn
#   dist/workbuddy-global-<goos>-<goarch>.so   provider=workbuddy-global region=global
#
# The file names carry the platform because CI builds a matrix; the deploy
# webhook picks the one matching the server and renames it on install.
#
# Native CGO cross-compilation does not work on macOS, so run this inside the
# golang image for anything other than the host platform:
#   docker run --rm --platform linux/amd64 -v "$PWD":/src -w /src \
#     golang:1.26-bookworm bash -c 'export PATH=$PATH:/usr/local/go/bin && ./build.sh'
set -euo pipefail

GOOS_EFF="${GOOS:-$(go env GOOS)}"
GOARCH_EFF="${GOARCH:-$(go env GOARCH)}"
export GOOS="$GOOS_EFF" GOARCH="$GOARCH_EFF"
export CGO_ENABLED=1

# Pick a cross compiler that matches the target. Override with CC=...
case "${GOOS_EFF}-${GOARCH_EFF}" in
  linux-amd64) CC_DEFAULT="gcc" ;;
  linux-arm64) CC_DEFAULT="aarch64-linux-gnu-gcc" ;;
  linux-386)   CC_DEFAULT="i686-linux-gnu-gcc" ;;
  linux-arm)   CC_DEFAULT="arm-linux-gnueabihf-gcc" ;;
  *)           CC_DEFAULT="$(go env CC)" ;;
esac
export CC="${CC:-$CC_DEFAULT}"

if ! command -v "$CC" >/dev/null 2>&1; then
  echo "error: cross compiler '$CC' not found." >&2
  echo "       Debian/Ubuntu: sudo apt-get install -y gcc-aarch64-linux-gnu" >&2
  exit 1
fi

SUFFIX="${GOOS_EFF}-${GOARCH_EFF}"
OUTDIR="${OUTDIR:-dist}"
mkdir -p "$OUTDIR"

# The realms are selected with a build tag rather than -ldflags -X, which does
# not reliably reach the symbol table of a -buildmode=c-shared binary.
go build -buildmode=c-shared -o "$OUTDIR/workbuddy-$SUFFIX.so" .
echo "built $OUTDIR/workbuddy-$SUFFIX.so (provider=workbuddy region=cn)"

go build -tags global -buildmode=c-shared -o "$OUTDIR/workbuddy-global-$SUFFIX.so" .
echo "built $OUTDIR/workbuddy-global-$SUFFIX.so (provider=workbuddy-global region=global)"
