package libevents

import (
	"fmt"
	"regexp"
)

// Config names the SQL identifiers this package interpolates; every other
// value in a statement is bound, not interpolated.
type Config struct {
	// TablePrefix prefixes every table this package owns:
	// {prefix}cursors, {prefix}firings, {prefix}listeners,
	// {prefix}listener_topics, {prefix}staging.
	TablePrefix string
	// ScopeColumn is the tenancy column present on every table.
	ScopeColumn string
}

var identRx = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Validate rejects identifiers that could not be safely interpolated.
func (c Config) Validate() error {
	if !identRx.MatchString(c.TablePrefix) {
		return fmt.Errorf("libevents: invalid table prefix %q", c.TablePrefix)
	}
	if !identRx.MatchString(c.ScopeColumn) {
		return fmt.Errorf("libevents: invalid scope column %q", c.ScopeColumn)
	}
	return nil
}

func (c Config) table(name string) string { return c.TablePrefix + name }
