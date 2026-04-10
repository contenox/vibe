// Package openapidocs serves the OpenAPI specification and API browser embedded in the binary.
//
// openapi.json is produced next to this package by `make docs-gen` (copy of docs/openapi.json).
// It is gitignored — use Makefile targets (`make build-cli`, `make test-unit`, etc.) or `make docs-gen`
// before a bare `go build` / `go test` that loads this package.
package openapidocs

import _ "embed"

//go:embed openapi.json
var specJSON []byte

//go:embed rapidoc.html
var rapidocHTML []byte
