package substrate

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libkvstore"
)

const (
	PostgresURLEnv = "CONTENOX_POSTGRES_URL"
	NATSURLEnv     = "CONTENOX_NATS_URL"
	ValkeyURLEnv   = "CONTENOX_VALKEY_URL"
)

const valkeyNamespaceParam = "namespace"

type Selection struct {
	PostgresDSN     string
	NATSURL         string
	ValkeyURL       string
	ValkeyAddr      string
	ValkeyUsername  string
	ValkeyPassword  string
	ValkeyDB        int
	ValkeyNamespace string
}

func (s Selection) UsesPostgres() bool { return s.PostgresDSN != "" }

func (s Selection) UsesNATS() bool { return s.NATSURL != "" }

func (s Selection) UsesValkey() bool { return s.ValkeyAddr != "" }

func (s Selection) valkeyConfig() libkvstore.Config {
	return libkvstore.Config{
		KVAddr:      s.ValkeyAddr,
		KVUsername:  s.ValkeyUsername,
		KVPassword:  s.ValkeyPassword,
		KVDB:        s.ValkeyDB,
		KVNamespace: s.ValkeyNamespace,
	}
}

func Resolve() (Selection, error) {
	return resolveFrom(os.Getenv)
}

func Configured() bool {
	return configuredFrom(os.Getenv)
}

func configuredFrom(getenv func(string) string) bool {
	for _, key := range []string{PostgresURLEnv, NATSURLEnv, ValkeyURLEnv} {
		if strings.TrimSpace(getenv(key)) != "" {
			return true
		}
	}
	return false
}

func resolveFrom(getenv func(string) string) (Selection, error) {
	var sel Selection

	if raw := strings.TrimSpace(getenv(PostgresURLEnv)); raw != "" {
		if err := checkPostgresDSN(raw); err != nil {
			return Selection{}, fmt.Errorf("%s is set to %q, which is not usable: %w", PostgresURLEnv, redact(raw), err)
		}
		sel.PostgresDSN = raw
	}

	if raw := strings.TrimSpace(getenv(NATSURLEnv)); raw != "" {
		if err := checkNATSURL(raw); err != nil {
			return Selection{}, fmt.Errorf("%s is set to %q, which is not usable: %w", NATSURLEnv, redact(raw), err)
		}
		sel.NATSURL = raw
	}

	if raw := strings.TrimSpace(getenv(ValkeyURLEnv)); raw != "" {
		target, err := parseValkeyURL(raw)
		if err != nil {
			return Selection{}, fmt.Errorf("%s is set to %q, which is not usable: %w", ValkeyURLEnv, redact(raw), err)
		}
		sel.ValkeyURL = raw
		sel.ValkeyAddr = target.addr
		sel.ValkeyUsername = target.username
		sel.ValkeyPassword = target.password
		sel.ValkeyDB = target.db
		sel.ValkeyNamespace = target.namespace
	}

	if sel.UsesPostgres() {
		var missing []string
		if !sel.UsesNATS() {
			missing = append(missing, NATSURLEnv)
		}
		if !sel.UsesValkey() {
			missing = append(missing, ValkeyURLEnv)
		}
		if len(missing) > 0 {
			return Selection{}, fmt.Errorf(
				"%s selects Postgres, but the SQLite message bus and the SQLite key-value table bind '?' placeholders that Postgres rejects, so they cannot run on that connection: also set %s, or unset %s",
				PostgresURLEnv, strings.Join(missing, " and "), PostgresURLEnv)
		}
	}

	return sel, nil
}

func checkPostgresDSN(raw string) error {
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") {
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("not a parseable URL (%s)", redact(raw))
		}
		if u.Host == "" {
			return fmt.Errorf("names no host")
		}
		return nil
	}
	if strings.Contains(raw, "=") {
		return nil
	}
	return fmt.Errorf("expected a postgres:// URL or a keyword/value connection string such as \"host=db user=contenox dbname=contenox sslmode=disable\"")
}

