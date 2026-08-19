package sshtool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/vfs"
	"golang.org/x/crypto/ssh"
)

// ToolsProviderName is the registered toolset name an allowlist addresses; the
// native- scope is a namespace, so a declared MCP source cannot mint the same
// key.
const ToolsProviderName = "native-ssh"

// ToolExecuteRemoteCommand is the one tool; it is unprefixed because the
// namespace scopes toolsets, not tools, and this is the seeded HITL policy key.
const ToolExecuteRemoteCommand = "execute_remote_command"

var toolNames = []string{ToolExecuteRemoteCommand}

const (
	defaultPort           = 22
	defaultTimeout        = 30 * time.Second
	maxTimeout            = 30 * time.Minute
	defaultOutputByteCap  = 2 * 1024 * 1024
	fingerprintSHA256Pfx  = "SHA256:"
	severityRecoverable   = "(recoverable: adjust parameters and retry)"
	severityFatalToken    = "(fatal:"
	knownHostsRelativeDir = ".ssh"
)

var (
	// ErrNoAllowedHosts means nothing enumerated a reachable host, so the
	// toolset can reach none; declaring it is not by itself consent to a machine.
	ErrNoAllowedHosts = errors.New("ssh: no host allowlist")

	// ErrHostNotAllowed means the requested host is outside the enumerated allowlist.
	ErrHostNotAllowed = errors.New("ssh: host not in the allowlist")

	// ErrHostKeyMismatch means the server presented a key other than the pinned one.
	ErrHostKeyMismatch = errors.New("ssh: host key mismatch")
)

type SSHTools struct {
	name            string
	defaultPort     int
	defaultTimeout  time.Duration
	knownHostsFile  string
	strict          bool
	hostKeyCallback ssh.HostKeyCallback
	allowedHosts    []hostPattern
	clientCache     *clientCache
	fileRoot        string
}

type SSHConfig struct {
	Host           string
	Port           int
	User           string
	Password       string
	PrivateKey     string
	PrivateKeyFile string
	Timeout        time.Duration
	// HostKey is an OPTIONAL per-call SHA256 fingerprint pin; it is checked in
	// addition to known_hosts, never instead of it.
	HostKey string
}

type SSHResult struct {
	ExitCode  int     `json:"exit_code"`
	Stdout    string  `json:"stdout"`
	Stderr    string  `json:"stderr"`
	Duration  float64 `json:"duration_seconds"`
	Command   string  `json:"command"`
	Host      string  `json:"host"`
	User      string  `json:"user"`
	Success   bool    `json:"success"`
	Truncated bool    `json:"truncated,omitempty"`
	Error     string  `json:"error,omitempty"`
	HostKey   string  `json:"host_key,omitempty"`
}

// SSHOption configures the toolset; an option that cannot be satisfied fails
// construction rather than degrading a security boundary silently.
type SSHOption func(*SSHTools) error

func WithName(name string) SSHOption {
	return func(h *SSHTools) error {
		if strings.TrimSpace(name) == "" {
			return errors.New("ssh: toolset name cannot be empty")
		}
		h.name = strings.TrimSpace(name)
		return nil
	}
}

func WithDefaultPort(port int) SSHOption {
	return func(h *SSHTools) error {
		if port < 1 || port > 65535 {
			return errors.New("ssh: port must be between 1 and 65535")
		}
		h.defaultPort = port
		return nil
	}
}

func WithDefaultTimeout(timeout time.Duration) SSHOption {
	return func(h *SSHTools) error {
		if timeout <= 0 {
			return errors.New("ssh: timeout must be positive")
		}
		if timeout > maxTimeout {
			return fmt.Errorf("ssh: timeout %v exceeds the %v ceiling", timeout, maxTimeout)
		}
		h.defaultTimeout = timeout
		return nil
	}
}

