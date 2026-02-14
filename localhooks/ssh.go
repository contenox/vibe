package localhooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/contenox/vibe/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
	"golang.org/x/crypto/ssh"
)

// SSHHook implements SSH remote command execution with proper security
type SSHHook struct {
	defaultPort     int
	defaultTimeout  time.Duration
	knownHostsFile  string
	hostKeyCallback ssh.HostKeyCallback
	clientCache     *SSHClientCache
}

// SSHConfig holds SSH connection parameters
type SSHConfig struct {
	Host           string
	Port           int
	User           string
	Password       string
	PrivateKey     string
	PrivateKeyFile string
	Timeout        time.Duration
	HostKey        string // Expected host key for verification
	StrictHostKey  bool   // Whether to require host key verification
}

// SSHResult holds command execution results
type SSHResult struct {
	ExitCode int     `json:"exit_code"`
	Stdout   string  `json:"stdout"`
	Stderr   string  `json:"stderr"`
	Duration float64 `json:"duration_seconds"`
	Command  string  `json:"command"`
	Host     string  `json:"host"`
	Success  bool    `json:"success"`
	Error    string  `json:"error,omitempty"`
	HostKey  string  `json:"host_key,omitempty"` // Fingerprint of connected host
}

// SSHClientCache manages SSH connection pooling with thread safety
type SSHClientCache struct {
	mu      sync.RWMutex
	clients map[string]*ssh.Client
	configs map[string]*ssh.ClientConfig
}

// NewSSHClientCache creates a new SSH client cache
func NewSSHClientCache() *SSHClientCache {
	return &SSHClientCache{
		clients: make(map[string]*ssh.Client),
		configs: make(map[string]*ssh.ClientConfig),
	}
}

// Get retrieves a cached SSH client
func (c *SSHClientCache) Get(key string) (*ssh.Client, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	client, exists := c.clients[key]
	return client, exists
}

// Put stores an SSH client in the cache
func (c *SSHClientCache) Put(key string, client *ssh.Client, config *ssh.ClientConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clients[key] = client
	c.configs[key] = config
}

// Remove removes a client from the cache and closes it
func (c *SSHClientCache) Remove(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if client, exists := c.clients[key]; exists {
		client.Close()
		delete(c.clients, key)
		delete(c.configs, key)
	}
}

// Clear closes all connections and clears the cache
func (c *SSHClientCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, client := range c.clients {
		client.Close()
		delete(c.clients, key)
		delete(c.configs, key)
	}
}

// IsAlive checks if a cached client is still connected
func (c *SSHClientCache) IsAlive(key string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	client, exists := c.clients[key]
	if !exists {
		return false
	}

	// Send a keepalive request to check connection
	_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
	return err == nil
}

// HostKeyVerifier handles host key verification
type HostKeyVerifier struct {
	mu         sync.RWMutex
	knownHosts map[string][]string // host -> []public keys
	strictMode bool
	customKeys map[string]string // host -> expected key
}

// NewHostKeyVerifier creates a new host key verifier
func NewHostKeyVerifier(knownHostsFile string, strict bool) (*HostKeyVerifier, error) {
	verifier := &HostKeyVerifier{
		knownHosts: make(map[string][]string),
		strictMode: strict,
		customKeys: make(map[string]string),
	}

	// Load known hosts file
	if err := verifier.loadKnownHosts(knownHostsFile); err != nil && strict {
		return nil, fmt.Errorf("failed to load known_hosts file: %w", err)
	}

	return verifier, nil
}

// loadKnownHosts parses the known_hosts file
func (v *HostKeyVerifier) loadKnownHosts(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// File doesn't exist, that's OK in non-strict mode
			return nil
		}
		return err
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.Split(line, " ")
		if len(parts) < 3 {
			continue
		}

		hosts := strings.Split(parts[0], ",")
		keyType := parts[1]
		keyData := parts[2]

		for _, host := range hosts {
			if strings.HasPrefix(host, "|") {
				// Hashed host entry, skip for simplicity
				continue
			}
			v.mu.Lock()
			v.knownHosts[host] = append(v.knownHosts[host], keyType+" "+keyData)
			v.mu.Unlock()
		}
	}

	return nil
}

// AddKnownHost adds a custom known host key
func (v *HostKeyVerifier) AddKnownHost(host, key string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.customKeys[host] = key
}