func checkNATSURL(raw string) error {
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return fmt.Errorf("contains an empty entry in its comma-separated server list")
		}
		u, err := url.Parse(part)
		if err != nil {
			return fmt.Errorf("not a parseable URL (%s)", redact(part))
		}
		switch strings.ToLower(u.Scheme) {
		case "nats", "tls", "ws", "wss":
		case "":
			return fmt.Errorf("has no scheme; expected a nats:// URL such as \"nats://127.0.0.1:4222\"")
		default:
			return fmt.Errorf("has scheme %q; expected one of nats, tls, ws, wss", u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("names no host")
		}
	}
	return nil
}

type valkeyTarget struct {
	addr      string
	username  string
	password  string
	db        int
	namespace string
}

func parseValkeyURL(raw string) (valkeyTarget, error) {
	if !strings.Contains(raw, "://") {
		if _, _, err := net.SplitHostPort(raw); err != nil {
			return valkeyTarget{}, fmt.Errorf("expected a valkey:// URL or a host:port address: %w", err)
		}
		return valkeyTarget{addr: raw}, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return valkeyTarget{}, fmt.Errorf("not a parseable URL (%s)", redact(raw))
	}
	switch strings.ToLower(u.Scheme) {
	case "valkey", "redis":
	case "valkeys", "rediss":
		return valkeyTarget{}, fmt.Errorf("requests TLS, which this key-value client is not wired for; use valkey:// or terminate TLS in front of it")
	default:
		return valkeyTarget{}, fmt.Errorf("has scheme %q; expected valkey or redis", u.Scheme)
	}
	if u.Host == "" {
		return valkeyTarget{}, fmt.Errorf("names no host")
	}
	target := valkeyTarget{addr: u.Host}
	if u.User != nil {
		target.username = u.User.Username()
		target.password, _ = u.User.Password()
		if target.username != "" && target.password == "" {
			return valkeyTarget{}, fmt.Errorf("carries a user with no password, which cannot authenticate: write it as \"valkey://user:password@%s\", or as \"valkey://:password@%s\" to authenticate as the default user",
				u.Host, u.Host)
		}
	}
	if target.db, err = parseValkeyDB(u.Path); err != nil {
		return valkeyTarget{}, err
	}
	if target.namespace, err = parseValkeyNamespace(u); err != nil {
		return valkeyTarget{}, err
	}
	return target, nil
}

func parseValkeyDB(path string) (int, error) {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return 0, nil
	}
	if strings.Contains(trimmed, "/") {
		return 0, fmt.Errorf("carries the path %q; only a database index such as \"/3\" is understood there", path)
	}
	db, err := strconv.Atoi(trimmed)
	if err != nil || db < 0 {
		return 0, fmt.Errorf("names the database %q, which is not a database index; use a whole number of zero or more, such as \"/3\"", trimmed)
	}
	return db, nil
}

func parseValkeyNamespace(u *url.URL) (string, error) {
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", fmt.Errorf("carries a query string that cannot be parsed: %w", err)
	}
	var unsupported []string
	for key := range values {
		if key != valkeyNamespaceParam {
			unsupported = append(unsupported, key)
		}
	}
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		return "", fmt.Errorf("sets %s, which nothing reads; only %q is understood, and the database index belongs in the path as \"/3\"",
			strings.Join(unsupported, " and "), valkeyNamespaceParam)
	}
	if _, set := values[valkeyNamespaceParam]; !set {
		return "", nil
	}
	namespace := strings.TrimSpace(values.Get(valkeyNamespaceParam))
	if strings.Trim(namespace, ":") == "" {
		return "", fmt.Errorf("sets an empty %s; give it a prefix such as %q, or leave it out",
			valkeyNamespaceParam, "contenox")
	}
	if strings.ContainsAny(namespace, "*?[]\\") || strings.ContainsAny(namespace, " \t\n") {
		return "", fmt.Errorf("sets the %s %q; a namespace is a literal key prefix, so it cannot contain whitespace or the glob characters *?[]\\",
			valkeyNamespaceParam, namespace)
	}
	return namespace, nil
}

const maskedCredential = "xxxxx"

