package liblog_test

import (
	"testing"

	"github.com/contenox/contenox/liblog"
)

func TestUnit_Log_ParseSizeAcceptsWhatAnOperatorTypes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"2048", 2048},
		{"512B", 512},
		{"10MB", 10 << 20},
		{"10mb", 10 << 20},
		{"10 MB", 10 << 20},
		{"10MiB", 10 << 20},
		{"512KB", 512 << 10},
		{"512k", 512 << 10},
		{"1GB", 1 << 30},
		{"1.5MB", 1 << 20 * 3 / 2},
	} {
		got, err := liblog.ParseSize(tc.in)
		if err != nil {
			t.Fatalf("ParseSize(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ParseSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// KB means 1024 here, deliberately: the value is a disk budget, and resolving
// it to 1000 would make the file smaller than the number the operator typed.
func TestUnit_Log_ParseSizeUsesBinaryUnits(t *testing.T) {
	got, err := liblog.ParseSize("1KB")
	if err != nil {
		t.Fatalf("ParseSize: %v", err)
	}
	if got != 1024 {
		t.Fatalf("ParseSize(\"1KB\") = %d, want 1024", got)
	}
}

// A typo must be refused at `config set`, not silently turned into a bound
// nobody chose.
func TestUnit_Log_ParseSizeRejectsNonsense(t *testing.T) {
	for _, in := range []string{"", "   ", "big", "MB", "-5MB", "0", "10PB", "1..2MB"} {
		if got, err := liblog.ParseSize(in); err == nil {
			t.Fatalf("ParseSize(%q) = %d, want an error", in, got)
		}
	}
}

// A status screen states the bound in force; it must round-trip through the
// parser so screen and config value cannot disagree.
func TestUnit_Log_FormatSizeRoundTrips(t *testing.T) {
	for _, n := range []int64{512, 1 << 10, 10 << 20, 1 << 30, 3 << 20} {
		s := liblog.FormatSize(n)
		back, err := liblog.ParseSize(s)
		if err != nil {
			t.Fatalf("FormatSize(%d) = %q, which does not parse: %v", n, s, err)
		}
		if back != n {
			t.Fatalf("round trip: %d → %q → %d", n, s, back)
		}
	}
}
