package acpsvc

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/models/modelcapability"
	"github.com/contenox/contenox/internal/store/runtimetypes"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantName string
		wantArgs string
		wantOk   bool
	}{
		{name: "bare command", input: "/help", wantName: "help", wantOk: true},
		{name: "command with args", input: "/model qwen2.5:7b", wantName: "model", wantArgs: "qwen2.5:7b", wantOk: true},
		{name: "command with trailing space", input: "/clear   ", wantName: "clear", wantOk: true},
		{name: "leading whitespace", input: "  /provider ollama", wantName: "provider", wantArgs: "ollama", wantOk: true},
		{name: "compact with keep", input: "/compact 12", wantName: "compact", wantArgs: "12", wantOk: true},
		{name: "max tokens command", input: "/max-tokens 8192", wantName: "max-tokens", wantArgs: "8192", wantOk: true},
		{name: "args with extra spaces collapse to trimmed", input: "/model   gpt-4o  ", wantName: "model", wantArgs: "gpt-4o", wantOk: true},

		// Not commands:
		{name: "plain text", input: "hello there", wantOk: false},
		{name: "unknown slash word", input: "/foobar", wantOk: false},
		{name: "absolute path", input: "/home/user/file.go", wantOk: false},
		{name: "mid-sentence slash path", input: "what does /etc/passwd do", wantOk: false},
		{name: "empty", input: "", wantOk: false},
		{name: "just a slash", input: "/", wantOk: false},
		{name: "command not leading", input: "please run /help", wantOk: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name, args, ok := parseCommand(tc.input)
			if ok != tc.wantOk {
				t.Fatalf("parseCommand(%q) ok = %v, want %v", tc.input, ok, tc.wantOk)
			}
			if !tc.wantOk {
				return
			}
			if name != tc.wantName {
				t.Errorf("parseCommand(%q) name = %q, want %q", tc.input, name, tc.wantName)
			}
			if args != tc.wantArgs {
				t.Errorf("parseCommand(%q) args = %q, want %q", tc.input, args, tc.wantArgs)
			}
		})
	}
}

// TestUnknownCommandName pins which unrecognized leading slashes are answered locally vs. which must keep reaching the model as prompt text.
func TestUnknownCommandName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantName string
		wantOk   bool
	}{
		// Answered locally: command-shaped, name unknown.
		{name: "the dogfood case", input: "/totallyfakecommand", wantName: "totallyfakecommand", wantOk: true},
		{name: "unknown with args", input: "/nonsense do a thing", wantName: "nonsense", wantOk: true},
		{name: "leading whitespace", input: "  /nope", wantName: "nope", wantOk: true},
		{name: "dashed name", input: "/max-token", wantName: "max-token", wantOk: true},
		{name: "digits allowed", input: "/gpt4", wantName: "gpt4", wantOk: true},

		// Passed through: a KNOWN command is parseCommand's business, not this.
		{name: "known command", input: "/help", wantOk: false},
		{name: "known command with args", input: "/model qwen2.5:7b", wantOk: false},

		// Passed through: not command-SHAPED. These are the regressions this
		// test exists to prevent.
		{name: "absolute path", input: "/etc/passwd", wantOk: false},
		{name: "absolute path with args", input: "/home/x y", wantOk: false},
		{name: "path with extension", input: "/tmp/notes.md", wantOk: false},
		{name: "capitalized path", input: "/Users/alex", wantOk: false},
		{name: "underscore is not a command char", input: "/some_thing", wantOk: false},
		{name: "colon is not a command char", input: "/qwen2.5:7b", wantOk: false},

		// Passed through: no leading slash at all.
		{name: "prose mentioning a path", input: "what does /etc do", wantOk: false},
		{name: "plain text", input: "hello there", wantOk: false},
		{name: "slash mid-sentence", input: "please run /help", wantOk: false},
		{name: "empty", input: "", wantOk: false},
		{name: "bare slash", input: "/", wantOk: false},
		{name: "slash then space", input: "/ help", wantOk: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name, ok := unknownCommandName(tc.input)
			if ok != tc.wantOk {
				t.Fatalf("unknownCommandName(%q) ok = %v, want %v", tc.input, ok, tc.wantOk)
			}
			if tc.wantOk && name != tc.wantName {
				t.Errorf("unknownCommandName(%q) name = %q, want %q", tc.input, name, tc.wantName)
			}
		})
	}
}

// TestUnknownCommandNameNeverClaimsAKnownCommand pins: anything parseCommand recognizes is invisible to unknownCommandName.
func TestUnknownCommandNameNeverClaimsAKnownCommand(t *testing.T) {
	for _, c := range allACPCommands() {
		if name, ok := unknownCommandName("/" + c.Name); ok {
			t.Errorf("known command %q was claimed as unknown (%q)", c.Name, name)
		}
		if name, ok := unknownCommandName("/" + c.Name + " some args"); ok {
			t.Errorf("known command %q with args was claimed as unknown (%q)", c.Name, name)
		}
	}
}

func TestUnknownCommandMessage(t *testing.T) {
	got := unknownCommandMessage("totallyfakecommand")
	if !strings.Contains(got, "/totallyfakecommand") {
		t.Errorf("message must name what was typed, got %q", got)
	}
	if !strings.Contains(got, "/help") {
		t.Errorf("message must name the one next action, got %q", got)
	}
}

