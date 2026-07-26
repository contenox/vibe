package reasoning

import "testing"

func TestUnit_NormalizeThinkLevelsAndAliases(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"auto", Auto},
		{"AUTO", Auto},
		{"default", Auto},
		{"off", Off},
		{"none", Off},
		{"disabled", Off},
		{"false", Off},
		{"minimal", Minimal},
		{"low", Low},
		{"medium", Medium},
		{"high", High},
		{"xhigh", XHigh},
		{"true", High},
		{"on", High},
	}
	for _, tc := range cases {
		got, err := Normalize(tc.in)
		if err != nil {
			t.Fatalf("Normalize(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUnit_NormalizeOptionalAndDisplay(t *testing.T) {
	if _, ok, err := NormalizeOptional("   "); err != nil || ok {
		t.Fatalf("NormalizeOptional(blank) = ok %v err %v, want ok false nil", ok, err)
	}
	if DisplayEnabled(Off) {
		t.Fatal("off must not display thinking")
	}
	if DisplayEnabled(Auto) {
		t.Fatal("auto must not force display thinking")
	}
	if !DisplayEnabled(High) {
		t.Fatal("high must display thinking")
	}
	if _, err := Normalize("wat"); err == nil {
		t.Fatal("invalid levels must error")
	}
}