func WithKnownHostsFile(path string) SSHOption {
	return func(h *SSHTools) error {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("ssh: known_hosts file not accessible: %w", err)
		}
		h.knownHostsFile = path
		return nil
	}
}

// WithStrictHostKey records the intent; the verifier is built once after every
// option has run, so this no longer depends on being ordered after
// WithKnownHostsFile.
func WithStrictHostKey() SSHOption {
	return func(h *SSHTools) error {
		h.strict = true
		return nil
	}
}

func WithCustomHostKeyCallback(callback ssh.HostKeyCallback) SSHOption {
	return func(h *SSHTools) error {
		if callback == nil {
			return errors.New("ssh: host key callback cannot be nil")
		}
		h.hostKeyCallback = callback
		return nil
	}
}

// WithAllowedHosts is the operator's ceiling on reach: every call must match one
// of these entries, and a declaration's own allowlist can only narrow within it.
func WithAllowedHosts(entries ...string) SSHOption {
	return func(h *SSHTools) error {
		patterns, err := parseHostPatterns(entries)
		if err != nil {
			return err
		}
		h.allowedHosts = patterns
		return nil
	}
}

func WithClientCache() SSHOption {
	return func(h *SSHTools) error {
		h.clientCache = newClientCache()
		return nil
	}
}

// WithFileRoot scopes every local file this toolset reads — the known_hosts file
// and any private key file — to dir: a read that resolves outside it is refused.
// It is the operator's ceiling on local file reach, the same containment seam
// local_fs uses. Unset, only the control-plane deny binds (see containFile).
func WithFileRoot(dir string) SSHOption {
	return func(h *SSHTools) error {
		if strings.TrimSpace(dir) == "" {
			return errors.New("ssh: file root cannot be empty")
		}
		resolved, err := vfs.ResolveRoot(dir)
		if err != nil {
			return fmt.Errorf("ssh: invalid file root: %w", err)
		}
		h.fileRoot = resolved
		return nil
	}
}

// NewSSHTools fails rather than returning a toolset whose host key verification
// could not be established.
func NewSSHTools(options ...SSHOption) (taskengine.ToolsRepo, error) {
	tools := &SSHTools{
		name:           ToolsProviderName,
		defaultPort:    defaultPort,
		defaultTimeout: defaultTimeout,
		strict:         true,
	}

	homeDir, homeErr := os.UserHomeDir()
	if homeErr == nil {
		tools.knownHostsFile = filepath.Join(homeDir, knownHostsRelativeDir, "known_hosts")
	}

	for _, opt := range options {
		if opt == nil {
			continue
		}
		if err := opt(tools); err != nil {
			return nil, err
		}
	}

	if tools.hostKeyCallback == nil {
		if tools.knownHostsFile == "" {
			return nil, fmt.Errorf("ssh: no known_hosts file could be located (%w); set one with WithKnownHostsFile", homeErr)
		}
		safeKnownHosts, err := tools.containFile("known_hosts file", tools.knownHostsFile)
		if err != nil {
			return nil, err
		}
		tools.knownHostsFile = safeKnownHosts
		verifier, err := NewHostKeyVerifier(tools.knownHostsFile, tools.strict)
		if err != nil {
			return nil, err
		}
		tools.hostKeyCallback = verifier.VerifyHostKey
	}

	return tools, nil
}

// containFile routes a local file this toolset reads (known_hosts, a private key
// file) through the shared vfs seam and returns the resolved path. The
// control-plane deny always binds; a path escaping WithFileRoot is refused once
// one is set. With no root, the read is contained within the file's own dir, so
// only the control-plane deny applies.
func (h *SSHTools) containFile(kind, path string) (string, error) {
	abs := path
	if !filepath.IsAbs(abs) {
		if a, err := filepath.Abs(abs); err == nil {
			abs = a
		}
	}
	root := h.fileRoot
	if root == "" {
		root = filepath.Dir(abs)
	}
	resolved, err := vfs.Contain(root, abs)
	if err != nil {
		switch {
		case errors.Is(err, vfs.ErrControlPlane):
			return "", fatalf("control plane", "ssh: the %s %s is inside the runtime control plane, which is never read as SSH material", kind, path)
		case errors.Is(err, vfs.ErrEscape):
			return "", fatalf("outside SSH file root", "ssh: the %s %s escapes the configured SSH file root %s", kind, path, h.fileRoot)
		default:
			return "", fmt.Errorf("ssh: cannot resolve the %s %s: %w", kind, path, err)
		}
	}
	return resolved, nil
}

