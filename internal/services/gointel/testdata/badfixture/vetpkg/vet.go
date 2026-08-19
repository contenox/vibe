// Package vetpkg type-checks cleanly but contains two findings the curated vet
// passes are expected to catch. It is a separate package from typeerr because a
// package that does not type-check has its analyzers skipped by design.
package vetpkg

import "fmt"

// PrintfMistake supplies no argument for the %d verb. The compiler accepts it;
// the printf pass does not.
func PrintfMistake() string {
	return fmt.Sprintf("%d")
}

// Unreachable has a statement after a return. The compiler accepts it; the
// unreachable pass does not.
func Unreachable() int {
	return 1
	fmt.Println("dead")
	return 2
}
