package libbus

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	natsclient "github.com/nats-io/nats.go"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/nats"
)

func quiet() func() {
	null, _ := os.Open(os.DevNull)
	sout := os.Stdout
	serr := os.Stderr
	os.Stdout = null
	os.Stderr = null
	log.SetOutput(null)
	return func() {
		defer null.Close()
		os.Stdout = sout
		os.Stderr = serr
		log.SetOutput(os.Stderr)
	}
}

func SetupNatsInstance(ctx context.Context) (string, testcontainers.Container, func(), error) {
	defer quiet()()
	cleanup := func() {}
	natsContainer, err := nats.Run(ctx, "nats:2.10")
	if err != nil {
		return "", nil, cleanup, err
	}
	cleanup = func() {
		if err := testcontainers.TerminateContainer(natsContainer); err != nil {
			log.Printf("failed to terminate container: %s", err)
		}
	}
	cons, err := natsContainer.ConnectionString(ctx)
	if err != nil {
		return "", nil, cleanup, err
	}
	// The nats module's readiness wait can return before the server accepts
	// connections (observed as a first-connect EOF under host load), so probe
	// the URL here rather than leaking that race into every caller.
	deadline := time.Now().Add(30 * time.Second)
	for {
		probe, cerr := natsclient.Connect(cons)
		if cerr == nil {
			probe.Close()
			break
		}
		if time.Now().After(deadline) {
			return "", nil, cleanup, fmt.Errorf("nats at %s never accepted a connection: %w", cons, cerr)
		}
		select {
		case <-ctx.Done():
			return "", nil, cleanup, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return cons, natsContainer, cleanup, nil
}

// NewTestPubSub starts a NATS container using SetupNatsInstance,
// creates a new PubSub instance, and returns it along with a cleanup function.
func NewTestPubSub() (Messenger, func(), error) {
	ctx := context.Background()
	cons, container, cleanup, err := SetupNatsInstance(ctx)
	if err != nil {
		return nil, func() {}, err
	}
	log.Printf("NATS container running: %v", container)

	cfg := &Config{
		NATSURL: cons,
	}
	// The nats module's readiness wait can return before the server accepts
	// connections (observed as a first-connect EOF under host load), so retry
	// briefly rather than leaking that race into every suite using this helper.
	var ps Messenger
	deadline := time.Now().Add(10 * time.Second)
	for {
		ps, err = NewPubSub(ctx, cfg)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return ps, func() {
		_ = ps.Close()
		cleanup()
	}, nil
}
