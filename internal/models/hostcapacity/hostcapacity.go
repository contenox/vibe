// Package hostcapacity gives the CLI a best-effort, pre-install estimate of
// how much memory a curated model would have to fit in on this machine.
//
// It is deliberately coarse and deliberately not modeld: modeld's real
// capacity planning (modeld/capacity) resolves a model's actual KV profile
// against the live device it opened a session on, through a CGO-linked
// backend (ggml / OpenVINO device enumeration). None of that exists before
// modeld is even installed - exactly when `contenox setup` and
// `contenox model registry-list` need to say something useful about fit.
//
// Detect returns the best signal available without linking modeld: an NVIDIA
// GPU's total/free VRAM via a best-effort `nvidia-smi` shell probe (pure Go,
// no CGO - this is the CLI, not modeld, so the project's
// no-subprocess-in-modeld rule does not apply here), falling back to system
// RAM via gopsutil when no such GPU is found or the probe fails. Detect never
// fabricates a number: Known is false when neither source could be read, and
// callers must not display fit information in that case.
package hostcapacity

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/mem"
)

// Budget is the best-effort memory pool a curated model's estimated resident
// footprint (modelregistry.ModelDescriptor.EstimatedResidentBytes) is
// compared against.
type Budget struct {
	Kind       string // "gpu" | "system"
	Label      string // e.g. "NVIDIA GeForce GTX 1660" or "system RAM"
	TotalBytes int64
	FreeBytes  int64
	// Known is false when detection found nothing usable. Callers must treat
	// this as "no fit signal", never as "nothing fits".
	Known bool
}

// commandRunner is the injectable seam for tests: it must behave like
// exec.CommandContext(ctx, name, args...).Output().
type commandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// systemRAMFunc is the injectable seam for gopsutil, so tests can force a
// known or a failing value without depending on the test host's real memory.
type systemRAMFunc func() (total, available int64, err error)

func realSystemRAM() (int64, int64, error) {
	v, err := mem.VirtualMemory()
	if err != nil {
		return 0, 0, err
	}
	return int64(v.Total), int64(v.Available), nil
}

// Detector resolves the best-effort host memory budget. Construct with New;
// run and systemRAM are injectable for tests.
type Detector struct {
	run       commandRunner
	systemRAM systemRAMFunc
	timeout   time.Duration
}

// New returns a Detector using the real nvidia-smi + gopsutil sources.
func New() *Detector {
	return &Detector{run: execRunner, systemRAM: realSystemRAM, timeout: 3 * time.Second}
}

// Detect resolves the best available memory budget using the real
// nvidia-smi + gopsutil sources: an NVIDIA GPU when nvidia-smi is present and
// answers, else system RAM, else an unknown (Known: false) budget.
func Detect(ctx context.Context) Budget {
	return New().Detect(ctx)
}

// Detect resolves the best available memory budget for this Detector.
func (d *Detector) Detect(ctx context.Context) Budget {
	if b, ok := d.detectNVIDIA(ctx); ok {
		return b
	}
	if d.systemRAM == nil {
		return Budget{}
	}
	total, free, err := d.systemRAM()
	if err != nil || total <= 0 {
		return Budget{}
	}
	return Budget{Kind: "system", Label: "system RAM", TotalBytes: total, FreeBytes: free, Known: true}
}

// detectNVIDIA shells out to nvidia-smi for the first GPU's name and
// total/free VRAM. It reports ok=false on any error, absent binary, or
// unparsable output - never a partial/guessed Budget.
func (d *Detector) detectNVIDIA(ctx context.Context) (Budget, bool) {
	if d.run == nil {
		return Budget{}, false
	}
	cctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	out, err := d.run(cctx, "nvidia-smi", "--query-gpu=name,memory.total,memory.free", "--format=csv,noheader,nounits")
	if err != nil {
		return Budget{}, false
	}
	line := firstLine(out)
	if line == "" {
		return Budget{}, false
	}
	fields := strings.Split(line, ",")
	if len(fields) != 3 {
		return Budget{}, false
	}
	name := strings.TrimSpace(fields[0])
	totalMiB, err1 := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
	freeMiB, err2 := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
	if err1 != nil || err2 != nil || totalMiB <= 0 {
		return Budget{}, false
	}
	const mib = 1 << 20
	return Budget{
		Kind:       "gpu",
		Label:      name,
		TotalBytes: totalMiB * mib,
		FreeBytes:  freeMiB * mib,
		Known:      true,
	}, true
}

func firstLine(b []byte) string {
	b = bytes.TrimSpace(b)
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}
