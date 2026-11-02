#!/bin/sh
# goreleaser `gobinary` for the neutral build: `go build ...` becomes
# `garble -literals -tiny build ...`. garble is installed by CI at a pinned
# version with the same Go as go.mod (garble refuses a newer toolchain).
PATH="$(go env GOPATH)/bin:$PATH"
export PATH
exec garble -literals -tiny "$@"
