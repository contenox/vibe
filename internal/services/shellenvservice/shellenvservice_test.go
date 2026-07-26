package shellenvservice

import "testing"

func TestUnit_ValidEnvName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"PATH", true},
		{"HTTP_PROXY", true},
		{"_UNDERSCORE_LEAD", true},
		{"A1", true},
		{"lower_ok", true},
		{"", false},
		{"1LEADING_DIGIT", false},
		{"HAS=EQUALS", false},
		{"HAS SPACE", false},
		{"HAS-DASH", false},
		{"HAS.DOT", false},
	}
	for _, c := range cases {
		if got := ValidEnvName(c.name); got != c.want {
			t.Errorf("ValidEnvName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
