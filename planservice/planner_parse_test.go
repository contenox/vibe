package planservice

import (
	"strings"
	"testing"
)

func Test_parsePlannerJSONRaw(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{
			name: "array",
			raw:  `["a", "b"]`,
			want: []string{"a", "b"},
		},
		{
			name: "array in fences",
			raw:  "```json\n[\"x\"]\n```",
			want: []string{"x"},
		},
		{
			name: "steps strings",
			raw:  `{"steps":["one","two"]}`,
			want: []string{"one", "two"},
		},
		{
			name: "steps objects",
			raw:  `{"steps":[{"description":"first"},{"description":"second"}]}`,
			want: []string{"first", "second"},
		},
		{
			name:    "prose",
			raw:     "Hello.\nDone.",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parsePlannerJSONRaw(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v want %v", got, tc.want)
				}
			}
		})
	}
}

func Test_parsePlannerJSONRaw_emptyArray(t *testing.T) {
	t.Parallel()
	got, err := parsePlannerJSONRaw(`[]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func Test_parsePlannerJSONRaw_errorMessageContainsRaw(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 600)
	_, err := parsePlannerJSONRaw(long)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "xxx") {
		t.Fatalf("expected truncated raw in error: %v", err)
	}
}
