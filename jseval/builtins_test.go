package jseval

import (
	"context"
	"testing"
)

func TestDefaultBuiltins(t *testing.T) {
	builtins := DefaultBuiltins()
	if len(builtins) == 0 {
		t.Fatal("DefaultBuiltins() returned empty slice")
	}
	names := make(map[string]bool)
	for _, b := range builtins {
		name := b.Name()
		if name == "" {
			t.Errorf("builtin has empty Name()")
		}
		if names[name] {
			t.Errorf("duplicate builtin name: %s", name)
		}
		names[name] = true
		if b.Description() == "" && name != "console" {
			t.Errorf("builtin %s has empty Description()", name)
		}
	}
	want := []string{"console", "sendEvent", "callTaskChain", "executeTask", "executeTaskChain", "executeHook", "httpFetch"}
	if len(builtins) != len(want) {
		t.Errorf("DefaultBuiltins() length = %d, want %d", len(builtins), len(want))
	}
	for _, w := range want {
		if !names[w] {
			t.Errorf("DefaultBuiltins() missing %s", w)
		}
	}
}

func TestBuiltinParametersSchema(t *testing.T) {
	builtins := DefaultBuiltins()
	for _, b := range builtins {
		name := b.Name()
		schema := b.ParametersSchema()
		if name == "console" {
			if schema != nil {
				t.Errorf("console should have nil ParametersSchema, got %v", schema)
			}
			continue
		}
		if name == "httpFetch" {
			// httpFetch has optional schema
			continue
		}
		if schema == nil && (name == "sendEvent" || name == "executeHook") {
			t.Errorf("builtin %s should have non-nil ParametersSchema", name)
		}
		if schema != nil {
			if _, ok := schema["type"]; !ok {
				t.Errorf("builtin %s ParametersSchema should have type", name)
			}
		}
	}
}

func TestGetBuiltinSignatures(t *testing.T) {
	env := NewEnv(nil, BuiltinHandlers{}, DefaultBuiltins())
	sigs := env.GetBuiltinSignatures()
	if len(sigs) != len(DefaultBuiltins()) {
		t.Errorf("GetBuiltinSignatures() length = %d, want %d", len(sigs), len(DefaultBuiltins()))
	}
	for i, tool := range sigs {
		if tool.Type != "function" {
			t.Errorf("sigs[%d].Type = %q, want function", i, tool.Type)
		}
		if tool.Function.Name == "" {
			t.Errorf("sigs[%d].Function.Name is empty", i)
		}
		if tool.Function.Description == "" && tool.Function.Name != "console" {
			t.Errorf("sigs[%d].Function.Description is empty for %s", i, tool.Function.Name)
		}
	}
}

func TestGetBuiltinSignatures_NilEnv(t *testing.T) {
	var env *Env
	sigs := env.GetBuiltinSignatures()
	if sigs != nil {
		t.Errorf("GetBuiltinSignatures() on nil Env should return nil, got len=%d", len(sigs))
	}
}

func TestGetBuiltinSignatures_NoBuiltins(t *testing.T) {
	env := NewEnv(nil, BuiltinHandlers{}, nil)
	sigs := env.GetBuiltinSignatures()
	if sigs != nil {
		t.Errorf("GetBuiltinSignatures() with nil builtins should return nil, got len=%d", len(sigs))
	}
}

func TestGetExecuteHookToolDescriptions_NilHookRepo(t *testing.T) {
	env := NewEnv(nil, BuiltinHandlers{}, DefaultBuiltins())
	ctx := context.Background()
	tools, err := env.GetExecuteHookToolDescriptions(ctx)
	if err != nil {
		t.Fatalf("GetExecuteHookToolDescriptions: %v", err)
	}
	if tools != nil {
		t.Errorf("GetExecuteHookToolDescriptions() with nil HookRepo should return nil, got len=%d", len(tools))
	}
}
