package libdbexec

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrInvalidSQLiteOptions reports a SQLiteOptions value that is rejected at
// construction.
var ErrInvalidSQLiteOptions = errors.New("libdb: invalid sqlite options")

// SQLiteJournalMode selects PRAGMA journal_mode. The zero value selects WAL.
type SQLiteJournalMode string

// SQLite journal modes. SQLiteJournalDefault resolves to SQLiteJournalWAL.
const (
	SQLiteJournalDefault  SQLiteJournalMode = ""
	SQLiteJournalWAL      SQLiteJournalMode = "WAL"
	SQLiteJournalDelete   SQLiteJournalMode = "DELETE"
	SQLiteJournalTruncate SQLiteJournalMode = "TRUNCATE"
	SQLiteJournalPersist  SQLiteJournalMode = "PERSIST"
	SQLiteJournalMemory   SQLiteJournalMode = "MEMORY"
	SQLiteJournalOff      SQLiteJournalMode = "OFF"
)

// SQLiteSynchronous selects PRAGMA synchronous. The zero value emits no pragma
// at all, leaving the driver default (FULL) in place.
type SQLiteSynchronous string

// SQLite synchronous levels. SQLiteSynchronousDefault emits no pragma;
// SQLiteSynchronousNormal and SQLiteSynchronousOff require WAL journal mode.
const (
	SQLiteSynchronousDefault SQLiteSynchronous = ""
	SQLiteSynchronousOff     SQLiteSynchronous = "OFF"
	SQLiteSynchronousNormal  SQLiteSynchronous = "NORMAL"
	SQLiteSynchronousFull    SQLiteSynchronous = "FULL"
	SQLiteSynchronousExtra   SQLiteSynchronous = "EXTRA"
)

const defaultSQLiteBusyTimeout = 5000 * time.Millisecond

// SQLiteOptions tunes a SQLite DBManager. The zero value is WAL journal mode, a
// 5000ms busy timeout, foreign keys enforced, no synchronous or cache_size
// pragma, and Go's default connection pool. Pointer fields distinguish unset
// (nil) from an explicit zero.
type SQLiteOptions struct {
	// JournalMode selects PRAGMA journal_mode. The zero value selects WAL.
	JournalMode SQLiteJournalMode

	// Synchronous selects PRAGMA synchronous. The zero value emits no pragma
	// and leaves SQLite's FULL default.
	Synchronous SQLiteSynchronous

	// BusyTimeout sets PRAGMA busy_timeout. Nil means the 5000ms default; a
	// pointer to zero fails contended writes immediately.
	BusyTimeout *time.Duration

	// DisableForeignKeys turns off PRAGMA foreign_keys. The zero value keeps
	// foreign keys enforced.
	DisableForeignKeys bool

	// CacheSize sets PRAGMA cache_size. Nil emits no pragma. Negative is a size
	// in KiB and positive a page count, so prefer SQLiteCacheKiB or
	// SQLiteCachePages over a bare literal.
	CacheSize *int

	// MaxOpenConns caps total open connections via sql.DB.SetMaxOpenConns. Nil
	// leaves the pool unbounded.
	MaxOpenConns *int

	// MaxIdleConns caps retained idle connections via sql.DB.SetMaxIdleConns.
	// Nil leaves Go's default of 2.
	MaxIdleConns *int

	// ConnMaxLifetime bounds how long a connection may be reused via
	// sql.DB.SetConnMaxLifetime. The zero value means unlimited.
	ConnMaxLifetime time.Duration

	// ConnMaxIdleTime bounds how long a connection may sit idle via
	// sql.DB.SetConnMaxIdleTime. The zero value means unlimited.
	ConnMaxIdleTime time.Duration
}

// SQLiteCacheKiB returns a CacheSize requesting approximately kib kibibytes of
// page cache. The argument is a magnitude; its sign is normalised away.
func SQLiteCacheKiB(kib int) *int {
	v := -abs(kib)
	return &v
}

