package taskengine

import "testing"

func TestUnit_RenderTemplate_SprigFuncsWired(t *testing.T) {
	out, err := renderTemplate(`up={{ "hi" | upper }} cut={{ trunc 3 "abcdef" }} arr={{ list 1 2 3 | toJson }}`, map[string]any{})
	if err != nil {
		t.Fatalf("renderTemplate error: %v", err)
	}
	want := `up=HI cut=abc arr=[1,2,3]`
	if out != want {
		t.Fatalf("sprig funcs not wired into renderTemplate\n want: %q\n got:  %q", want, out)
	}
}

func TestUnit_RenderTemplate_ToJsonStringifiesStructCleanly(t *testing.T) {
	type msg struct {
		Role    string
		Content string
	}
	vars := map[string]any{"out": []msg{{"user", "hi"}, {"assistant", "yo"}}}
	out, err := renderTemplate(`{{ .out | toJson }}`, vars)
	if err != nil {
		t.Fatalf("renderTemplate error: %v", err)
	}
	want := `[{"Role":"user","Content":"hi"},{"Role":"assistant","Content":"yo"}]`
	if out != want {
		t.Fatalf("toJson must cleanly stringify structs (the recovered prompt-injection-eval use case), not Go dump\n want: %q\n got:  %q", want, out)
	}
}
