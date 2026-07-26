package hostcapacity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestDetectPrefersNVIDIAWhenProbeSucceeds(t *testing.T) {
	d := &Detector{
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			assert.Equal(t, "nvidia-smi", name)
			assert.Equal(t, []string{
				"--query-gpu=name,memory.total,memory.free",
				"--format=csv,noheader,nounits",
			}, args)
			return []byte("NVIDIA GeForce GTX 1660, 6144, 5120\n"), nil
		},
		systemRAM: func() (int64, int64, error) {
			t.Fatal("system RAM fallback should not be used after successful NVIDIA probe")
			return 0, 0, nil
		},
		timeout: time.Second,
	}

	got := d.Detect(context.Background())

	assert.True(t, got.Known)
	assert.Equal(t, "gpu", got.Kind)
	assert.Equal(t, "NVIDIA GeForce GTX 1660", got.Label)
	assert.Equal(t, int64(6144<<20), got.TotalBytes)
	assert.Equal(t, int64(5120<<20), got.FreeBytes)
}

func TestDetectFallsBackToSystemRAMWhenNVIDIAProbeFails(t *testing.T) {
	d := &Detector{
		run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("nvidia-smi unavailable")
		},
		systemRAM: func() (int64, int64, error) {
			return 16_000_000_000, 12_000_000_000, nil
		},
		timeout: time.Second,
	}

	got := d.Detect(context.Background())

	assert.True(t, got.Known)
	assert.Equal(t, "system", got.Kind)
	assert.Equal(t, "system RAM", got.Label)
	assert.Equal(t, int64(16_000_000_000), got.TotalBytes)
	assert.Equal(t, int64(12_000_000_000), got.FreeBytes)
}

func TestDetectFallsBackToSystemRAMWhenNVIDIAOutputIsMalformed(t *testing.T) {
	d := &Detector{
		run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("not,csv,enough,fields"), nil
		},
		systemRAM: func() (int64, int64, error) {
			return 8_000_000_000, 4_000_000_000, nil
		},
		timeout: time.Second,
	}

	got := d.Detect(context.Background())

	assert.True(t, got.Known)
	assert.Equal(t, "system", got.Kind)
	assert.Equal(t, int64(8_000_000_000), got.TotalBytes)
	assert.Equal(t, int64(4_000_000_000), got.FreeBytes)
}

func TestDetectReturnsUnknownWhenNoSourceWorks(t *testing.T) {
	d := &Detector{
		run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("nvidia-smi unavailable")
		},
		systemRAM: func() (int64, int64, error) {
			return 0, 0, errors.New("mem unavailable")
		},
		timeout: time.Second,
	}

	got := d.Detect(context.Background())

	assert.False(t, got.Known)
	assert.Empty(t, got.Kind)
	assert.Zero(t, got.TotalBytes)
	assert.Zero(t, got.FreeBytes)
}
