package owner_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/modeld/owner"
)

func TestUnit_JoinSingleOwnerWithAdvertisedEndpoint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	leasePath := filepath.Join(t.TempDir(), "modeld.lease")
	const instances = 16

	start := make(chan struct{})
	results := make(chan *owner.Owner, instances)
	errs := make(chan error, instances)

	var wg sync.WaitGroup
	for i := range instances {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			o, err := owner.Join(ctx, owner.Config{
				LeasePath: leasePath,
				TTL:       time.Second,
				Endpoint:  fmt.Sprintf("127.0.0.1:%d", 30000+i),
			})
			if err != nil {
				errs <- err
				return
			}
			results <- o
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("Join: %v", err)
	}

	var ownerCount, followerCount int
	var advertisedEndpoint string
	for o := range results {
		if o.IsOwner() {
			ownerCount++
			advertisedEndpoint = o.Endpoint()
			defer func() { _ = o.Release() }()
			continue
		}
		followerCount++
		if o.Endpoint() == "" {
			t.Fatal("follower did not observe owner endpoint")
		}
	}

	if ownerCount != 1 {
		t.Fatalf("owner count = %d, want 1", ownerCount)
	}
	if followerCount != instances-1 {
		t.Fatalf("follower count = %d, want %d", followerCount, instances-1)
	}
	if advertisedEndpoint == "" {
		t.Fatal("owner endpoint was not advertised")
	}
}
