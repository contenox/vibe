// Package openapidocs serves the OpenAPI specification and API browser embedded in the binary.
//
// openapi.json is produced next to this package by `make docs-gen` (copy of docs/openapi.json).
// Run `make docs-gen` or `make build-cli` / `make test-unit` so the file exists before `go build`.
package openapidocs

import _ "embed"

//go:embed openapi.json
var specJSON []byte

//go:embed rapidoc.html
var rapidocHTML []byte
