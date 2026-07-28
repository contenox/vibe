package gojatool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// script.go is the script-tool loader: *.js files in a configured directory
// become ordinary tools at engine build time.
//
// File convention:
//
//	const tool = {
//	  name: "changelog_entry",              // unprefixed tool name
//	  description: "One line telling the model when to use this.",
//	  schema: { type: "object", properties: { version: { type: "string" } }, required: ["version"] },
//	  tools: ["local_fs.read_file"],        // optional declared reach
//	  deadline_ms: 5000,                    // optional, capped at MaxDeadline
//	};
//
//	function run(args) {
//	  const notes = host.tool("local_fs.read_file", { path: "CHANGELOG.md" });
//	  return { version: args.version, lines: notes.text.split("\n").length };
//	}
//
// `tools: []` declares that the script reaches nothing; omitting `tools`
// leaves it unrestricted. Every validation failure is a startup error naming
// the file — nothing is silently skipped — except an absent script
// directory, which is not an error.

// Script is one loaded script tool.
type Script struct {
	// File is the path the script was loaded from — the identity every error
	// message uses.
	File string
	// Name is the declared tool name, registered unprefixed: the provider
	// ("goja") is the namespace, so the model addresses it as goja.<Name>.
	Name string
	// Description is the model-facing description.
	Description string
	// Schema is the declared JSON Schema object, normalised so `type` and
	// `properties` are always present.
	Schema map[string]any
	// Deadline is the effective per-execution budget (the declared
	// deadline_ms clamped to MaxDeadline, or the sandbox default).
	Deadline time.Duration

	// Tools is the script's declared reach: every "<provider>.<tool>"
	// address it may pass to host.tool, in declaration order.
	//
	// Read together with ToolsDeclared: an empty Tools with ToolsDeclared
	// true means the script declared it reaches nothing (host.tool refuses
	// every call); ToolsDeclared false means no list was declared and the
	// script may reach anything the envelope allows.
	Tools []string
	// ToolsDeclared reports whether the script's descriptor carried a
	// `tools` field. See Tools.
	ToolsDeclared bool

	prog       *goja.Program
	properties []string
	required   []string
	allowExtra bool
	reach      *declaredReach
}

