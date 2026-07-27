// Package typeerr holds one deliberate type error and nothing else, so a
// diagnostics test can assert on the type-error path without the vet passes
// having anything to say about the same package.
package typeerr

// Broken assigns a string to an int. `go build` rejects it; go/types reports it
// as a package error on the load.
func Broken() int {
	var n int = "not an int"
	return n
}