// SQLiteCachePages returns a CacheSize requesting that many cache pages. The
// argument is a magnitude; its sign is normalised away.
func SQLiteCachePages(pages int) *int {
	v := abs(pages)
	return &v
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// SQLiteConns returns a pointer to n for the pool limit fields.
func SQLiteConns(n int) *int {
	return &n
}

// SQLiteBusyTimeout returns a pointer to d for SQLiteOptions.BusyTimeout.
func SQLiteBusyTimeout(d time.Duration) *time.Duration {
	return &d
}

var validSQLiteJournalModes = map[SQLiteJournalMode]bool{
	SQLiteJournalWAL:      true,
	SQLiteJournalDelete:   true,
	SQLiteJournalTruncate: true,
	SQLiteJournalPersist:  true,
	SQLiteJournalMemory:   true,
	SQLiteJournalOff:      true,
}

var validSQLiteSynchronous = map[SQLiteSynchronous]bool{
	SQLiteSynchronousOff:    true,
	SQLiteSynchronousNormal: true,
	SQLiteSynchronousFull:   true,
	SQLiteSynchronousExtra:  true,
}

func (o SQLiteOptions) journalMode() SQLiteJournalMode {
	if o.JournalMode == SQLiteJournalDefault {
		return SQLiteJournalWAL
	}
	return o.JournalMode
}

func (o SQLiteOptions) busyTimeout() time.Duration {
	if o.BusyTimeout == nil {
		return defaultSQLiteBusyTimeout
	}
	return *o.BusyTimeout
}

// validate rejects malformed and unsafe option combinations before any
// connection is opened. SQLite silently ignores unrecognised pragma values, so
// these checks are the only place a typo can be caught.
func (o SQLiteOptions) validate() error {
	if o.JournalMode != SQLiteJournalDefault && !validSQLiteJournalModes[o.JournalMode] {
		return fmt.Errorf("%w: unknown journal mode %q", ErrInvalidSQLiteOptions, o.JournalMode)
	}
	if o.Synchronous != SQLiteSynchronousDefault && !validSQLiteSynchronous[o.Synchronous] {
		return fmt.Errorf("%w: unknown synchronous level %q", ErrInvalidSQLiteOptions, o.Synchronous)
	}
	if o.Synchronous == SQLiteSynchronousNormal || o.Synchronous == SQLiteSynchronousOff {
		if mode := o.journalMode(); mode != SQLiteJournalWAL {
			return fmt.Errorf("%w: synchronous=%s requires journal_mode=WAL, got %s: reduced synchronous levels risk corruption in rollback journal modes",
				ErrInvalidSQLiteOptions, o.Synchronous, mode)
		}
	}
	if o.BusyTimeout != nil {
		d := *o.BusyTimeout
		if d < 0 {
			return fmt.Errorf("%w: busy timeout %s is negative", ErrInvalidSQLiteOptions, d)
		}
		if d > 0 && d.Milliseconds() == 0 {
			return fmt.Errorf("%w: busy timeout %s rounds to 0ms, which disables waiting entirely", ErrInvalidSQLiteOptions, d)
		}
	}
	if o.CacheSize != nil && *o.CacheSize == 0 {
		return fmt.Errorf("%w: cache size 0 disables the page cache; leave CacheSize nil to keep SQLite's default", ErrInvalidSQLiteOptions)
	}
	if o.MaxOpenConns != nil && *o.MaxOpenConns < 0 {
		return fmt.Errorf("%w: max open conns %d is negative", ErrInvalidSQLiteOptions, *o.MaxOpenConns)
	}
	if o.MaxIdleConns != nil && *o.MaxIdleConns < 0 {
		return fmt.Errorf("%w: max idle conns %d is negative", ErrInvalidSQLiteOptions, *o.MaxIdleConns)
	}
	if o.ConnMaxLifetime < 0 {
		return fmt.Errorf("%w: conn max lifetime %s is negative", ErrInvalidSQLiteOptions, o.ConnMaxLifetime)
	}
	if o.ConnMaxIdleTime < 0 {
		return fmt.Errorf("%w: conn max idle time %s is negative", ErrInvalidSQLiteOptions, o.ConnMaxIdleTime)
	}
	return nil
}

// buildSQLiteDSN appends the pragma query parameters for opts to path, so that
// every pooled connection inherits them rather than one leased connection.
func buildSQLiteDSN(path string, opts SQLiteOptions) string {
	dsn := path
	if !strings.Contains(dsn, "?") {
		dsn += "?"
	} else {
		dsn += "&"
	}
	pragmas := []string{
		fmt.Sprintf("journal_mode(%s)", opts.journalMode()),
		fmt.Sprintf("busy_timeout(%d)", opts.busyTimeout().Milliseconds()),
		fmt.Sprintf("foreign_keys(%d)", boolPragma(!opts.DisableForeignKeys)),
	}
	if opts.CacheSize != nil {
		pragmas = append(pragmas, fmt.Sprintf("cache_size(%d)", *opts.CacheSize))
	}
	if opts.Synchronous != SQLiteSynchronousDefault {
		pragmas = append(pragmas, fmt.Sprintf("synchronous(%s)", opts.Synchronous))
	}
	for i, p := range pragmas {
		if i > 0 {
			dsn += "&"
		}
		dsn += "_pragma=" + p
	}
	return dsn
}

func boolPragma(v bool) int {
	if v {
		return 1
	}
	return 0
}

// applySQLitePool applies the pool limits in opts to db, skipping unset limits.
func applySQLitePool(db *sql.DB, opts SQLiteOptions) {
	if opts.MaxOpenConns != nil {
		db.SetMaxOpenConns(*opts.MaxOpenConns)
	}
	if opts.MaxIdleConns != nil {
		db.SetMaxIdleConns(*opts.MaxIdleConns)
	}
	if opts.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(opts.ConnMaxLifetime)
	}
	if opts.ConnMaxIdleTime > 0 {
		db.SetConnMaxIdleTime(opts.ConnMaxIdleTime)
	}
}