// VerifyHostKey implements ssh.HostKeyCallback
func (v *HostKeyVerifier) VerifyHostKey(hostname string, remote net.Addr, key ssh.PublicKey) error {
	host := hostname
	if strings.Contains(hostname, ":") {
		host = strings.Split(hostname, ":")[0]
	}

	// Get the fingerprint of the presented key
	fingerprint := ssh.FingerprintSHA256(key)

	v.mu.RLock()
	defer v.mu.RUnlock()

	// Check custom keys first
	if expectedKey, exists := v.customKeys[host]; exists {
		if fingerprint == expectedKey || ssh.FingerprintLegacyMD5(key) == expectedKey {
			return nil
		}
		return fmt.Errorf("host key verification failed for %s: expected %s, got %s", host, expectedKey, fingerprint)
	}

	// Check known hosts
	if knownKeys, exists := v.knownHosts[host]; exists {
		for _, knownKey := range knownKeys {
			parts := strings.Split(knownKey, " ")
			if len(parts) != 2 {
				continue
			}

			// Parse the known key
			knownPubKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(knownKey))
			if err != nil {
				continue
			}

			if ssh.FingerprintSHA256(knownPubKey) == fingerprint {
				return nil
			}
		}
	}

	// In strict mode, reject unknown hosts
	if v.strictMode {
		return fmt.Errorf("host %s is not in known_hosts and strict mode is enabled", host)
	}

	// In non-strict mode, we could prompt or log, but for security we'll still reject
	// You could modify this behavior based on your security requirements
	return fmt.Errorf("host %s not in known_hosts (fingerprint: %s)", host, fingerprint)
}

// NewSSHHook creates a new SSH hook with secure defaults
func NewSSHHook(options ...SSHOption) (taskengine.HookRepo, error) {
	// Get user's known_hosts file path
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get user home directory: %w", err)
	}

	defaultKnownHosts := filepath.Join(homeDir, ".ssh", "known_hosts")

	hook := &SSHHook{
		defaultPort:    22,
		defaultTimeout: 30 * time.Second,
		knownHostsFile: defaultKnownHosts,
	}

	// Apply options
	for _, opt := range options {
		if err := opt(hook); err != nil {
			return nil, fmt.Errorf("failed to apply SSH option: %w", err)
		}
	}

	// Initialize host key callback
	if hook.hostKeyCallback == nil {
		verifier, err := NewHostKeyVerifier(hook.knownHostsFile, true) // Default to strict mode
		if err != nil {
			return nil, fmt.Errorf("failed to create host key verifier: %w", err)
		}
		hook.hostKeyCallback = verifier.VerifyHostKey
	}

	return hook, nil
}

// SSHOption configures the SSHHook
type SSHOption func(*SSHHook) error

// WithDefaultPort sets the default SSH port
func WithDefaultPort(port int) SSHOption {
	return func(h *SSHHook) error {
		if port < 1 || port > 65535 {
			return errors.New("port must be between 1 and 65535")
		}
		h.defaultPort = port
		return nil
	}
}

// WithDefaultTimeout sets the default connection/command timeout
func WithDefaultTimeout(timeout time.Duration) SSHOption {
	return func(h *SSHHook) error {
		if timeout <= 0 {
			return errors.New("timeout must be positive")
		}
		h.defaultTimeout = timeout
		return nil
	}
}

// WithKnownHostsFile sets the known_hosts file path
func WithKnownHostsFile(path string) SSHOption {
	return func(h *SSHHook) error {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("known_hosts file not accessible: %w", err)
		}
		h.knownHostsFile = path
		return nil
	}
}

// WithStrictHostKey enables strict host key verification
func WithStrictHostKey() SSHOption {
	return func(h *SSHHook) error {
		verifier, err := NewHostKeyVerifier(h.knownHostsFile, true)
		if err != nil {
			return err
		}
		h.hostKeyCallback = verifier.VerifyHostKey
		return nil
	}
}

// WithCustomHostKeyCallback sets a custom host key verification callback
func WithCustomHostKeyCallback(callback ssh.HostKeyCallback) SSHOption {
	return func(h *SSHHook) error {
		if callback == nil {
			return errors.New("host key callback cannot be nil")
		}
		h.hostKeyCallback = callback
		return nil
	}
}