// TestAcpCommandsCoverDispatch pins: every command in allACPCommands is recognized by parseCommand.
func TestAcpCommandsCoverDispatch(t *testing.T) {
	for _, c := range allACPCommands() {
		if _, _, ok := parseCommand("/" + c.Name); !ok {
			t.Errorf("known command %q is not recognized by parseCommand", c.Name)
		}
	}
}

func TestUnit_HandleThinkStatusSetAndInvalid(t *testing.T) {
	sess := &sessionEntry{Think: "medium"}
	tr := &Transport{}

	out, err := tr.handleThink(sess, "")
	if err != nil {
		t.Fatalf("handleThink status: %v", err)
	}
	if out != "Think: medium" {
		t.Fatalf("status = %q, want Think: medium", out)
	}

	out, err = tr.handleThink(sess, "true")
	if err != nil {
		t.Fatalf("handleThink set alias: %v", err)
	}
	if out != "Think set to high for this session." {
		t.Fatalf("set output = %q", out)
	}
	if got := sess.think(); got != "high" {
		t.Fatalf("session think = %q, want high", got)
	}

	_, err = tr.handleThink(sess, "nonsense")
	if err == nil {
		t.Fatal("invalid think level should error")
	}
	if !strings.Contains(err.Error(), "invalid think level") {
		t.Fatalf("invalid error = %q", err.Error())
	}
	if got := sess.think(); got != "high" {
		t.Fatalf("invalid /think mutated session value to %q", got)
	}
}

func TestUnit_HandleMaxTokensStatusSetAndInvalid(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "max-tokens-acp.db")
	db, err := libdb.NewSQLiteDBManager(ctx, path, runtimetypes.SchemaSQLite)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tr := &Transport{deps: Deps{DB: db}, defaultMaxTokens: "4096"}
	out, err := tr.handleMaxTokens(ctx, "")
	if err != nil {
		t.Fatalf("handleMaxTokens status: %v", err)
	}
	if out != "Max tokens: 4096 | provider ceiling: unknown" {
		t.Fatalf("status = %q, want Max tokens: 4096 | provider ceiling: unknown", out)
	}

	out, err = tr.handleMaxTokens(ctx, " 8192 ")
	if err != nil {
		t.Fatalf("handleMaxTokens set: %v", err)
	}
	if out != "Max tokens set to 8192." {
		t.Fatalf("set output = %q", out)
	}
	if got := tr.maxTokens(); got != "8192" {
		t.Fatalf("transport max tokens = %q, want 8192", got)
	}
	if got := ReadConfigValue(ctx, db, "default-max-tokens"); got != "8192" {
		t.Fatalf("persisted max tokens = %q, want 8192", got)
	}

	_, err = tr.handleMaxTokens(ctx, "many")
	if err == nil || !strings.Contains(err.Error(), "max-tokens must be") {
		t.Fatalf("invalid max-tokens error = %v", err)
	}
	if got := tr.maxTokens(); got != "8192" {
		t.Fatalf("invalid /max-tokens mutated value to %q", got)
	}
}

func TestUnit_HandleCapabilitySetShowUnset(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "capability-acp.db")
	db, err := libdb.NewSQLiteDBManager(ctx, path, runtimetypes.SchemaSQLite)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tr := &Transport{deps: Deps{DB: db}}
	out, err := tr.handleCapability(ctx, "set OpenAI gpt-5-mini --think true")
	if err != nil {
		t.Fatalf("set capability: %v", err)
	}
	if out != "Capability override set for openai/gpt-5-mini: think=true." {
		t.Fatalf("set output = %q", out)
	}

	override, ok, err := modelcapability.New(runtimetypes.New(db.WithoutTransaction())).Get(ctx, "openai", "gpt-5-mini")
	if err != nil || !ok || override.CanThink == nil || !*override.CanThink {
		t.Fatalf("stored override = %#v ok=%v err=%v", override, ok, err)
	}

	out, err = tr.handleCapability(ctx, "show openai gpt-5-mini")
	if err != nil {
		t.Fatalf("show capability: %v", err)
	}
	if out != "Capability override for openai/gpt-5-mini: think=true." {
		t.Fatalf("show output = %q", out)
	}

	out, err = tr.handleCapability(ctx, "unset openai gpt-5-mini")
	if err != nil {
		t.Fatalf("unset capability: %v", err)
	}
	if out != "Capability override removed for openai/gpt-5-mini." {
		t.Fatalf("unset output = %q", out)
	}
}

func TestUnit_ParseCapabilitySetArgs(t *testing.T) {
	provider, model, canThink, err := parseCapabilitySetArgs([]string{"set", "VLLM", "Qwen/Qwen3-32B", "--think=false"})
	if err != nil {
		t.Fatalf("parse inline flag: %v", err)
	}
	if provider != "VLLM" || model != "Qwen/Qwen3-32B" || canThink {
		t.Fatalf("parsed = provider=%q model=%q canThink=%v", provider, model, canThink)
	}

	_, _, _, err = parseCapabilitySetArgs([]string{"set", "openai", "gpt-5", "--think", "maybe"})
	if err == nil || !strings.Contains(err.Error(), "--think must be true or false") {
		t.Fatalf("invalid think error = %v", err)
	}
}