func redact(raw string) string {
	if strings.Count(raw, "://") < 2 {
		return redactURL(raw)
	}
	parts := strings.Split(raw, ",")
	for i, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		if masked := redactURL(trimmed); masked != trimmed {
			parts[i] = strings.Replace(part, trimmed, masked, 1)
		}
	}
	return strings.Join(parts, ",")
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return maskUserinfo(raw)
	}
	if _, set := u.User.Password(); set {
		u.User = url.UserPassword(u.User.Username(), maskedCredential)
	} else {
		u.User = url.User(maskedCredential)
	}
	return u.String()
}

// maskUserinfo finds '@' scanning from the end, since a password may itself contain '/', '?' or '#'.
func maskUserinfo(raw string) string {
	sep := strings.Index(raw, "://")
	if sep < 0 {
		return raw
	}
	rest := raw[sep+len("://"):]
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return raw
	}
	masked := maskedCredential
	if colon := strings.Index(rest[:at], ":"); colon >= 0 {
		masked = rest[:colon] + ":" + maskedCredential
	}
	return raw[:sep+len("://")] + masked + rest[at:]
}

// dbPathExplicit must be true only when sqlitePath was named by the caller rather than defaulted.
func OpenDB(ctx context.Context, sqlitePath string, dbPathExplicit bool) (libdb.DBManager, error) {
	sel, err := Resolve()
	if err != nil {
		return nil, err
	}
	if sel.UsesPostgres() {
		if dbPathExplicit {
			return nil, fmt.Errorf("%s selects Postgres, but %q was also given explicitly as the SQLite database path; use one or the other", PostgresURLEnv, sqlitePath)
		}
		db, err := libdb.NewPostgresDBManager(ctx, sel.PostgresDSN, runtimetypes.SchemaPostgres)
		if err != nil {
			return nil, fmt.Errorf("%s: cannot use the Postgres database it names: %w", PostgresURLEnv, err)
		}
		return db, nil
	}
	if err := os.MkdirAll(filepath.Dir(sqlitePath), 0o755); err != nil {
		return nil, fmt.Errorf("cannot create database directory: %w", err)
	}
	schema := runtimetypes.SchemaSQLite + "\n" + libkvstore.SQLiteSchema
	db, err := libdb.NewSQLiteDBManager(ctx, sqlitePath, schema)
	if err != nil {
		return nil, fmt.Errorf("failed to open database %q: %w", sqlitePath, err)
	}
	return db, nil
}

func OpenBus(ctx context.Context, exec libdb.Exec) (libbus.Messenger, error) {
	sel, err := Resolve()
	if err != nil {
		return nil, err
	}
	if sel.UsesNATS() {
		bus, err := libbus.NewPubSub(ctx, &libbus.Config{NATSURL: sel.NATSURL})
		if err != nil {
			return nil, fmt.Errorf("%s: cannot reach the NATS server it names: %w", NATSURLEnv, err)
		}
		return bus, nil
	}
	return libbus.NewSQLite(exec), nil
}

func OpenKV(ctx context.Context, db libdb.DBManager) (libkvstore.KVManager, func(), error) {
	sel, err := Resolve()
	if err != nil {
		return nil, nil, err
	}
	if sel.UsesValkey() {
		mgr, err := libkvstore.NewManager(sel.valkeyConfig(), 0)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: cannot reach the Valkey server it names: %w", ValkeyURLEnv, err)
		}
		if err := probeKV(ctx, mgr); err != nil {
			_ = mgr.Close()
			return nil, nil, fmt.Errorf("%s: cannot reach the Valkey server it names: %w", ValkeyURLEnv, err)
		}
		return mgr, func() { _ = mgr.Close() }, nil
	}
	return libkvstore.NewSQLiteManager(db), func() {}, nil
}

func probeKV(ctx context.Context, mgr libkvstore.KVManager) error {
	exec, err := mgr.Executor(ctx)
	if err != nil {
		return err
	}
	_, err = exec.Exists(ctx, "contenox:substrate:probe")
	return err
}