// toolNamePattern is what a script may call itself. No dots: the engine
// addresses every tool as "<provider>.<tool>", so a dot in the name would
// produce an address nothing can resolve.
var toolNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`)

const (
	// maxDescriptionBytes bounds a script's description; descriptions are
	// paid on every turn.
	maxDescriptionBytes = 4 << 10
	// maxSchemaBytes bounds the marshaled schema, for the same reason.
	maxSchemaBytes = 16 << 10
)

// toolDescriptor is the JSON projection of a script's `tool` export. Tools is
// a RawMessage rather than a []string so an absent declaration and an empty
// one stay distinguishable — decoding straight into []string would collapse
// both to nil.
type toolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      map[string]any  `json:"schema"`
	Tools       json.RawMessage `json:"tools"`
	DeadlineMS  json.RawMessage `json:"deadline_ms"`
}

// maxDeclaredTools bounds the declared reach.
const maxDeclaredTools = 64

// loadScripts reads dir and returns its script tools in a deterministic
// order. reserved holds names the caller has already committed to, so a
// collision is caught here rather than discovered as a shadowed tool at
// runtime.
func (s *sandbox) loadScripts(ctx context.Context, dir string, reserved []string) ([]*Script, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	dir = filepath.Clean(dir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// Not an error: no script directory means no script tools.
			return nil, nil
		}
		return nil, wrapRecoverable(ErrScriptLoad, "cannot read script directory %s: %v", dir, err)
	}

	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".js") {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	sort.Strings(files)

	taken := make(map[string]string, len(files)+len(reserved))
	for _, name := range reserved {
		if name != "" {
			taken[name] = "a built-in tool of this provider"
		}
	}

	scripts := make([]*Script, 0, len(files))
	for _, file := range files {
		sc, err := s.loadScript(ctx, file)
		if err != nil {
			return nil, err
		}
		if owner, dup := taken[sc.Name]; dup {
			return nil, wrapRecoverable(ErrScriptLoad, "%s declares tool name %q, which is already %s. Tool names are unique within the %q provider; rename one of them",
				file, sc.Name, owner, ToolsProviderName)
		}
		taken[sc.Name] = fmt.Sprintf("declared by %s", sc.File)
		scripts = append(scripts, sc)
	}
	return scripts, nil
}

// loadScript compiles, executes and validates ONE file.
func (s *sandbox) loadScript(ctx context.Context, file string) (*Script, error) {
	src, err := os.ReadFile(file)
	if err != nil {
		return nil, wrapRecoverable(ErrScriptLoad, "cannot read %s: %v", file, err)
	}
	if len(src) > maxSourceBytes {
		return nil, wrapRecoverable(ErrScriptLoad, "%s is %d bytes, over the %d-byte limit for a script tool. A tool this large is a program; move the bulk behind a real tool", file, len(src), maxSourceBytes)
	}
	prog, err := s.cache.compile(file, string(src))
	if err != nil {
		return nil, wrapRecoverable(ErrScriptLoad, "%s did not parse: %v", file, err)
	}

	label := "load " + filepath.Base(file)
	// The file's top level runs here under the same deadline, stack cap and
	// panic recovery as a real execution, so a script that loops at load
	// time fails the build rather than hanging it. host.tool is refused
	// during load (see installHost).
	res, err := s.run(ctx, execSpec{
		label:       label,
		deadline:    s.deadline,
		hostEnabled: false,
		body: func(vm *goja.Runtime, _ jsonCodec) (goja.Value, error) {
			if _, rerr := vm.RunProgram(prog); rerr != nil {
				return nil, rerr
			}
			if _, ok := goja.AssertFunction(vm.Get("run")); !ok {
				return nil, fmt.Errorf("does not define `function run(args)`. Add: function run(args) { … return <json-serialisable value>; }")
			}
			return vm.Get("tool"), nil
		},
	})
	if err != nil {
		return nil, wrapRecoverable(ErrScriptLoad, "%s: %v", file, stripLabel(err.Error(), label))
	}
	if res.Truncated {
		return nil, wrapRecoverable(ErrScriptLoad, "%s exports a `tool` descriptor larger than the %d-byte output cap. Keep name/description/schema small", file, s.outputCap)
	}

	var desc toolDescriptor
	if err := json.Unmarshal(res.Value, &desc); err != nil || desc.Name == "" && desc.Description == "" && desc.Schema == nil {
		return nil, wrapRecoverable(ErrScriptLoad, "%s does not export a `tool` object. Add: const tool = { name: \"…\", description: \"…\", schema: { type: \"object\", properties: {} } };", file)
	}
	return s.validateDescriptor(file, prog, desc)
}

// validateDescriptor turns a parsed descriptor into a Script or a teaching
// error. Each check names the file and the exact repair.
func (s *sandbox) validateDescriptor(file string, prog *goja.Program, desc toolDescriptor) (*Script, error) {
	name := strings.TrimSpace(desc.Name)
	switch {
	case name == "":
		return nil, wrapRecoverable(ErrScriptLoad, "%s: tool.name is missing. Every script tool declares the name the model will call it by", file)
	case strings.Contains(name, "."):
		return nil, wrapRecoverable(ErrScriptLoad, "%s: tool.name %s contains a dot. The provider %q is already the namespace — the engine addresses this tool as %s.<name>, so the name itself must be a plain identifier",
			file, echoArg(name), ToolsProviderName, ToolsProviderName)
	case !toolNamePattern.MatchString(name):
		return nil, wrapRecoverable(ErrScriptLoad, "%s: tool.name %s is not a valid tool name. Use letters, digits, underscore or hyphen, starting with a letter or underscore, at most 64 characters", file, echoArg(name))
	case name == ToolsProviderName:
		return nil, wrapRecoverable(ErrScriptLoad, "%s: tool.name %q is the provider name itself and cannot also name a tool", file, name)
	case strings.HasPrefix(name, ToolsProviderName+"_"):
		return nil, wrapRecoverable(ErrScriptLoad, "%s: tool.name %q uses the reserved %q prefix, which belongs to this provider's built-in tools (%s). Pick a name that says what the script does",
			file, name, ToolsProviderName+"_", ToolEval)
	}

	description := strings.TrimSpace(desc.Description)
	if description == "" {
		return nil, wrapRecoverable(ErrScriptLoad, "%s: tool.description is missing. One line on WHEN to use this tool is what makes the model pick it correctly", file)
	}
	if len(description) > maxDescriptionBytes {
		return nil, wrapRecoverable(ErrScriptLoad, "%s: tool.description is %d bytes, over the %d-byte limit. Descriptions are paid on every turn; teach the details in the error messages the script returns instead", file, len(description), maxDescriptionBytes)
	}

	schema, props, required, allowExtra, err := normaliseSchema(file, desc.Schema)
	if err != nil {
		return nil, err
	}

	declaredTools, toolsDeclared, err := normaliseDeclaredTools(file, desc.Tools)
	if err != nil {
		return nil, err
	}

	deadline := time.Duration(0)
	if len(desc.DeadlineMS) > 0 && string(desc.DeadlineMS) != "null" {
		var ms float64
		if err := json.Unmarshal(desc.DeadlineMS, &ms); err != nil || ms <= 0 {
			return nil, wrapRecoverable(ErrScriptLoad, "%s: tool.deadline_ms must be a positive number of milliseconds (ceiling %d), got %s", file, MaxDeadline.Milliseconds(), clampText(string(desc.DeadlineMS), 64))
		}
		// Clamped, not refused: a script asking for more gets the most the
		// sandbox has.
		deadline = s.clampDeadline(time.Duration(ms) * time.Millisecond)
	}

	sc := &Script{
		File:          file,
		Name:          name,
		Description:   description,
		Schema:        schema,
		Deadline:      deadline,
		Tools:         declaredTools,
		ToolsDeclared: toolsDeclared,
		prog:          prog,
		properties:    props,
		required:      required,
		allowExtra:    allowExtra,
	}
	if toolsDeclared {
		sc.reach = newDeclaredReach(file, declaredTools)
	}
	return sc, nil
}

// normaliseDeclaredTools validates the optional `tools` declaration. Every
// failure is a load-time error naming the file.
func normaliseDeclaredTools(file string, raw json.RawMessage) (names []string, declared bool, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false, nil
	}
	var list []string
	if uerr := json.Unmarshal(raw, &list); uerr != nil {
		return nil, false, wrapRecoverable(ErrScriptLoad,
			`%s: tool.tools must be an array of "<provider>.<tool>" addresses, like tools: ["local_fs.read_file", "git.git_status"], got %s`,
			file, clampText(string(raw), 120))
	}
	if len(list) > maxDeclaredTools {
		return nil, false, wrapRecoverable(ErrScriptLoad,
			"%s: tool.tools declares %d tools, over the limit of %d. A script that addresses this many tools is a chain, and the list stops being something an operator can read on an approval card",
			file, len(list), maxDeclaredTools)
	}
	seen := make(map[string]struct{}, len(list))
	names = make([]string, 0, len(list))
	for _, entry := range list {
		addr := strings.TrimSpace(entry)
		provider, _, serr := splitToolName(addr)
		if serr != nil {
			return nil, false, wrapRecoverable(ErrScriptLoad,
				`%s: tool.tools entry %s is not a tool address — it needs both a provider and a tool, spelled exactly as host.tool addresses it ("local_fs.read_file")`,
				file, echoArg(entry))
		}
		if provider == ToolsProviderName {
			// Refused at load rather than at the call: the recursion guard
			// would refuse it anyway.
			return nil, false, wrapRecoverable(ErrScriptLoad,
				"%s: tool.tools declares %s, but scripts may not invoke %q-provider tools — sandbox depth is exactly one. Inline the computation instead",
				file, echoArg(addr), ToolsProviderName)
		}
		if _, dup := seen[addr]; dup {
			continue
		}
		seen[addr] = struct{}{}
		names = append(names, addr)
	}
	return names, true, nil
}

// normaliseSchema validates the declared schema and fills in the parts every
// provider expects, so a script author writes only the interesting half.
func normaliseSchema(file string, raw map[string]any) (schema map[string]any, props, required []string, allowExtra bool, err error) {
	if raw == nil {
		return nil, nil, nil, false, wrapRecoverable(ErrScriptLoad, "%s: tool.schema is missing. Declare the arguments: schema: { type: \"object\", properties: { … } } (use an empty properties object for a tool that takes none)", file)
	}
	if t, ok := raw["type"]; ok {
		ts, _ := t.(string)
		if ts != "object" {
			return nil, nil, nil, false, wrapRecoverable(ErrScriptLoad, "%s: tool.schema.type is %s; a tool's arguments are always an object", file, echoArg(fmt.Sprint(t)))
		}
	}

	schema = make(map[string]any, len(raw)+2)
	for k, v := range raw {
		schema[k] = v
	}
	schema["type"] = "object"

	propsRaw, ok := schema["properties"]
	if !ok || propsRaw == nil {
		schema["properties"] = map[string]any{}
		propsRaw = schema["properties"]
	}
	propsMap, ok := propsRaw.(map[string]any)
	if !ok {
		return nil, nil, nil, false, wrapRecoverable(ErrScriptLoad, "%s: tool.schema.properties must be an object mapping argument names to {type, description}", file)
	}
	props = make([]string, 0, len(propsMap))
	for name, def := range propsMap {
		if _, ok := def.(map[string]any); !ok {
			return nil, nil, nil, false, wrapRecoverable(ErrScriptLoad, "%s: tool.schema.properties.%s must be an object like { type: \"string\", description: \"…\" }", file, echoArg(name))
		}
		props = append(props, name)
	}
	sort.Strings(props)

	if reqRaw, ok := schema["required"]; ok && reqRaw != nil {
		list, ok := reqRaw.([]any)
		if !ok {
			return nil, nil, nil, false, wrapRecoverable(ErrScriptLoad, "%s: tool.schema.required must be an array of argument names", file)
		}
		for _, item := range list {
			name, ok := item.(string)
			if !ok {
				return nil, nil, nil, false, wrapRecoverable(ErrScriptLoad, "%s: tool.schema.required must contain only argument names (strings)", file)
			}
			if _, declared := propsMap[name]; !declared {
				return nil, nil, nil, false, wrapRecoverable(ErrScriptLoad, "%s: tool.schema.required names %s, which is not in properties. A required argument the model is never told about is an argument it can never supply", file, echoArg(name))
			}
			required = append(required, name)
		}
	}

	if extra, ok := schema["additionalProperties"]; ok {
		if b, isBool := extra.(bool); isBool {
			allowExtra = b
		}
	}

	encoded, merr := json.Marshal(schema)
	if merr != nil {
		return nil, nil, nil, false, wrapRecoverable(ErrScriptLoad, "%s: tool.schema is not serialisable: %v", file, merr)
	}
	if len(encoded) > maxSchemaBytes {
		return nil, nil, nil, false, wrapRecoverable(ErrScriptLoad, "%s: tool.schema is %d bytes, over the %d-byte limit", file, len(encoded), maxSchemaBytes)
	}
	return schema, props, required, allowExtra, nil
}

// exec runs one script tool: fresh VM, program re-evaluated (from the cached
// compilation), then run(args) with JSON-only arguments.
func (s *sandbox) execScript(ctx context.Context, sc *Script, args map[string]any) (*Result, error) {
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, recoverablef("goja: %s: arguments are not JSON-serialisable: %v", sc.Name, err)
	}
	deadline := sc.Deadline
	if deadline <= 0 {
		deadline = s.deadline
	}
	return s.run(ctx, execSpec{
		label:       sc.Name,
		deadline:    deadline,
		hostEnabled: true,
		reach:       sc.reach,
		body: func(vm *goja.Runtime, codec jsonCodec) (goja.Value, error) {
			if _, rerr := vm.RunProgram(sc.prog); rerr != nil {
				return nil, rerr
			}
			runFn, ok := goja.AssertFunction(vm.Get("run"))
			if !ok {
				// Load-time validation already proved this; a file edited on disk
				// after startup is the only way here, and it still must not panic.
				return nil, fmt.Errorf("%s no longer defines `function run(args)`", sc.File)
			}
			argVal, aerr := codec.toJS(encoded)
			if aerr != nil {
				return nil, aerr
			}
			return runFn(goja.Undefined(), argVal)
		},
	})
}

// stripLabel removes the sandbox's own label from an execution error, so a load
// failure reads "<file>: threw: …" rather than
// "<file>: goja: load <file> threw: …".
func stripLabel(msg, label string) string {
	msg = strings.TrimPrefix(msg, "goja: "+label)
	msg = strings.TrimPrefix(msg, ":")
	return strings.TrimSpace(msg)
}
