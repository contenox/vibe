package taskengine

import "testing"

func TestExtractJSONObject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{`{"steps":["a"]}`, `{"steps":["a"]}`},
		{`prefix {"a":1} suffix`, `{"a":1}`},
		{"```json\n{\"x\":1}\n```", `{"x":1}`},
	}
	for _, tc := range cases {
		got := ExtractJSONObject(tc.in)
		if got != tc.want {
			t.Errorf("ExtractJSONObject(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}
