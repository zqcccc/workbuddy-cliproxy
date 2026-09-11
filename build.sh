#!/usr/bin/env bash
# Build the workbuddy plugin for linux/amd64, once per realm.
#
# The host derives a plugin's id from its file name, and a plugin can register
# only one auth provider identifier. So CN and Global ship as two files and
# show up as two providers, each with its own OAuth button:
# workbuddy.so (cn) and workbuddy-global.so (global).
#
# Native CGO cross-compilation does not work on macOS, so run this inside the
# golang image (see README):
#   docker run --rm --platform linux/amd64 -v "$PWD":/src -w /src \
#     golang:1.26-bookworm bash -c 'export PATH=$PATH:/usr/local/go/bin && ./build.sh'
set -euo pipefail

export CGO_ENABLED=1 GOOS=linux GOARCH=amd64

# The realms are selected with a build tag rather than -ldflags -X, which does
# not reliably reach the symbol table of a -buildmode=c-shared binary.
go build -buildmode=c-shared -o workbuddy.so .
echo "built workbuddy.so (provider=workbuddy region=cn)"

go build -tags global -buildmode=c-shared -o workbuddy-global.so .
echo "built workbuddy-global.so (provider=workbuddy-global region=global)"
