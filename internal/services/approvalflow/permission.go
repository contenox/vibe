package approvalflow

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/libacp"
)

const (
	OptionAllow = "allow"
	OptionDeny  = "deny"
)

type BuildOptions struct {
	SessionID  libacp.SessionID
	PolicyName string
	PolicyPath string

	// MayCall and MayCallDeclared are the gated call's declared reach; see Meta.MayCall.
	MayCall         []string
	MayCallDeclared *bool
}

type Meta struct {
	ToolsName  string `json:"toolsName,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	PolicyName string `json:"policyName,omitempty"`
	PolicyPath string `json:"policyPath,omitempty"`
	Diff       string `json:"diff,omitempty"`
	DiffOld    string `json:"diffOld,omitempty"`
	DiffNew    string `json:"diffNew,omitempty"`

	// MayCall is the gated call's declared reach, in order; a declaration, not a proof.
	MayCall []string `json:"mayCall,omitempty"`

	// MayCallDeclared: nil = unknown; true = MayCall exhaustive; false = reaches others, undeclared.
	MayCallDeclared *bool `json:"mayCallDeclared,omitempty"`
}

// IsZero reports whether the envelope carries nothing at all.
func (m Meta) IsZero() bool { return reflect.DeepEqual(m, Meta{}) }

func BuildRequest(req hitlservice.ApprovalRequest, opts BuildOptions) libacp.RequestPermissionRequest {
	toolCallID := ToolCallID(req)
	title := Title(req)
	summary := SummarizeToolCallArgs(req.ToolName, req.Args)
	content := DiffContent(diffPath(req.Args), req.DiffOld, req.DiffNew)
	if content == nil && strings.TrimSpace(req.Diff) != "" {
		cb := libacp.NewTextContent(req.Diff)
		content = []libacp.ToolCallContent{{Type: libacp.ToolCallContentRegular, Content: &cb}}
	}
	if content == nil && summary != "" {
		cb := libacp.NewTextContent(summary)
		content = []libacp.ToolCallContent{{Type: libacp.ToolCallContentRegular, Content: &cb}}
	}

	meta := MarshalMeta(Meta{
		ToolsName:  req.ToolsName,
		ToolName:   req.ToolName,
		PolicyName: opts.PolicyName,
		PolicyPath: opts.PolicyPath,
		Diff:       req.Diff,
		DiffOld:    req.DiffOld,
		DiffNew:    req.DiffNew,

		MayCall:         opts.MayCall,
		MayCallDeclared: opts.MayCallDeclared,
	})

	return libacp.RequestPermissionRequest{
		SessionID: opts.SessionID,
		ToolCall: libacp.PermissionToolCall{
			ToolCallID: toolCallID,
			Title:      title,
			Kind:       ToolKindFor(req.ToolsName, req.ToolName),
			Status:     libacp.ToolCallStatusPending,
			RawInput:   MarshalArgs(req.Args),
			Content:    content,
			Meta:       meta,
		},
		Options: []libacp.PermissionOption{
			{OptionID: OptionAllow, Name: "Allow", Kind: libacp.PermissionAllowOnce},
			{OptionID: OptionDeny, Name: "Deny", Kind: libacp.PermissionRejectOnce},
		},
		Meta: meta,
	}
}

func ToolCallID(req hitlservice.ApprovalRequest) string {
	if req.ToolCallID != "" {
		return req.ToolCallID
	}
	if req.ToolsName != "" && req.ToolName != "" {
		return req.ToolsName + "." + req.ToolName
	}
	if req.ToolName != "" {
		return req.ToolName
	}
	if req.ToolsName != "" {
		return req.ToolsName
	}
	return "tool_call"
}

func Title(req hitlservice.ApprovalRequest) string {
	title := req.ToolsName
	if req.ToolName != "" {
		if title != "" {
			title += "."
		}
		title += req.ToolName
	}
	if title == "" {
		title = "Tool call"
	}
	if summary := SummarizeToolCallArgs(req.ToolName, req.Args); summary != "" && !strings.Contains(title, summary) {
		title += ": " + summary
	}
	return title
}

func ToolKindFor(toolsName, toolName string) libacp.ToolKind {
	switch toolsName {
	case "local_shell":
		return libacp.ToolKindExecute
	case "local_fs":
		return localFSKind(toolName)
	case "webtools":
		return libacp.ToolKindFetch
	}

	base := toolName
	if idx := strings.Index(base, "."); idx >= 0 {
		base = base[idx+1:]
	}
	switch {
	case strings.HasPrefix(base, "read"), strings.HasPrefix(base, "stat"), strings.HasPrefix(base, "list"), strings.HasPrefix(base, "grep"), strings.HasPrefix(base, "find"):
		return libacp.ToolKindRead
	case strings.HasPrefix(base, "write"), strings.HasPrefix(base, "edit"), base == "sed", strings.HasPrefix(base, "patch"):
		return libacp.ToolKindEdit
	case strings.HasPrefix(base, "delete"), strings.HasPrefix(base, "remove"), base == "rm":
		return libacp.ToolKindDelete
	case strings.HasPrefix(base, "move"), strings.HasPrefix(base, "rename"):
		return libacp.ToolKindMove
	case strings.HasPrefix(base, "fetch"), strings.HasPrefix(base, "http"):
		return libacp.ToolKindFetch
	case strings.HasPrefix(toolName, "local_shell."), strings.HasPrefix(base, "exec"), strings.HasPrefix(base, "run"):
		return libacp.ToolKindExecute
	}
	return libacp.ToolKindOther
}

func localFSKind(toolName string) libacp.ToolKind {
	switch {
	case strings.Contains(toolName, "read"), strings.Contains(toolName, "list"), strings.Contains(toolName, "grep"), strings.Contains(toolName, "find"), strings.Contains(toolName, "stat"):
		return libacp.ToolKindRead
	case strings.Contains(toolName, "write"), strings.Contains(toolName, "sed"), strings.Contains(toolName, "edit"), strings.Contains(toolName, "patch"):
		return libacp.ToolKindEdit
	case strings.Contains(toolName, "move"), strings.Contains(toolName, "rename"):
		return libacp.ToolKindMove
	case strings.Contains(toolName, "delete"), strings.Contains(toolName, "remove"), strings.Contains(toolName, "rm"):
		return libacp.ToolKindDelete
	}
	return libacp.ToolKindEdit
}

func SummarizeToolCallArgs(toolName string, args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	base := toolName
	if idx := strings.LastIndex(base, "."); idx >= 0 {
		base = base[idx+1:]
	}
	asString := func(key string) string {
		if v, ok := args[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
		return ""
	}
	asStringSlice := func(key string) []string {
		v, ok := args[key]
		if !ok {
			return nil
		}
		arr, ok := v.([]any)
		if !ok {
			return nil
		}
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}

	var summary string
	switch base {
	case "exec", "run", "execute", "local_shell":
		cmd := asString("command")
		if cmd == "" {
			break
		}
		parts := []string{cmd}
		if a := asString("args"); a != "" {
			parts = append(parts, a)
		} else {
			parts = append(parts, asStringSlice("args")...)
		}
		summary = strings.Join(parts, " ")
	case "read_file", "read_file_range", "write_file", "stat_file", "list_dir", "delete_file":
		summary = asString("path")
	case "grep":
		if p := asString("pattern"); p != "" {
			if path := asString("path"); path != "" {
				summary = p + " in " + path
			} else {
				summary = p
			}
		}
	case "sed":
		if path := asString("path"); path != "" {
			if pat := asString("pattern"); pat != "" {
				summary = pat + " in " + path
			} else {
				summary = path
			}
		}
	case "fetch_url", "fetch", "http_get":
		summary = asString("url")
	}
	if summary == "" {
		for _, key := range []string{"path", "command", "url", "pattern"} {
			if value := asString(key); strings.TrimSpace(value) != "" {
				summary = value
				break
			}
		}
	}
	if summary == "" {
		return ""
	}
	summary = strings.TrimSpace(strings.ReplaceAll(summary, "\n", " "))
	const maxRunes = 80
	if r := []rune(summary); len(r) > maxRunes {
		summary = string(r[:maxRunes-3]) + "..."
	}
	return summary
}

func MarshalArgs(args map[string]any) json.RawMessage {
	if len(args) == 0 {
		return nil
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil
	}
	return raw
}

func MarshalMeta(meta Meta) json.RawMessage {
	if meta.IsZero() {
		return nil
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return nil
	}
	return raw
}

func DiffContent(path, oldText, newText string) []libacp.ToolCallContent {
	if strings.TrimSpace(path) == "" || oldText == newText || (oldText == "" && newText == "") {
		return nil
	}
	return []libacp.ToolCallContent{
		{Type: libacp.ToolCallContentDiff, Path: strings.TrimSpace(path), OldText: oldText, NewText: newText},
	}
}

func diffPath(args map[string]any) string {
	if p, ok := args["path"].(string); ok {
		return strings.TrimSpace(p)
	}
	return ""
}
