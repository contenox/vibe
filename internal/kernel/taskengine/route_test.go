package taskengine

import "testing"

func TestUnit_declaredRoutes_onlyEqualsBranches(t *testing.T) {
	branches := []TransitionBranch{
		{Operator: OpEquals, When: "code_task", Goto: "a"},
		{Operator: OpEquals, When: "question", Goto: "b"},
		{Operator: OpEquals, When: "  ", Goto: "blank"},
		{Operator: OpDefault, When: "", Goto: "fallback"},
		{Operator: OpEdgeTraversedAtLeast, When: "10", Goto: "budget"},
	}
	got := declaredRoutes(branches)
	want := []string{"code_task", "question"}
	if len(got) != len(want) {
		t.Fatalf("declaredRoutes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("declaredRoutes[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestUnit_selectRoute(t *testing.T) {
	routes := []string{"code_task", "question", "unsafe"}
	cases := []struct {
		answer string
		want   string
	}{
		{"question", "question"},
		{"  CODE_TASK ", "code_task"},
		{"The label is: unsafe", "unsafe"},
		{"i think this is a question really", "question"},
		{"none of these", "none of these"},
	}
	for _, c := range cases {
		if got := selectRoute(c.answer, routes); got != c.want {
			t.Fatalf("selectRoute(%q) = %q, want %q", c.answer, got, c.want)
		}
	}
}
