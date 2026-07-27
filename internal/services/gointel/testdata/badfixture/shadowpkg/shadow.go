// Package shadowpkg holds a shadowed err. It type-checks cleanly and the
// DEFAULT vet passes have nothing to say about it — only the opt-in shadow pass
// does, which is the point of the fixture.
package shadowpkg

import "errors"

func first() error  { return nil }
func second() error { return errors.New("second") }

// Shadowed re-declares err inside the if-block and then returns the OUTER err,
// so the inner failure is silently dropped.
func Shadowed() error {
	err := first()
	if err == nil {
		err := second()
		_ = err
	}
	return err
}