func (h *SSHTools) Exec(ctx context.Context, _ time.Time, input any, _ bool, tools *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	res, dt, err := h.execDispatch(ctx, input, tools)
	return res, dt, markSeverity(err)
}

func (h *SSHTools) execDispatch(ctx context.Context, input any, tools *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	config, command, err := h.prepare(ctx, input, tools)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	result, err := h.executeCommand(ctx, config, command)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	return result, taskengine.DataTypeJSON, nil
}

var _ taskengine.Prechecker = (*SSHTools)(nil)

// Precheck refuses a host the allowlist does not name without opening a
// connection, so an out-of-scope call never costs a human an approval decision;
// it is an early copy, never a replacement — Exec applies the same boundary.
func (h *SSHTools) Precheck(ctx context.Context, input any, tools *taskengine.ToolsCall) error {
	_, _, err := h.prepare(ctx, input, tools)
	return markSeverity(err)
}

func (h *SSHTools) prepare(ctx context.Context, input any, tools *taskengine.ToolsCall) (*SSHConfig, string, error) {
	if tools == nil {
		return nil, "", errors.New("ssh: tools required")
	}
	toolName := tools.ToolName
	if toolName == "" {
		toolName = tools.Name
	}
	if toolName != ToolExecuteRemoteCommand && toolName != h.name {
		return nil, "", fmt.Errorf("ssh: unknown tool %q; this toolset provides %s", toolName, strings.Join(toolNames, ", "))
	}

	config, command, err := h.parseSSHConfig(tools, input)
	if err != nil {
		return nil, "", err
	}
	if err := h.checkHostAllowed(ctx, config); err != nil {
		return nil, "", err
	}
	return config, command, nil
}

func (h *SSHTools) parseSSHConfig(tools *taskengine.ToolsCall, input any) (*SSHConfig, string, error) {
	config := &SSHConfig{
		Port:    h.defaultPort,
		Timeout: h.defaultTimeout,
	}

	var command string

	switch v := input.(type) {
	case map[string]any:
		if err := rejectUnknownArgs(h.name+"."+ToolExecuteRemoteCommand, v,
			"host", "port", "user", "password", "private_key", "private_key_file",
			"command", "timeout", "host_key",
		); err != nil {
			return nil, "", err
		}
		if cmd, ok := v["command"].(string); ok {
			command = cmd
		}
		if host, ok := v["host"].(string); ok {
			config.Host = host
		}
		if port, ok := v["port"]; ok {
			config.Port = h.parsePort(port)
		}
		if user, ok := v["user"].(string); ok {
			config.User = user
		}
		if password, ok := v["password"].(string); ok {
			config.Password = password
		}
		if key, ok := v["private_key"].(string); ok {
			config.PrivateKey = key
		}
		if keyFile, ok := v["private_key_file"].(string); ok {
			config.PrivateKeyFile = keyFile
		}
		if timeout, ok := v["timeout"]; ok {
			config.Timeout = h.parseDuration(timeout)
		}
		if hostKey, ok := v["host_key"].(string); ok {
			config.HostKey = hostKey
		}
	case string:
		// A bare string input is the command; the rest of the config comes from
		// the chain's static tools args.
		command = v
	case nil:
	default:
		return nil, "", fmt.Errorf("ssh: unsupported input type: %T", input)
	}

	// The chain's static arguments are authored by a human and outrank whatever
	// the model put in the call.
	if tools.Args != nil {
		h.applyToolsArgs(config, tools.Args)
	}

	if config.Host == "" {
		return nil, "", errors.New("ssh: `host` is required")
	}
	if config.User == "" {
		return nil, "", errors.New("ssh: `user` is required")
	}
	if command == "" {
		return nil, "", errors.New("ssh: `command` is required")
	}
	if config.HostKey != "" && !strings.HasPrefix(config.HostKey, fingerprintSHA256Pfx) {
		return nil, "", fmt.Errorf("ssh: `host_key` %q is not a SHA256 fingerprint; it must read %sBASE64 as `ssh-keyscan` and this tool's own result report it",
			config.HostKey, fingerprintSHA256Pfx)
	}
	if config.Port < 1 || config.Port > 65535 {
		return nil, "", fmt.Errorf("ssh: `port` %d is not between 1 and 65535", config.Port)
	}
	if config.Timeout <= 0 {
		config.Timeout = h.defaultTimeout
	}
	if config.Timeout > maxTimeout {
		config.Timeout = maxTimeout
	}

	return config, command, nil
}

