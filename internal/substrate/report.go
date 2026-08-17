package substrate

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libkvstore"
)

const (
	StoreSubstrate = "store"
	BusSubstrate   = "message bus"
	KVSubstrate    = "key-value cache"
)

const probeTimeout = 5 * time.Second

type Status struct {
	Substrate string
	Backend   string
	Setting   string
	Target    string
	Err       error
}

func (s Status) Remote() bool { return s.Setting != "" }

func Report(ctx context.Context, store libdb.Exec, sqlitePath string) ([]Status, error) {
	sel, err := Resolve()
	if err != nil {
		return nil, err
	}
	return []Status{
		storeStatus(ctx, sel, store, sqlitePath),
		busStatus(ctx, sel, sqlitePath),
		kvStatus(ctx, sel, sqlitePath),
	}, nil
}

func AnyRemote(statuses []Status) bool {
	for _, s := range statuses {
		if s.Remote() {
			return true
		}
	}
	return false
}

func storeStatus(ctx context.Context, sel Selection, store libdb.Exec, sqlitePath string) Status {
	if !sel.UsesPostgres() {
		return Status{Substrate: StoreSubstrate, Backend: "SQLite", Target: sqlitePath}
	}
	s := Status{Substrate: StoreSubstrate, Backend: "Postgres", Setting: PostgresURLEnv, Target: redactDSN(sel.PostgresDSN)}
	if store == nil {
		return s
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var reachable int
	if err := store.QueryRowContext(probeCtx, `SELECT 1`).Scan(&reachable); err != nil {
		s.Err = err
	}
	return s
}

func busStatus(ctx context.Context, sel Selection, sqlitePath string) Status {
	if !sel.UsesNATS() {
		return Status{Substrate: BusSubstrate, Backend: "SQLite", Target: sqlitePath}
	}
	s := Status{Substrate: BusSubstrate, Backend: "NATS", Setting: NATSURLEnv, Target: redact(sel.NATSURL)}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	bus, err := libbus.NewPubSub(probeCtx, &libbus.Config{NATSURL: sel.NATSURL})
	if err != nil {
		s.Err = err
		return s
	}
	_ = bus.Close()
	return s
}

func kvStatus(ctx context.Context, sel Selection, sqlitePath string) Status {
	if !sel.UsesValkey() {
		return Status{Substrate: KVSubstrate, Backend: "SQLite", Target: sqlitePath}
	}
	s := Status{Substrate: KVSubstrate, Backend: "Valkey", Setting: ValkeyURLEnv, Target: redact(sel.ValkeyURL)}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	mgr, err := libkvstore.NewManager(sel.valkeyConfig(), 0)
	if err != nil {
		s.Err = err
		return s
	}
	if err := probeKV(probeCtx, mgr); err != nil {
		s.Err = err
	}
	_ = mgr.Close()
	return s
}

var dsnPassword = regexp.MustCompile(`(?i)\bpassword\s*=\s*('[^']*'|"[^"]*"|\S*)`)

func redactDSN(raw string) string {
	if strings.Contains(raw, "://") {
		raw = redact(raw)
	}
	return dsnPassword.ReplaceAllString(raw, "password=xxxxx")
}
