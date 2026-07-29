package devicelease

import (
	"context"
	"errors"
	"testing"

	"github.com/contenox/contenox/internal/transport"
)

func TestUnit_DeviceLeaseBlocksSecondAcceleratorSessionUntilClose(t *testing.T) {
	dir := t.TempDir()
	req := transport.OpenSessionRequest{ModelName: "m", Type: "llama"}
	info := transport.ModelInfo{DeviceKind: "gpu", DeviceID: "GPU-0"}
	first := New(&fakeService{info: info}, WithLeaseDir(dir))
	second := New(&fakeService{info: info}, WithLeaseDir(dir))

	sess, err := first.OpenSession(context.Background(), req)
	if err != nil {
		t.Fatalf("first OpenSession: %v", err)
	}
	if _, err := second.OpenSession(context.Background(), req); !errors.Is(err, transport.ErrDeviceBusy) {
		t.Fatalf("second OpenSession while first holds device = %v, want ErrDeviceBusy", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	sess2, err := second.OpenSession(context.Background(), req)
	if err != nil {
		t.Fatalf("second OpenSession after close: %v", err)
	}
	_ = sess2.Close()
}

func TestUnit_DeviceLeaseDescribeDoesNotAcquire(t *testing.T) {
	dir := t.TempDir()
	req := transport.OpenSessionRequest{ModelName: "m", Type: "llama"}
	info := transport.ModelInfo{DeviceKind: "gpu", DeviceID: "GPU-0"}
	first := New(&fakeService{info: info}, WithLeaseDir(dir))
	second := New(&fakeService{info: info}, WithLeaseDir(dir))

	if _, err := first.Describe(context.Background(), req); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	sess, err := second.OpenSession(context.Background(), req)
	if err != nil {
		t.Fatalf("OpenSession after side-effect-free Describe: %v", err)
	}
	_ = sess.Close()
}

func TestUnit_DeviceLeaseSkipsSystemMemory(t *testing.T) {
	dir := t.TempDir()
	req := transport.OpenSessionRequest{ModelName: "m", Type: "llama"}
	info := transport.ModelInfo{DeviceKind: "system", DeviceID: "ram"}
	first := New(&fakeService{info: info}, WithLeaseDir(dir))
	second := New(&fakeService{info: info}, WithLeaseDir(dir))

	sess1, err := first.OpenSession(context.Background(), req)
	if err != nil {
		t.Fatalf("first OpenSession: %v", err)
	}
	defer sess1.Close()
	sess2, err := second.OpenSession(context.Background(), req)
	if err != nil {
		t.Fatalf("second system-memory OpenSession: %v", err)
	}
	_ = sess2.Close()
}

func TestUnit_DeviceLeaseEmbedsAcquireForCallDuration(t *testing.T) {
	dir := t.TempDir()
	info := transport.ModelInfo{DeviceKind: "gpu", DeviceID: "GPU-0"}
	block := make(chan struct{})
	started := make(chan struct{})
	first := New(&fakeService{info: info, embedStarted: started, embedBlock: block}, WithLeaseDir(dir))
	second := New(&fakeService{info: info}, WithLeaseDir(dir))

	done := make(chan error, 1)
	go func() {
		_, err := first.Embed(context.Background(), transport.EmbedRequest{ModelName: "m", Type: "llama"})
		done <- err
	}()
	<-started
	if _, err := second.OpenSession(context.Background(), transport.OpenSessionRequest{ModelName: "m", Type: "llama"}); !errors.Is(err, transport.ErrDeviceBusy) {
		t.Fatalf("OpenSession while Embed holds device = %v, want ErrDeviceBusy", err)
	}
	close(block)
	if err := <-done; err != nil {
		t.Fatalf("Embed: %v", err)
	}
	sess, err := second.OpenSession(context.Background(), transport.OpenSessionRequest{ModelName: "m", Type: "llama"})
	if err != nil {
		t.Fatalf("OpenSession after Embed released device: %v", err)
	}
	_ = sess.Close()
}

type fakeService struct {
	info         transport.ModelInfo
	embedStarted chan struct{}
	embedBlock   chan struct{}
}

func (s *fakeService) OpenSession(context.Context, transport.OpenSessionRequest) (transport.Session, error) {
	return &fakeSession{}, nil
}

func (s *fakeService) Describe(context.Context, transport.OpenSessionRequest) (transport.ModelInfo, error) {
	return s.info, nil
}

func (s *fakeService) Embed(context.Context, transport.EmbedRequest) (transport.EmbedResult, error) {
	if s.embedStarted != nil {
		close(s.embedStarted)
	}
	if s.embedBlock != nil {
		<-s.embedBlock
	}
	return transport.EmbedResult{Vector: []float32{1}}, nil
}

type fakeSession struct{ transport.Session }

func (s *fakeSession) Close() error { return nil }