func (h *SSHTools) parsePort(port any) int {
	switch p := port.(type) {
	case float64:
		return int(p)
	case int:
		return p
	case int64:
		return int(p)
	case string:
		if portInt, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			return portInt
		}
	}
	return h.defaultPort
}

func (h *SSHTools) parseDuration(timeout any) time.Duration {
	switch t := timeout.(type) {
	case float64:
		return time.Duration(t) * time.Second
	case int:
		return time.Duration(t) * time.Second
	case string:
		if dur, err := time.ParseDuration(strings.TrimSpace(t)); err == nil {
			return dur
		}
	}
	return h.defaultTimeout
}

func (h *SSHTools) applyToolsArgs(config *SSHConfig, args map[string]string) {
	if host, ok := args["host"]; ok {
		config.Host = host
	}
	if port, ok := args["port"]; ok {
		if portInt, err := strconv.Atoi(strings.TrimSpace(port)); err == nil {
			config.Port = portInt
		}
	}
	if user, ok := args["user"]; ok {
		config.User = user
	}
	if password, ok := args["password"]; ok {
		config.Password = password
	}
	if key, ok := args["private_key"]; ok {
		config.PrivateKey = key
	}
	if keyFile, ok := args["private_key_file"]; ok {
		config.PrivateKeyFile = keyFile
	}
	if timeout, ok := args["timeout"]; ok {
		if dur, err := time.ParseDuration(strings.TrimSpace(timeout)); err == nil {
			config.Timeout = dur
		}
	}
	if hostKey, ok := args["host_key"]; ok {
		config.HostKey = hostKey
	}
}

func (h *SSHTools) Supports(context.Context) ([]string, error) {
	return append([]string{h.name}, toolNames...), nil
}

func (h *SSHTools) Close() error {
	if h.clientCache != nil {
		h.clientCache.Clear()
	}
	return nil
}

func rejectUnknownArgs(toolName string, args map[string]any, allowed ...string) error {
	if len(args) == 0 {
		return nil
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}

	var unknown []string
	for key := range args {
		if _, ok := allowedSet[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}

	sort.Strings(unknown)
	sort.Strings(allowed)
	return fmt.Errorf("%s: unknown argument(s): %s (allowed: %s)",
		toolName, strings.Join(unknown, ", "), strings.Join(allowed, ", "))
}

func markSeverity(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), severityRecoverable) || strings.Contains(err.Error(), severityFatalToken) {
		return err
	}
	return fmt.Errorf("%w %s", err, severityRecoverable)
}

func fatalf(reason, format string, a ...any) error {
	return fmt.Errorf("%s (fatal: %s)", fmt.Sprintf(format, a...), reason)
}

var _ taskengine.ToolsRepo = (*SSHTools)(nil)
