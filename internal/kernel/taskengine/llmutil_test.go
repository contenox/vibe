package taskengine_test

import (
	"testing"

	"github.com/contenox/beam/internal/kernel/taskengine"
)

func TestUnit_ExtractJSONObject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{`{"steps":["a"]}`, `{"steps":["a"]}`},
		{`prefix {"a":1} suffix`, `{"a":1}`},
		{"```json\n{\"x\":1}\n```", `{"x":1}`},
	}
	for _, tc := range cases {
		got := taskengine.ExtractJSONObject(tc.in)
		if got != tc.want {
			t.Errorf("ExtractJSONObject(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}
