package libdbexec

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrInvalidSQLiteOptions reports a SQLiteOptions value that is rejected at
// construction. It covers both malformed values (an unknown journal mode, a
// negative pool limit) and combinations that are individually valid but unsafe
// together, notably a reduced synchronous level outside WAL journal mode.
var ErrInvalidSQLiteOptions = errors.New("libdb: invalid sqlite options")

// SQLiteJournalMode selects PRAGMA journal_mode. The zero value selects WAL,
// which is the only mode the reduced synchronous levels are corruption-safe in.
type SQLiteJournalMode string

// SQLite journal modes. SQLiteJournalDefault is the zero value and resolves to
// SQLiteJournalWAL.
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

// SQLite synchronous levels. SQLiteSynchronousDefault is the zero value and
// emits no pragma. SQLiteSynchronousNormal and SQLiteSynchronousOff are
// accepted only in WAL journal mode.
const (
	SQLiteSynchronousDefault SQLiteSynchronous = ""
	SQLiteSynchronousOff     SQLiteSynchronous = "OFF"
	SQLiteSynchronousNormal  SQLiteSynchronous = "NORMAL"
	SQLiteSynchronousFull    SQLiteSynchronous = "FULL"
	SQLiteSynchronousExtra   SQLiteSynchronous = "EXTRA"
)

// defaultSQLiteBusyTimeout is the busy timeout applied when
// SQLiteOptions.BusyTimeout is nil. It matches the value NewSQLiteDBManager
// has always used.
const defaultSQLiteBusyTimeout = 5000 * time.Millisecond

// SQLiteOptions tunes a SQLite DBManager.
//
// The zero value reproduces NewSQLiteDBManager byte for byte: WAL journal mode,
// a 5000ms busy timeout, foreign keys enforced, no synchronous or cache_size
// pragma, and Go's default connection pool (unbounded, two idle connections,
// no lifetime cap).
//
// Numeric fields whose zero is a meaningful setting are pointers, so nil means
// "leave alone" and a pointer to zero means "set it to zero". Fields whose zero
// already coincides with the database/sql default (ConnMaxLifetime,
// ConnMaxIdleTime, both meaning unlimited) are plain values, because there is
// no observable difference between unset and zero for them.
type SQLiteOptions struct {
	// JournalMode selects PRAGMA journal_mode. The zero value selects WAL.
	// Journal mode is a persistent property of the database file, so changing
	// it converts the file on open.
	JournalMode SQLiteJournalMode

	// Synchronous selects PRAGMA synchronous. The zero value emits no pragma
	// and leaves SQLite's FULL default. SQLiteSynchronousNormal trades an
	// fsync per commit for durability of the most recent transactions across
	// a power loss; in WAL mode it cannot corrupt the database, which is why
	// it is refused outside WAL.
	Synchronous SQLiteSynchronous

	// BusyTimeout sets PRAGMA busy_timeout, how long SQLite retries a locked
	// database before returning SQLITE_BUSY. Nil means the 5000ms default. A
	// pointer to zero disables waiting and fails contended writes immediately.
	// Sub-millisecond precision is not representable and is refused.
	BusyTimeout *time.Duration

	// DisableForeignKeys turns off PRAGMA foreign_keys. The zero value keeps
	// foreign keys enforced. The sense is inverted so that the zero value
	// matches the enforced default.
	DisableForeignKeys bool

	// CacheSize sets PRAGMA cache_size. Nil emits no pragma. A negative value
	// is a size in KiB, a positive value is a count of pages -- this is
	// SQLite's own sign convention and it is easy to invert, so prefer
	// SQLiteCacheKiB or SQLiteCachePages over a bare literal. A pointer to
	// zero is refused: it would disable the page cache, and it is far more
	// often a zero value leaking through than a deliberate choice.
	CacheSize *int

	// MaxOpenConns caps total open connections via sql.DB.SetMaxOpenConns.
	// Nil leaves the pool unbounded, which is Go's default. A pointer to zero
	// is also unbounded, stated explicitly. See NewSQLiteDBManagerWithOptions
	// for what capping this to 1 costs.
	MaxOpenConns *int

	// MaxIdleConns caps retained idle connections via
	// sql.DB.SetMaxIdleConns. Nil leaves Go's default of 2. A pointer to zero
	// retains none, reopening the file on every acquisition. database/sql
	// silently clamps this to MaxOpenConns when it is larger.
	MaxIdleConns *int

	// ConnMaxLifetime bounds how long a connection may be reused via
	// sql.DB.SetConnMaxLifetime. The zero value means unlimited, matching
	// database/sql, so unset and zero are indistinguishable here.
	ConnMaxLifetime time.Duration

	// ConnMaxIdleTime bounds how long a connection may sit idle via
	// sql.DB.SetConnMaxIdleTime. The zero value means unlimited, matching
	// database/sql, so unset and zero are indistinguishable here.
	ConnMaxIdleTime time.Duration
}

