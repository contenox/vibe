package sshtool_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/sshtool"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const (
	testUser     = "tester"
	testPassword = "hunter2"
)

// remote is an in-process SSH server: the revival is only believable against a
// real handshake, a real host key and a real exec channel.
type remote struct {
	listener net.Listener
	signer   ssh.Signer
	host     string
	port     int
	done     chan struct{}

	mu  sync.Mutex
	ran []string
}

func newRemote(t *testing.T) *remote {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	host, portStr, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	s := &remote{listener: listener, signer: signer, host: host, port: port, done: make(chan struct{})}

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == testUser && string(pass) == testPassword {
				return nil, nil
			}
			return nil, errors.New("authentication failed")
		},
	}
	cfg.AddHostKey(signer)

	go s.accept(cfg)
	t.Cleanup(func() {
		close(s.done)
		listener.Close()
	})
	return s
}

func (s *remote) addr() string { return net.JoinHostPort(s.host, strconv.Itoa(s.port)) }

// knownHosts writes a known_hosts file naming this server, in the [host]:port
// form OpenSSH uses for a non-default port.
func (s *remote) knownHosts(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	line := knownhosts.Line([]string{s.addr()}, s.signer.PublicKey())
	require.NoError(t, os.WriteFile(path, []byte(line+"\n"), 0o600))
	return path
}

func (s *remote) fingerprint() string { return ssh.FingerprintSHA256(s.signer.PublicKey()) }

// commands returns every command the server was actually asked to run, which is
// how a test proves a refused call never reached the machine.
func (s *remote) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ran...)
}

func (s *remote) accept(cfg *ssh.ServerConfig) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.serveConn(conn, cfg)
	}
}

func (s *remote) serveConn(nc net.Conn, cfg *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		nc.Close()
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "only session channels are served")
			continue
		}
		ch, chReqs, err := newChan.Accept()
		if err != nil {
			return
		}
		go s.serveSession(ch, chReqs)
	}
}

func (s *remote) serveSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
		var msg struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &msg); err != nil {
			req.Reply(false, nil)
			return
		}
		req.Reply(true, nil)

		s.mu.Lock()
		s.ran = append(s.ran, msg.Command)
		s.mu.Unlock()

		status := s.run(ch, msg.Command)
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
		return
	}
}

// run is the scripted remote: each command name is one behaviour a test needs.
func (s *remote) run(ch ssh.Channel, command string) int {
	switch {
	case command == "greet":
		fmt.Fprintln(ch, "hello from the remote")
		return 0
	case command == "fail":
		fmt.Fprintln(ch.Stderr(), "boom")
		return 3
	case command == "flood":
		fmt.Fprint(ch, strings.Repeat("x", 64*1024))
		return 0
	case command == "hang":
		select {
		case <-s.done:
		case <-time.After(30 * time.Second):
		}
		return 0
	default:
		fmt.Fprintf(ch.Stderr(), "no such scripted command: %s\n", command)
		return 127
	}
}

// callArgs is one execute_remote_command call against this server, authenticated.
func (s *remote) callArgs(command string) map[string]any {
	return map[string]any{
		"host":     s.host,
		"port":     s.port,
		"user":     testUser,
		"password": testPassword,
		"command":  command,
	}
}

// newTools builds the toolset against an explicit known_hosts file, so a test
// never depends on the developer's ~/.ssh.
func newTools(t *testing.T, knownHostsFile string, opts ...sshtool.SSHOption) taskengine.ToolsRepo {
	t.Helper()
	if knownHostsFile == "" {
		knownHostsFile = filepath.Join(t.TempDir(), "known_hosts")
		require.NoError(t, os.WriteFile(knownHostsFile, nil, 0o600))
	}
	all := append([]sshtool.SSHOption{sshtool.WithKnownHostsFile(knownHostsFile)}, opts...)
	repo, err := sshtool.NewSSHTools(all...)
	require.NoError(t, err)
	return repo
}

func call(name string) *taskengine.ToolsCall {
	return &taskengine.ToolsCall{Name: name, ToolName: sshtool.ToolExecuteRemoteCommand}
}

// hashHost renders the |1| form OpenSSH writes when HashKnownHosts is on.
func hashHost(t *testing.T, address string) string {
	t.Helper()
	return knownhosts.HashHostname(address)
}

// knownhostsLine records other's host key under address, so a test can present
// a known host that answers with a key nobody recorded for it.
func knownhostsLine(t *testing.T, address string, other *remote) string {
	t.Helper()
	return knownhosts.Line([]string{address}, other.signer.PublicKey())
}
