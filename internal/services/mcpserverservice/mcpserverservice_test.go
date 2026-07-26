package mcpserverservice

import (
	"strings"
	"testing"

	"github.com/contenox/beam/internal/store/runtimetypes"
)

func TestUnit_MCPServerService_ValidateTransportRequirements(t *testing.T) {
	tests := []struct {
		name      string
		srv       *runtimetypes.MCPServer
		wantError string
	}{
		{
			name:      "stdio requires command",
			srv:       &runtimetypes.MCPServer{Name: "test", Transport: "stdio"},
			wantError: "command is required for stdio transport",
		},
		{
			name:      "sse requires url",
			srv:       &runtimetypes.MCPServer{Name: "test", Transport: "sse"},
			wantError: "url is required for sse transport",
		},
		{
			name:      "unknown transport fails closed",
			srv:       &runtimetypes.MCPServer{Name: "test", Transport: "grpc"},
			wantError: "unknown transport",
		},
		{
			name:      "valid stdio",
			srv:       &runtimetypes.MCPServer{Name: "test", Transport: "stdio", Command: "npx"},
			wantError: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validate(tt.srv)
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("expected error containing %q, got: %v", tt.wantError, err)
				}
			}
		})
	}
}
