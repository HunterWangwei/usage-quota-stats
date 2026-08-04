#!/usr/bin/env sh
set -eu
cd "$(dirname "$0")"
mkdir -p dist/linux/amd64
go test ./...
go build -buildmode=c-shared -o dist/linux/amd64/usage-quota-stats.so .
rm -f dist/linux/amd64/usage-quota-stats.h