// WithClientCache enables connection pooling
func WithClientCache() SSHOption {
	return func(h *SSHHook) error {
		h.clientCache = NewSSHClientCache()
		return nil
	}
}

// Exec implements the HookRepo interface
func (h *SSHHook) Exec(ctx context.Context, startTime time.Time, input any, debug bool, hook *taskengine.HookCall) (any, taskengine.DataType, error) {
	config, command, err := h.parseSSHConfig(hook, input)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("failed to parse SSH config: %w", err)
	}

	result, err := h.executeCommand(ctx, config, command)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("SSH command failed: %w", err)
	}

	return result, taskengine.DataTypeJSON, nil
}

// parseSSHConfig extracts SSH configuration from hook arguments and input
func (h *SSHHook) parseSSHConfig(hook *taskengine.HookCall, input any) (*SSHConfig, string, error) {
	config := &SSHConfig{
		Port:          h.defaultPort,
		Timeout:       h.defaultTimeout,
		StrictHostKey: true, // Default to strict mode for security
	}

	var command string

	// Handle different input types
	switch v := input.(type) {
	case map[string]any:
		// Tool call with structured parameters
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
		if strict, ok := v["strict_host_key"]; ok {
			if strictBool, ok := strict.(bool); ok {
				config.StrictHostKey = strictBool
			}
		}
	case string:
		// Direct call - input is the command, config from hook args
		command = v
	default:
		return nil, "", fmt.Errorf("unsupported input type: %T", input)
	}

	// Override with hook arguments (higher priority)
	h.applyHookArgs(config, hook.Args)

	// Validate required fields
	if config.Host == "" {
		return nil, "", errors.New("SSH host is required")
	}
	if config.User == "" {
		return nil, "", errors.New("SSH user is required")
	}
	if command == "" {
		return nil, "", errors.New("SSH command is required")
	}

	return config, command, nil
}

func (h *SSHHook) parsePort(port any) int {
	switch p := port.(type) {
	case float64:
		return int(p)
	case int:
		return p
	case string:
		if portInt, err := strconv.Atoi(p); err == nil {
			return portInt
		}
	}
	return h.defaultPort
}

func (h *SSHHook) parseDuration(timeout any) time.Duration {
	switch t := timeout.(type) {
	case float64:
		return time.Duration(t) * time.Second
	case string:
		if dur, err := time.ParseDuration(t); err == nil {
			return dur
		}
	}
	return h.defaultTimeout
}

