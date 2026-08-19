package sshtool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// HostKeyVerifier checks a presented key against known_hosts plus any pinned
// fingerprints. An unknown host is always refused: there is no safe default
// accept for an unverified host key, so non-strict only decides whether an
// unreadable known_hosts file fails construction.
type HostKeyVerifier struct {
	mu         sync.RWMutex
	path       string
	callback   ssh.HostKeyCallback
	customKeys map[string]string
}

func NewHostKeyVerifier(knownHostsFile string, strict bool) (*HostKeyVerifier, error) {
	verifier := &HostKeyVerifier{path: knownHostsFile, customKeys: make(map[string]string)}

	if _, err := os.Stat(knownHostsFile); err != nil {
		if strict {
			return nil, fmt.Errorf("ssh: cannot read known_hosts %s: %w", knownHostsFile, err)
		}
		return verifier, nil
	}

	// knownhosts parses hashed entries, @revoked and @cert-authority markers and
	// the [host]:port form; the hand-rolled parser this replaces silently skipped
	// hashed entries, which is how OpenSSH writes the file by default.
	callback, err := knownhosts.New(knownHostsFile)
	if err != nil {
		if strict {
			return nil, fmt.Errorf("ssh: cannot load known_hosts %s: %w", knownHostsFile, err)
		}
		return verifier, nil
	}
	verifier.callback = callback
	return verifier, nil
}

// AddKnownHost pins host to an exact SHA256 fingerprint, which is accepted in
// place of a known_hosts entry for that host alone.
func (v *HostKeyVerifier) AddKnownHost(host, fingerprint string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.customKeys[strings.ToLower(hostOnly(host))] = fingerprint
}

func (v *HostKeyVerifier) VerifyHostKey(hostname string, remote net.Addr, key ssh.PublicKey) error {
	fingerprint := ssh.FingerprintSHA256(key)

	v.mu.RLock()
	pinned, hasPin := v.customKeys[strings.ToLower(hostOnly(hostname))]
	callback := v.callback
	v.mu.RUnlock()

	if hasPin {
		if fingerprint == pinned {
			return nil
		}
		return fmt.Errorf("%w: %s presented %s, pinned to %s", ErrHostKeyMismatch, hostOnly(hostname), fingerprint, pinned)
	}

	if callback == nil {
		return fmt.Errorf("ssh: no known_hosts entries are loaded from %s, so %s cannot be verified (it presented %s); add the host there, or pin it with the `host_key` argument",
			v.path, hostOnly(hostname), fingerprint)
	}
	err := callback(hostname, remote, key)
	if err == nil {
		return nil
	}

	// A host that is KNOWN and presented a DIFFERENT key is not a missing entry;
	// it is the case host key checking exists for, and no retry is the answer.
	var keyErr *knownhosts.KeyError
	if errors.As(err, &keyErr) && len(keyErr.Want) > 0 {
		return fatalf("host key changed", "ssh: %s presented %s, but known_hosts (%s) records a DIFFERENT key for it; refusing to connect — the host was rebuilt, or the connection is being intercepted",
			hostOnly(hostname), fingerprint, v.path)
	}
	var revoked *knownhosts.RevokedError
	if errors.As(err, &revoked) {
		return fatalf("host key revoked", "ssh: the key %s presented (%s) is marked @revoked in known_hosts (%s)", hostOnly(hostname), fingerprint, v.path)
	}
	return fmt.Errorf("ssh: host key verification failed for %s (it presented %s): %w; add it to known_hosts (%s) after checking that fingerprint, or pin it with the `host_key` argument",
		hostOnly(hostname), fingerprint, err, v.path)
}

func hostOnly(hostname string) string {
	if host, _, err := net.SplitHostPort(hostname); err == nil {
		return host
	}
	return hostname
}

type cachedClient struct {
	client      *ssh.Client
	fingerprint string
}

type clientCache struct {
	mu      sync.RWMutex
	clients map[string]cachedClient
}

func newClientCache() *clientCache {
	return &clientCache{clients: make(map[string]cachedClient)}
}

func (c *clientCache) get(key string) (cachedClient, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, exists := c.clients[key]
	return entry, exists
}

func (c *clientCache) put(key string, entry cachedClient) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clients[key] = entry
}

func (c *clientCache) remove(key string) {
	c.mu.Lock()
	entry, exists := c.clients[key]
	delete(c.clients, key)
	c.mu.Unlock()
	if exists {
		entry.client.Close()
	}
}

// Clear closes every pooled connection; the toolset's Close calls it.
func (c *clientCache) Clear() {
	c.mu.Lock()
	entries := make([]cachedClient, 0, len(c.clients))
	for key, entry := range c.clients {
		entries = append(entries, entry)
		delete(c.clients, key)
	}
	c.mu.Unlock()
	for _, entry := range entries {
		entry.client.Close()
	}
}

