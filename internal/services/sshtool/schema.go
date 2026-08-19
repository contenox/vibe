package sshtool

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// The description states the one thing nothing else re-teaches: this toolset is
// the only native one that does NOT act on the agent host. It reaches another
// machine, so the declaration that admits it must also enumerate which machine,
// and neither the model's arguments nor the toolset itself can widen that.
const executeRemoteCommandDoc = "Run a shell command on a REMOTE host over SSH and return its exit code, stdout and stderr. " +
	"This does NOT run on the agent host — use local_shell for that; every call leaves this machine and lands on another one, under the account you name in `user`. " +
	"Reachable hosts are ENUMERATED by the agent declaration (tools_policies." + ToolsProviderName + "._allowed_hosts): a host outside that list is refused before any connection is opened, and no argument widens it. " +
	"The host key is verified against known_hosts on every connection; `host_key` can pin an exact SHA256 fingerprint on top of that, and verification cannot be turned off. " +
	"A non-zero exit is a RESULT, not a failure: stdout, stderr and exit_code all come back, so read them before retrying. " +
	"Long or interactive commands do not work — there is no TTY and no stdin, and the command is cut off at the timeout."

type sshProperty struct {
	name        string
	typ         string
	description string
	required    bool
}

// sshProperties is the single source of truth for the argument set, rendered
// into both the model-facing descriptor and the published contract so the two
// cannot drift.
func (h *SSHTools) sshProperties() []sshProperty {
	return []sshProperty{
		{
			name:        "host",
			typ:         "string",
			description: "Hostname or IP of the remote machine. It must be named by the declaration's _allowed_hosts, or the call is refused.",
			required:    true,
		},
		{
			name:        "user",
			typ:         "string",
			description: "Account to log in as on the remote machine.",
			required:    true,
		},
		{
			name:        "command",
			typ:         "string",
			description: "The command line to run, executed by the remote login shell. No TTY and no stdin are attached, so anything interactive hangs until the timeout.",
			required:    true,
		},
		{
			name:        "port",
			typ:         "integer",
			description: fmt.Sprintf("SSH port (default %d).", h.defaultPort),
		},
		{
			name:        "password",
			typ:         "string",
			description: "Password authentication. Supply exactly one of password, private_key or private_key_file.",
		},
		{
			name:        "private_key",
			typ:         "string",
			description: "PEM private key material for key authentication. Passphrase-protected keys are not supported.",
		},
		{
			name:        "private_key_file",
			typ:         "string",
			description: "Path on THIS machine to a private key file, which must be mode 600.",
		},
		{
			name:        "timeout",
			typ:         "string",
			description: fmt.Sprintf("Command timeout as a Go duration such as \"90s\" (default %v, ceiling %v). On expiry the result carries what was produced so far.", h.defaultTimeout, maxTimeout),
		},
		{
			name:        "host_key",
			typ:         "string",
			description: "Expected host key fingerprint, \"SHA256:...\", checked in ADDITION to known_hosts. Use it to pin a host; it cannot be used to skip verification.",
		},
	}
}

func (h *SSHTools) sshRequired() []string {
	var out []string
	for _, p := range h.sshProperties() {
		if p.required {
			out = append(out, p.name)
		}
	}
	return out
}

func (h *SSHTools) GetToolsForToolsByName(_ context.Context, name string) ([]taskengine.Tool, error) {
	if name != h.name {
		return nil, fmt.Errorf("ssh: unknown toolset %q; this repo registers %q", name, h.name)
	}

	properties := map[string]any{}
	for _, p := range h.sshProperties() {
		properties[p.name] = map[string]any{
			"type":        p.typ,
			"description": p.description,
		}
	}

	return []taskengine.Tool{
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        ToolExecuteRemoteCommand,
				Description: executeRemoteCommandDoc,
				Parameters: map[string]any{
					"type":       "object",
					"properties": properties,
					"required":   h.sshRequired(),
				},
			},
		},
	}, nil
}