func (h *SSHHook) applyHookArgs(config *SSHConfig, args map[string]string) {
	if host, ok := args["host"]; ok {
		config.Host = host
	}
	if port, ok := args["port"]; ok {
		if portInt, err := strconv.Atoi(port); err == nil {
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
		if dur, err := time.ParseDuration(timeout); err == nil {
			config.Timeout = dur
		}
	}
	if hostKey, ok := args["host_key"]; ok {
		config.HostKey = hostKey
	}
	if strict, ok := args["strict_host_key"]; ok {
		if strictBool, err := strconv.ParseBool(strict); err == nil {
			config.StrictHostKey = strictBool
		}
	}
}

// executeCommand establishes SSH connection and runs the command
func (h *SSHHook) executeCommand(ctx context.Context, config *SSHConfig, command string) (*SSHResult, error) {
	start := time.Now()
	result := &SSHResult{
		Command: command,
		Host:    config.Host,
	}

	// Create SSH client config with proper host key verification
	sshConfig, err := h.createSSHConfig(config)
	if err != nil {
		result.Error = err.Error()
		return result, err
	}

	// Establish connection
	var client *ssh.Client
	if h.clientCache != nil {
		client, err = h.getCachedClient(config, sshConfig)
	} else {
		client, err = h.createNewClient(config, sshConfig)
	}
	if err != nil {
		result.Error = err.Error()
		return result, err
	}

	// Only close the connection if it's not from cache
	if h.clientCache == nil {
		defer client.Close()
	}

	// Create session
	session, err := client.NewSession()
	if err != nil {
		result.Error = err.Error()
		return result, err
	}
	defer session.Close()

	// Capture stdout and stderr
	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	// Execute command with timeout
	cmdCtx, cancel := context.WithTimeout(ctx, config.Timeout)
	defer cancel()

	// Run the command in a goroutine to handle timeouts
	cmdDone := make(chan error, 1)
	go func() {
		cmdDone <- session.Run(command)
	}()

	// Wait for command completion or timeout
	var cmdErr error
	select {
	case <-cmdCtx.Done():
		cmdErr = fmt.Errorf("command timed out after %v", config.Timeout)
		session.Close() // Force close session on timeout
	case cmdErr = <-cmdDone:
	}

	duration := time.Since(start).Seconds()
	result.Duration = duration

	// Capture output
	result.Stdout = strings.TrimSpace(stdout.String())
	result.Stderr = strings.TrimSpace(stderr.String())

	// Handle command results
	if cmdErr != nil {
		result.Error = cmdErr.Error()
		// Try to extract exit code
		if exitErr, ok := cmdErr.(*ssh.ExitError); ok {
			result.ExitCode = exitErr.ExitStatus()
		} else {
			result.ExitCode = -1
		}
		result.Success = false
		return result, fmt.Errorf("command failed: %w", cmdErr)
	}

	result.ExitCode = 0
	result.Success = true
	return result, nil
}

// createSSHConfig creates SSH client configuration with secure defaults
func (h *SSHHook) createSSHConfig(config *SSHConfig) (*ssh.ClientConfig, error) {
	sshConfig := &ssh.ClientConfig{
		User:            config.User,
		HostKeyCallback: h.hostKeyCallback, // Secure host key verification
		Timeout:         config.Timeout,
	}

	// Authentication methods
	var authMethods []ssh.AuthMethod

	// Private key authentication
	if config.PrivateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(config.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("failed to parse private key: %w", err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}

	// Private key file authentication
	if config.PrivateKeyFile != "" {
		signer, err := h.parsePrivateKeyFile(config.PrivateKeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to parse private key file: %w", err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}

	// Password authentication
	if config.Password != "" {
		authMethods = append(authMethods, ssh.Password(config.Password))
	}

	if len(authMethods) == 0 {
		return nil, errors.New("no authentication method provided (need password or private key)")
	}

	sshConfig.Auth = authMethods
	return sshConfig, nil
}

// parsePrivateKeyFile reads and parses a private key from file with proper permissions check
func (h *SSHHook) parsePrivateKeyFile(path string) (ssh.Signer, error) {
	// Check file permissions (should be 600 or 400)
	if info, err := os.Stat(path); err == nil {
		if mode := info.Mode(); mode.Perm()&0077 != 0 {
			return nil, fmt.Errorf("private key file %s has overly permissive permissions %04o", path, mode.Perm())
		}
	}

	keyData, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read private key file: %w", err)
	}

	// Try without passphrase first
	signer, err := ssh.ParsePrivateKey(keyData)
	if err != nil {
		// If it's a passphrase-protected key, we'd need to handle that
		if strings.Contains(err.Error(), "passphrase") {
			return nil, fmt.Errorf("passphrase-protected keys not supported in this implementation")
		}
		return nil, err
	}

	return signer, nil
}

// createNewClient creates a new SSH client
func (h *SSHHook) createNewClient(config *SSHConfig, sshConfig *ssh.ClientConfig) (*ssh.Client, error) {
	address := net.JoinHostPort(config.Host, strconv.Itoa(config.Port))
	return ssh.Dial("tcp", address, sshConfig)
}

// getCachedClient retrieves or creates a cached SSH client
func (h *SSHHook) getCachedClient(config *SSHConfig, sshConfig *ssh.ClientConfig) (*ssh.Client, error) {
	cacheKey := fmt.Sprintf("%s@%s:%d", config.User, config.Host, config.Port)

	// Check cache first
	if client, exists := h.clientCache.Get(cacheKey); exists {
		if h.clientCache.IsAlive(cacheKey) {
			return client, nil
		}
		// Remove dead connection from cache
		h.clientCache.Remove(cacheKey)
	}

	// Create new connection
	client, err := h.createNewClient(config, sshConfig)
	if err != nil {
		return nil, err
	}

	// Store in cache
	h.clientCache.Put(cacheKey, client, sshConfig)
	return client, nil
}

// Supports returns the hook types supported by this hook
func (h *SSHHook) Supports(ctx context.Context) ([]string, error) {
	return []string{"ssh"}, nil
}

// GetSchemasForSupportedHooks returns OpenAPI schemas for supported hooks
// GetSchemasForSupportedHooks returns OpenAPI schemas for supported hooks
func (h *SSHHook) GetSchemasForSupportedHooks(ctx context.Context) (map[string]*openapi3.T, error) {
	// Create a complete OpenAPI schema for the SSH hook
	schema := &openapi3.T{
		OpenAPI: "3.1.0",
		Info: &openapi3.Info{
			Title:       "SSH Remote Command Execution Hook",
			Description: "Execute commands on remote servers via SSH with secure host key verification",
			Version:     "1.0.0",
			Contact: &openapi3.Contact{
				Name:  "Task Engine Team",
				Email: "dev@example.com",
			},
		},
		Paths: openapi3.NewPaths(),
		Components: &openapi3.Components{
			Schemas:         make(map[string]*openapi3.SchemaRef),
			SecuritySchemes: make(map[string]*openapi3.SecuritySchemeRef),
		},
	}

	// Define SSHExecuteRequest schema
	schema.Components.Schemas["SSHExecuteRequest"] = &openapi3.SchemaRef{
		Value: &openapi3.Schema{
			Type: &openapi3.Types{openapi3.TypeObject},
			Properties: map[string]*openapi3.SchemaRef{
				"host": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeString},
						Description: "Remote hostname or IP address",
					},
				},
				"port": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeInteger},
						Description: fmt.Sprintf("SSH port (default: %d)", h.defaultPort),
						Min:         openapi3.Float64Ptr(1),
						Max:         openapi3.Float64Ptr(65535),
					},
				},
				"user": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeString},
						Description: "SSH username",
					},
				},
				"password": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeString},
						Description: "SSH password for authentication",
						Format:      "password",
					},
				},
				"private_key": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeString},
						Description: "SSH private key content for key-based authentication",
					},
				},
				"private_key_file": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeString},
						Description: "Path to SSH private key file",
					},
				},
				"command": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeString},
						Description: "Command to execute on the remote host",
					},
				},
				"timeout": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeString},
						Description: fmt.Sprintf("Command timeout duration (default: %v)", h.defaultTimeout),
						Pattern:     `^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$`,
					},
				},
				"host_key": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeString},
						Description: "Expected host key fingerprint for verification (SHA256 format)",
						Pattern:     `^SHA256:[A-Za-z0-9+/=]+$`,
					},
				},
				"strict_host_key": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeBoolean},
						Description: "Enable strict host key verification",
					},
				},
			},
			Required: []string{"host", "user", "command"},
		},
	}

	// Define SSHExecuteResponse schema
	schema.Components.Schemas["SSHExecuteResponse"] = &openapi3.SchemaRef{
		Value: &openapi3.Schema{
			Type: &openapi3.Types{openapi3.TypeObject},
			Properties: map[string]*openapi3.SchemaRef{
				"exit_code": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeInteger},
						Description: "Exit code from the remote command",
					},
				},
				"stdout": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeString},
						Description: "Standard output from the command",
					},
				},
				"stderr": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeString},
						Description: "Standard error from the command",
					},
				},
				"duration_seconds": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeNumber},
						Description: "Command execution time in seconds",
						Format:      "float",
					},
				},
				"command": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeString},
						Description: "The executed command",
					},
				},
				"host": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeString},
						Description: "Target host",
					},
				},
				"success": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeBoolean},
						Description: "Whether the command executed successfully",
					},
				},
				"error": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeString},
						Description: "Error message if command failed",
					},
				},
				"host_key": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeString},
						Description: "Host key fingerprint of the connected server",
					},
				},
			},
		},
	}

	// Define security schemes
	schema.Components.SecuritySchemes["SSHKeyAuth"] = &openapi3.SecuritySchemeRef{
		Value: &openapi3.SecurityScheme{
			Type:        "apiKey",
			Description: "SSH Private Key authentication",
			In:          "header",
			Name:        "X-SSH-Private-Key",
		},
	}

	schema.Components.SecuritySchemes["SSHPasswordAuth"] = &openapi3.SecuritySchemeRef{
		Value: &openapi3.SecurityScheme{
			Type:        "http",
			Scheme:      "basic",
			Description: "SSH Password authentication",
		},
	}

	// Create error response schema
	errorSchema := &openapi3.SchemaRef{
		Value: &openapi3.Schema{
			Type: &openapi3.Types{openapi3.TypeObject},
			Properties: map[string]*openapi3.SchemaRef{
				"error": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeString},
						Description: "Error description",
					},
				},
				"details": {
					Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeString},
						Description: "Detailed error information",
					},
				},
			},
		},
	}

	// Add the execute_remote_command operation
	schema.Paths.Set("/execute_remote_command", &openapi3.PathItem{
		Post: &openapi3.Operation{
			OperationID: "executeRemoteCommand",
			Summary:     "Execute a command on a remote server via SSH",
			Description: "Securely execute commands on remote servers with host key verification and multiple authentication methods",
			Tags:        []string{"SSH"},
			RequestBody: &openapi3.RequestBodyRef{
				Value: &openapi3.RequestBody{
					Description: "SSH command execution request",
					Required:    true,
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
	descr200 := "Command executed successfully"

	// Add responses to the operation
	operation := schema.Paths.Value("/execute_remote_command").Post
	operation.Responses.Set("200", &openapi3.ResponseRef{
		Value: &openapi3.Response{
			Description: &descr200,
			Content: openapi3.NewContentWithSchemaRef(
				schema.Components.Schemas["SSHExecuteResponse"],
				[]string{"application/json"},
			),
		},
	})
	descr400 := "Bad request - invalid parameters"
	operation.Responses.Set("400", &openapi3.ResponseRef{
		Value: &openapi3.Response{
			Description: &descr400,
			Content: openapi3.NewContentWithSchemaRef(
				errorSchema,
				[]string{"application/json"},
			),
		},
	})
	descr500 := "Internal server error or SSH connection failed"
	operation.Responses.Set("500", &openapi3.ResponseRef{
		Value: &openapi3.Response{
			Description: &descr500,
			Content: openapi3.NewContentWithSchemaRef(
				errorSchema,
				[]string{"application/json"},
			),
		},
	})

	// Add security requirements
	operation.Security.With(openapi3.SecurityRequirement{
		"SSHKeyAuth":      {},
		"SSHPasswordAuth": {},
	})

	return map[string]*openapi3.T{"ssh": schema}, nil
}

// GetToolsForHookByName returns tools exposed by this hook
func (h *SSHHook) GetToolsForHookByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	if name != "ssh" {
		return nil, fmt.Errorf("unknown hook: %s", name)
	}

	return []taskengine.Tool{
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        "execute_remote_command",
				Description: "Execute commands on remote hosts via SSH with secure host key verification",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"host": map[string]interface{}{
							"type":        "string",
							"description": "Remote hostname or IP address",
						},
						"port": map[string]interface{}{
							"type":        "integer",
							"description": fmt.Sprintf("SSH port (optional, default %d)", h.defaultPort),
							"default":     h.defaultPort,
							"minimum":     1,
							"maximum":     65535,
						},
						"user": map[string]interface{}{
							"type":        "string",
							"description": "SSH username",
						},
						"password": map[string]interface{}{
							"type":        "string",
							"description": "SSH password (if using password auth)",
						},
						"private_key": map[string]interface{}{
							"type":        "string",
							"description": "SSH private key content (if using key-based auth)",
						},
						"private_key_file": map[string]interface{}{
							"type":        "string",
							"description": "Path to SSH private key file (if using key-based auth)",
						},
						"command": map[string]interface{}{
							"type":        "string",
							"description": "Command to execute on the remote host",
						},
						"timeout": map[string]interface{}{
							"type":        "string",
							"description": fmt.Sprintf("Command timeout (optional, default %v)", h.defaultTimeout),
							"default":     h.defaultTimeout.String(),
						},
						"host_key": map[string]interface{}{
							"type":        "string",
							"description": "Expected host key fingerprint for verification",
						},
						"strict_host_key": map[string]interface{}{
							"type":        "boolean",
							"description": "Enable strict host key verification (default: true)",
							"default":     true,
						},
					},
					"required": []string{"host", "user", "command"},
				},
			},
		},
	}, nil
}

// Close cleans up any resources
func (h *SSHHook) Close() error {
	if h.clientCache != nil {
		h.clientCache.Clear()
	}
	return nil
}

var _ taskengine.HookRepo = (*SSHHook)(nil)
