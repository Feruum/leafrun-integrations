package integrations

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type privateSSHServer struct {
	listener    net.Listener
	config      *ssh.ServerConfig
	repo        string
	wg          sync.WaitGroup
	mu          sync.Mutex
	connections []net.Conn
	cancel      context.CancelFunc
	ctx         context.Context
}

func newPrivateSSHServer(t *testing.T, repo string, userKey ssh.PublicKey) (*privateSSHServer, ssh.Signer) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	host, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if meta.User() == "fixture" && string(key.Marshal()) == string(userKey.Marshal()) {
			return nil, nil
		}
		return nil, errors.New("synthetic authentication failure")
	}}
	config.AddHostKey(host)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := &privateSSHServer{listener: listener, config: config, repo: repo, ctx: ctx, cancel: cancel}
	server.wg.Add(1)
	go func() {
		defer server.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			server.mu.Lock()
			server.connections = append(server.connections, conn)
			server.mu.Unlock()
			server.wg.Add(1)
			go server.serve(t, conn)
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		server.mu.Lock()
		for _, conn := range server.connections {
			_ = conn.Close()
		}
		server.mu.Unlock()
		server.wg.Wait()
	})
	return server, host
}
func (s *privateSSHServer) serve(t *testing.T, conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	_, channels, requests, err := ssh.NewServerConn(conn, s.config)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(requests)
	for incoming := range channels {
		if incoming.ChannelType() != "session" {
			_ = incoming.Reject(ssh.UnknownChannelType, "only Git sessions supported")
			continue
		}
		channel, requests, err := incoming.Accept()
		if err != nil {
			return
		}
		for request := range requests {
			if request.Type != "exec" {
				_ = request.Reply(false, nil)
				continue
			}
			var payload struct{ Command string }
			if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
				_ = request.Reply(false, nil)
				continue
			}
			if payload.Command != "git-upload-pack '/repo.git'" {
				_ = request.Reply(false, nil)
				_ = channel.Close()
				break
			}
			_ = request.Reply(true, nil)
			inputR, inputW, err := os.Pipe()
			if err != nil {
				t.Errorf("SSH stdin: %v", err)
				_ = channel.Close()
				break
			}
			copied := make(chan struct{})
			go func() { _, _ = io.Copy(inputW, channel); _ = inputW.Close(); close(copied) }()
			cmd := exec.Command("git", "upload-pack", "--strict", s.repo)
			cmd.Stdin = inputR
			cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(filepath.Dir(s.repo), "no-config"))
			err = runNative(s.ctx, cmd, func(r io.Reader) error { _, err := io.Copy(channel, r); return err })
			code := uint32(0)
			if err != nil {
				code = 1
			}
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
			_ = channel.Close()
			_ = inputR.Close()
			_ = inputW.Close()
			<-copied
			break
		}
	}
}

func sshFixture(t *testing.T) (url, config, known string, host ssh.Signer) {
	t.Helper()
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Fatal("native OpenSSH fixture is required:", err)
	}
	repo, _ := gitFixture(t)
	base := t.TempDir()
	bare := filepath.Join(base, "repo.git")
	fixtureGit(t, base, "clone", "--bare", repo, bare)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "synthetic fixture")
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(base, "synthetic-key")
	if err := os.WriteFile(key, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	restrictTestPrivateKey(t, key)
	server, host := newPrivateSSHServer(t, bare, signer.PublicKey())
	_, port, err := net.SplitHostPort(server.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	known = filepath.Join(base, "known_hosts")
	entry := knownhosts.Line([]string{server.listener.Addr().String()}, host.PublicKey()) + "\n"
	if err := os.WriteFile(known, []byte(entry), 0600); err != nil {
		t.Fatal(err)
	}
	config = filepath.Join(base, "ssh_config")
	content := fmt.Sprintf("Host fixture-git\n HostName 127.0.0.1\n Port %s\n User fixture\n IdentityFile %s\n IdentitiesOnly yes\n IdentityAgent none\n UserKnownHostsFile %s\n GlobalKnownHostsFile none\n StrictHostKeyChecking no\n BatchMode no\n PasswordAuthentication yes\n", port, sshConfigQuote(key), sshConfigQuote(known))
	if err := os.WriteFile(config, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return "ssh://fixture-git/repo.git", config, known, host
}
func sshConfigQuote(value string) string {
	return `"` + strings.ReplaceAll(filepath.ToSlash(value), `"`, `\"`) + `"`
}

func TestGitPrivateSSHNativeOpenSSHAuthAndHostVerification(t *testing.T) {
	url, config, known, _ := sshFixture(t)
	temp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	source := GitSource{URL: url, Ref: "main", Options: GitOptions{TempDir: temp, SSHConfig: config}}
	tree, err := source.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Close(); err != nil {
		t.Fatal(err)
	}
	// scp transport must use the same native SSH configuration.
	source.URL = "fixture-git:/repo.git"
	tree, err = source.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Close(); err != nil {
		t.Fatal(err)
	}
	source.URL = url
	for _, mode := range []string{"unknown", "changed", "auth"} {
		t.Run(mode, func(t *testing.T) {
			original, err := os.ReadFile(known)
			if err != nil {
				t.Fatal(err)
			}
			originalConfig, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.WriteFile(known, original, 0600); _ = os.WriteFile(config, originalConfig, 0600) }()
			switch mode {
			case "unknown":
				if err := os.WriteFile(known, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "changed":
				_, key, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				signer, err := ssh.NewSignerFromKey(key)
				if err != nil {
					t.Fatal(err)
				}
				fields := strings.Fields(string(original))
				if len(fields) < 1 {
					t.Fatal("missing known host")
				}
				if err := os.WriteFile(known, []byte(knownhosts.Line([]string{fields[0]}, signer.PublicKey())+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "auth":
				updated := string(originalConfig) + "\nHost fixture-git\n PubkeyAuthentication no\n"
				if err := os.WriteFile(config, []byte(updated), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err = source.Open(ctx)
			if err == nil {
				t.Fatal("SSH failure accepted")
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("process cause inaccessible: %v", err)
			}
			if mode != "auth" && !strings.Contains(err.Error(), "Host key verification failed") {
				t.Fatalf("host failure unclear: %v", err)
			}
			if mode == "auth" && !strings.Contains(err.Error(), "Permission denied") {
				t.Fatalf("auth failure unclear: %v", err)
			}
			data, err := os.ReadFile(known)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "unknown" && len(data) != 0 {
				t.Fatal("native client wrote unknown host")
			}
		})
	}
	items, err := os.ReadDir(temp)
	if err != nil || len(items) != 0 {
		t.Fatalf("SSH owned cleanup %v %v", items, err)
	}
}
