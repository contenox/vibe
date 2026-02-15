package jseval

import (
	"context"

	"github.com/contenox/vibe/libtracker"
	"github.com/dop251/goja"
)

// Builtin is the plugin interface for VM globals. Each implementation provides
// name, description, parameters schema, and registers itself on the VM.
type Builtin interface {
	Name() string
	Description() string
	// ParametersSchema returns a JSON Schema object (type, properties, required)
	// compatible with taskengine.FunctionTool.Parameters. May be nil or empty.
	ParametersSchema() map[string]any
	// Register performs vm.Set(name, ...) and uses deps where needed.
	Register(vm *goja.Runtime, ctx context.Context, tracker libtracker.ActivityTracker, col *Collector, deps BuiltinHandlers) error
}