// SQLiteCacheKiB returns a CacheSize requesting approximately kib kibibytes of
// page cache, applying SQLite's negative-means-KiB convention so callers never
// write the sign themselves. The argument is a magnitude: its sign is
// normalised away, so SQLiteCacheKiB cannot silently yield a page count. A zero
// magnitude is refused at construction.
func SQLiteCacheKiB(kib int) *int {
	v := -abs(kib)
	return &v
}

// SQLiteCachePages returns a CacheSize requesting that many cache pages, whose
// byte cost depends on the database page size. The argument is a magnitude: its
// sign is normalised away, so SQLiteCachePages cannot silently yield a size in
// KiB. A zero magnitude is refused at construction.
func SQLiteCachePages(pages int) *int {
	v := abs(pages)
	return &v
}

// abs returns the magnitude of v. math.MinInt has no positive counterpart and
// is returned unchanged, which is harmless here: it is not a cache size anyone
// can mean.
func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// SQLiteConns returns a pointer to n for the pool limit fields, which
// distinguish an unset limit from an explicit zero.
func SQLiteConns(n int) *int {
	return &n
}

// SQLiteBusyTimeout returns a pointer to d for SQLiteOptions.BusyTimeout, which
// distinguishes an unset timeout from an explicit zero.
func SQLiteBusyTimeout(d time.Duration) *time.Duration {
	return &d
}

// validSQLiteJournalModes is the set the driver accepts for journal_mode.
var validSQLiteJournalModes = map[SQLiteJournalMode]bool{
	SQLiteJournalWAL:      true,
	SQLiteJournalDelete:   true,
	SQLiteJournalTruncate: true,
	SQLiteJournalPersist:  true,
	SQLiteJournalMemory:   true,
	SQLiteJournalOff:      true,
}

// validSQLiteSynchronous is the set the driver accepts for synchronous.
var validSQLiteSynchronous = map[SQLiteSynchronous]bool{
	SQLiteSynchronousOff:    true,
	SQLiteSynchronousNormal: true,
	SQLiteSynchronousFull:   true,
	SQLiteSynchronousExtra:  true,
}

// journalMode resolves JournalMode's zero value to the WAL default.
func (o SQLiteOptions) journalMode() SQLiteJournalMode {
	if o.JournalMode == SQLiteJournalDefault {
		return SQLiteJournalWAL
	}
	return o.JournalMode
}

// busyTimeout resolves BusyTimeout's nil to the 5000ms default.
func (o SQLiteOptions) busyTimeout() time.Duration {
	if o.BusyTimeout == nil {
		return defaultSQLiteBusyTimeout
	}
	return *o.BusyTimeout
}

// validate rejects malformed and unsafe option combinations before any
// connection is opened.
//
// The driver executes _pragma values verbatim and SQLite silently ignores a
// pragma value it does not recognise, so a typo like synchronous(NORMLA) opens
// successfully and leaves the setting at its default. Nothing downstream will
// report it; these checks are the only place it can be caught.
//
// The synchronous/journal_mode pairing is the one rule here that is about
// safety rather than typos. A synchronous level below FULL is free of
// corruption risk only under WAL: the rollback journal modes depend on the
// fsync that NORMAL and OFF skip to order journal writes against database
// writes, so an ill-timed power loss leaves the file unrecoverable rather than
// merely dropping recent commits. Under WAL the same relaxation costs at most
// the last few transactions, which is why it is allowed there and refused
// everywhere else.
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

// buildSQLiteDSN appends the pragma query parameters for opts to path.
//
// Set via DSN pragmas, not a one-off ExecContext, so every pooled connection
// inherits WAL mode, busy timeout, and foreign_keys — not just one leased
// connection.
//
// The emitted order is journal_mode, busy_timeout, foreign_keys, then the
// optional extras, so a zero-valued opts reproduces the original DSN string
// exactly. Order is cosmetic to the driver, which sorts _pragma values itself
// (busy_timeout first, then lexicographically) before executing them; that sort
// is also what guarantees journal_mode is applied before synchronous.
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

// boolPragma renders a pragma boolean as SQLite's 1 or 0.
func boolPragma(v bool) int {
	if v {
		return 1
	}
	return 0
}

// applySQLitePool applies the pool limits in opts to db, skipping every limit
// left unset so an untouched SQLiteOptions leaves database/sql's own defaults
// in place.
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