// alive releases the read lock before the network call, so a blocked keepalive
// cannot deadlock a concurrent put or remove.
func (c *clientCache) alive(key string) bool {
	c.mu.RLock()
	entry, exists := c.clients[key]
	c.mu.RUnlock()
	if !exists {
		return false
	}
	_, _, err := entry.client.SendRequest("keepalive@openssh.com", true, nil)
	return err == nil
}

type capWriter struct {
	buf       bytes.Buffer
	limit     int64
	written   int64
	truncated bool
}

func (cw *capWriter) Write(p []byte) (int, error) {
	if cw.limit > 0 {
		if cw.written >= cw.limit {
			cw.truncated = true
			return 0, io.ErrShortWrite
		}
		if writeLen := int64(len(p)); cw.written+writeLen > cw.limit {
			writeLen = cw.limit - cw.written
			cw.buf.Write(p[:writeLen])
			cw.written += writeLen
			cw.truncated = true
			return int(writeLen), io.ErrShortWrite
		}
	}
	n, err := cw.buf.Write(p)
	cw.written += int64(n)
	return n, err
}

func (h *SSHTools) executeCommand(ctx context.Context, config *SSHConfig, command string) (*SSHResult, error) {
	start := time.Now()
	result := &SSHResult{Command: command, Host: config.Host, User: config.User}

	sshConfig, fingerprint, err := h.createSSHConfig(config)
	if err != nil {
		return nil, err
	}

	var (
		client   *ssh.Client
		fromPool bool
	)
	if h.clientCache != nil {
		client, fromPool, err = h.getCachedClient(config, sshConfig, fingerprint)
	} else {
		client, err = h.createNewClient(config, sshConfig)
	}
	if err != nil {
		return nil, err
	}
	if h.clientCache == nil {
		defer client.Close()
	}
	result.HostKey = *fingerprint

	session, err := client.NewSession()
	if err != nil {
		if fromPool {
			// A pooled connection can die between the keepalive and the session.
			h.clientCache.remove(poolKey(config))
		}
		return nil, fmt.Errorf("ssh: cannot open a session on %s: %w", config.Host, err)
	}
	defer session.Close()

	limit := int64(defaultOutputByteCap)
	if val, ok := ctx.Value(taskengine.ContextKeyOutputByteLimit).(int64); ok && val > 0 {
		limit = val
	}
	stdout := &capWriter{limit: limit}
	stderr := &capWriter{limit: limit}
	session.Stdout = stdout
	session.Stderr = stderr

	cmdCtx, cancel := context.WithTimeout(ctx, config.Timeout)
	defer cancel()

	// session.Run blocks, so it races cmdCtx in a goroutine to enforce the timeout.
	cmdDone := make(chan error, 1)
	go func() {
		cmdDone <- session.Run(command)
	}()

	var (
		cmdErr    error
		timedOut  bool
		cancelled bool
	)
	select {
	case <-cmdCtx.Done():
		timedOut = errors.Is(cmdCtx.Err(), context.DeadlineExceeded)
		cancelled = !timedOut
		session.Close() // force the still-running command to stop
		<-cmdDone
	case cmdErr = <-cmdDone:
	}

	result.Duration = time.Since(start).Seconds()
	result.Stdout = strings.TrimRight(stdout.buf.String(), "\r\n")
	result.Stderr = strings.TrimRight(stderr.buf.String(), "\r\n")

	if stdout.truncated || stderr.truncated {
		// Never silent: the kept bytes are a clean head of the stream, so they
		// stay, and the notice says the rest is missing.
		result.Truncated = true
		result.Success = false
		result.ExitCode = -1
		result.Error = fmt.Sprintf("Output truncated: the command exceeded the context budget (%d bytes); only the first %d bytes of each stream are shown. Re-run with a narrower scope, or redirect the output to a file on the remote host. %s",
			limit, limit, severityRecoverable)
		return result, nil
	}

	switch {
	case cancelled:
		return nil, fmt.Errorf("ssh: the call was cancelled before %s finished", config.Host)
	case timedOut:
		result.Success = false
		result.ExitCode = -1
		result.Error = fmt.Sprintf("the command did not finish within %v and was cut off; the remote side may still be running it %s", config.Timeout, severityRecoverable)
	case cmdErr != nil:
		result.Success = false
		var exitErr *ssh.ExitError
		if errors.As(cmdErr, &exitErr) {
			// A non-zero exit is an ANSWER, not a transport failure: the model
			// gets stdout, stderr and the status rather than a bare Go error.
			result.ExitCode = exitErr.ExitStatus()
			result.Error = fmt.Sprintf("the command exited with status %d %s", result.ExitCode, severityRecoverable)
		} else {
			result.ExitCode = -1
			result.Error = cmdErr.Error() + " " + severityRecoverable
		}
	default:
		result.ExitCode = 0
		result.Success = true
	}
	return result, nil
}

