package libevents

import (
	"fmt"
	"regexp"
)

// Config names the identifiers this package interpolates into SQL. Values are
// validated by Validate before any DDL or statement is built; everything else
// in a statement is a bound parameter.
type Config struct {
	// TablePrefix prefixes every table this package owns:
	// {prefix}cursors, {prefix}firings, {prefix}listeners,
	// {prefix}listener_topics, {prefix}staging.
	TablePrefix string
	// ScopeColumn is the tenancy column present on every table — the
	// importer's own dimension (a workspace, an account). The column exists
	// even for importers that pass empty scope values, so the schema never
	// depends on how it is used.
	ScopeColumn string
}

// identRx admits lower_snake SQL identifiers and nothing else — the guard that
// keeps interpolated table and column names from carrying SQL.
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