func (h *SSHTools) GetSchemasForSupportedTools(_ context.Context) (map[string]*openapi3.T, error) {
	request := &openapi3.Schema{
		Type:       &openapi3.Types{openapi3.TypeObject},
		Properties: map[string]*openapi3.SchemaRef{},
		Required:   h.sshRequired(),
	}
	for _, p := range h.sshProperties() {
		field := &openapi3.Schema{
			Type:        &openapi3.Types{p.typ},
			Description: p.description,
		}
		switch p.name {
		case "port":
			field.Min = openapi3.Float64Ptr(1)
			field.Max = openapi3.Float64Ptr(65535)
		case "password", "private_key":
			field.Format = "password"
		case "host_key":
			field.Pattern = `^SHA256:[A-Za-z0-9+/=]+$`
		case "timeout":
			field.Pattern = `^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$`
		}
		request.Properties[p.name] = &openapi3.SchemaRef{Value: field}
	}

	response := &openapi3.Schema{
		Type: &openapi3.Types{openapi3.TypeObject},
		Properties: map[string]*openapi3.SchemaRef{
			"exit_code":        schemaField(openapi3.TypeInteger, "Exit status of the remote command; -1 when it never produced one."),
			"stdout":           schemaField(openapi3.TypeString, "Standard output, trailing newlines trimmed."),
			"stderr":           schemaField(openapi3.TypeString, "Standard error, trailing newlines trimmed."),
			"duration_seconds": schemaField(openapi3.TypeNumber, "Wall-clock time the call took, including connection setup."),
			"command":          schemaField(openapi3.TypeString, "The command that was run."),
			"host":             schemaField(openapi3.TypeString, "The remote host it ran on."),
			"user":             schemaField(openapi3.TypeString, "The account it ran as."),
			"success":          schemaField(openapi3.TypeBoolean, "True only when the command exited zero and nothing was truncated."),
			"truncated":        schemaField(openapi3.TypeBoolean, "True when output was cut at the context budget; the shown bytes are the head of each stream."),
			"error":            schemaField(openapi3.TypeString, "Why the call did not succeed, when it did not."),
			"host_key":         schemaField(openapi3.TypeString, "SHA256 fingerprint the remote host presented and that verification accepted."),
		},
	}

	errorSchema := &openapi3.SchemaRef{
		Value: &openapi3.Schema{
			Type: &openapi3.Types{openapi3.TypeObject},
			Properties: map[string]*openapi3.SchemaRef{
				"error": schemaField(openapi3.TypeString, "Error description."),
			},
		},
	}

	schema := &openapi3.T{
		OpenAPI: "3.1.0",
		Info: &openapi3.Info{
			Title:       "SSH remote command execution",
			Description: executeRemoteCommandDoc,
			Version:     "1.0.0",
		},
		Paths: openapi3.NewPaths(),
		Components: &openapi3.Components{
			Schemas: map[string]*openapi3.SchemaRef{
				"SSHExecuteRequest":  {Value: request},
				"SSHExecuteResponse": {Value: response},
			},
			SecuritySchemes: map[string]*openapi3.SecuritySchemeRef{
				"SSHKeyAuth": {Value: &openapi3.SecurityScheme{
					Type:        "apiKey",
					In:          "header",
					Name:        "X-SSH-Private-Key",
					Description: "SSH private key authentication",
				}},
				"SSHPasswordAuth": {Value: &openapi3.SecurityScheme{
					Type:        "http",
					Scheme:      "basic",
					Description: "SSH password authentication",
				}},
			},
		},
	}

	schema.Paths.Set("/"+ToolExecuteRemoteCommand, &openapi3.PathItem{
		Post: &openapi3.Operation{
			OperationID: "executeRemoteCommand",
			Summary:     "Run a command on a remote host over SSH",
			Description: executeRemoteCommandDoc,
			Tags:        []string{"SSH"},
			RequestBody: &openapi3.RequestBodyRef{
				Value: &openapi3.RequestBody{
					Required: true,
					Content: openapi3.NewContentWithSchemaRef(
						schema.Components.Schemas["SSHExecuteRequest"],
						[]string{"application/json"},
					),
				},
			},
			Responses: openapi3.NewResponses(),
			Security:  openapi3.NewSecurityRequirements(),
		},
	})

	operation := schema.Paths.Value("/" + ToolExecuteRemoteCommand).Post
	descr200 := "The command ran; read exit_code to see how it went"
	operation.Responses.Set("200", &openapi3.ResponseRef{
		Value: &openapi3.Response{
			Description: &descr200,
			Content: openapi3.NewContentWithSchemaRef(
				schema.Components.Schemas["SSHExecuteResponse"],
				[]string{"application/json"},
			),
		},
	})
	descr400 := "Refused: bad arguments, or a host outside the declaration's allowlist"
	operation.Responses.Set("400", &openapi3.ResponseRef{
		Value: &openapi3.Response{
			Description: &descr400,
			Content:     openapi3.NewContentWithSchemaRef(errorSchema, []string{"application/json"}),
		},
	})
	descr500 := "The connection, authentication or host key verification failed"
	operation.Responses.Set("500", &openapi3.ResponseRef{
		Value: &openapi3.Response{
			Description: &descr500,
			Content:     openapi3.NewContentWithSchemaRef(errorSchema, []string{"application/json"}),
		},
	})
	operation.Security.With(openapi3.SecurityRequirement{
		"SSHKeyAuth":      {},
		"SSHPasswordAuth": {},
	})

	return map[string]*openapi3.T{h.name: schema}, nil
}

func schemaField(typ, description string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:        &openapi3.Types{typ},
		Description: description,
	}}
}