// createSSHConfig returns the dial config and the address of the fingerprint the
// host key callback will record, so the result can report which key answered.
func (h *SSHTools) createSSHConfig(config *SSHConfig) (*ssh.ClientConfig, *string, error) {
	fingerprint := new(string)

	var authMethods []ssh.AuthMethod
	if config.PrivateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(config.PrivateKey))
		if err != nil {
			return nil, nil, fmt.Errorf("ssh: cannot parse the supplied private key: %w", err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}
	if config.PrivateKeyFile != "" {
		safePath, err := h.containFile("private key file", config.PrivateKeyFile)
		if err != nil {
			return nil, nil, err
		}
		signer, err := parsePrivateKeyFile(safePath)
		if err != nil {
			return nil, nil, err
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}
	if config.Password != "" {
		authMethods = append(authMethods, ssh.Password(config.Password))
	}
	if len(authMethods) == 0 {
		return nil, nil, errors.New("ssh: no authentication method was supplied; set `password`, `private_key` or `private_key_file`")
	}

	return &ssh.ClientConfig{
		User:            config.User,
		Auth:            authMethods,
		Timeout:         config.Timeout,
		HostKeyCallback: h.pinningHostKeyCallback(config, fingerprint),
	}, fingerprint, nil
}

// pinningHostKeyCallback treats a call's `host_key` as an ADDITIONAL
// requirement: a pin can only narrow what known_hosts already accepts, never
// stand in for it, so a model-supplied argument cannot loosen verification.
func (h *SSHTools) pinningHostKeyCallback(config *SSHConfig, fingerprint *string) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		*fingerprint = ssh.FingerprintSHA256(key)
		if config.HostKey != "" && *fingerprint != config.HostKey {
			return fmt.Errorf("%w: %s presented %s but the call pinned %s", ErrHostKeyMismatch, hostOnly(hostname), *fingerprint, config.HostKey)
		}
		return h.hostKeyCallback(hostname, remote, key)
	}
}

func parsePrivateKeyFile(path string) (ssh.Signer, error) {
	// A key readable by group or other is refused rather than used.
	if info, err := os.Stat(path); err == nil {
		if mode := info.Mode(); mode.Perm()&0o077 != 0 {
			return nil, fatalf("private key permissions", "ssh: private key file %s is mode %04o, readable beyond its owner; chmod 600 it", path, mode.Perm())
		}
	}

	keyData, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ssh: cannot read private key file %s: %w", path, err)
	}

	signer, err := ssh.ParsePrivateKey(keyData)
	if err != nil {
		var passErr *ssh.PassphraseMissingError
		if errors.As(err, &passErr) {
			return nil, fatalf("passphrase-protected key", "ssh: private key file %s needs a passphrase, which this tool cannot supply", path)
		}
		return nil, fmt.Errorf("ssh: cannot parse private key file %s: %w", path, err)
	}
	return signer, nil
}

func (h *SSHTools) createNewClient(config *SSHConfig, sshConfig *ssh.ClientConfig) (*ssh.Client, error) {
	address := net.JoinHostPort(config.Host, strconv.Itoa(config.Port))
	client, err := ssh.Dial("tcp", address, sshConfig)
	if err != nil {
		return nil, fmt.Errorf("ssh: cannot connect to %s as %s: %w", address, config.User, err)
	}
	return client, nil
}

// poolKey separates connections by everything that changes what a session is
// authorised to do: the identity, the credentials and any host key pin.
func poolKey(config *SSHConfig) string {
	authMaterial := config.Password + "|" + config.PrivateKey + "|" + config.PrivateKeyFile + "|" + config.HostKey
	authHash := sha256.Sum256([]byte(authMaterial))
	return fmt.Sprintf("%s@%s:%d|%x", config.User, config.Host, config.Port, authHash)
}

func (h *SSHTools) getCachedClient(config *SSHConfig, sshConfig *ssh.ClientConfig, fingerprint *string) (*ssh.Client, bool, error) {
	// The pooled entry carries the fingerprint the handshake recorded, since a
	// reused connection never runs the host key callback again.
	key := poolKey(config)
	if entry, exists := h.clientCache.get(key); exists {
		if h.clientCache.alive(key) {
			*fingerprint = entry.fingerprint
			return entry.client, true, nil
		}
		h.clientCache.remove(key)
	}

	client, err := h.createNewClient(config, sshConfig)
	if err != nil {
		return nil, false, err
	}
	h.clientCache.put(key, cachedClient{client: client, fingerprint: *fingerprint})
	return client, false, nil
}
