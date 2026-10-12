package integrations

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This subprocess is a configured native helper, with entirely synthetic data.
// Authentication is deliberately performed by Git, never by the adapter.
func TestGitCredentialHelper(t *testing.T) {
	if os.Getenv("LEAFRUN_TEST_CREDENTIAL_HELPER") == "" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	if os.Args[len(os.Args)-1] != "get" {
		os.Exit(0)
	}
	for _, name := range []string{"GIT_TERMINAL_PROMPT", "GCM_INTERACTIVE", "GCM_TRACE", "GCM_TRACE_SECRETS", "GCM_DEBUG"} {
		if os.Getenv(name) != "0" {
			fmt.Fprintf(os.Stderr, "unsafe helper env %s=%s", name, os.Getenv(name))
			os.Exit(9)
		}
	}
	if os.Getenv("LEAFRUN_TEST_CREDENTIAL_FAIL") != "" {
		fmt.Fprintln(os.Stderr, "Authorization: Basic synthetic-encoded-secret\npassword=synthetic-password token=synthetic-token https://user:synthetic-url-secret@127.0.0.1/")
		os.Exit(7)
	}
	fmt.Fprint(os.Stdout, "username=fixture\npassword=synthetic-password\n\n")
	os.Exit(0)
}

func privateHTTPFixture(t *testing.T) (string, *atomic.Int64) {
	return privateHTTPFixtureGate(t, nil)
}

func privateHTTPFixtureGate(t *testing.T, gate func(*http.Request)) (string, *atomic.Int64) {
	t.Helper()
	repo, _ := gitFixture(t)
	base := t.TempDir()
	bare := filepath.Join(base, "repo.git")
	fixtureGit(t, base, "clone", "--bare", repo, bare)
	var accepted atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		user, password, ok := request.BasicAuth()
		if !ok || user != "fixture" || password != "synthetic-password" {
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		accepted.Add(1)
		if gate != nil {
			gate(request)
			if request.Context().Err() != nil {
				return
			}
		}
		cmd := exec.Command("git", "http-backend")
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(base, "no-config"), "GIT_PROJECT_ROOT="+base, "GIT_HTTP_EXPORT_ALL=1", "REQUEST_METHOD="+request.Method, "PATH_INFO="+request.URL.Path, "QUERY_STRING="+request.URL.RawQuery, "CONTENT_TYPE="+request.Header.Get("Content-Type"), "CONTENT_LENGTH="+strconv.FormatInt(request.ContentLength, 10), "SERVER_PROTOCOL=HTTP/1.1", "REMOTE_USER=fixture")
		cmd.Stdin = request.Body
		var output bytes.Buffer
		err := runNative(request.Context(), cmd, func(r io.Reader) error { _, err := copyBounded(request.Context(), &output, r, 16<<20); return err })
		if err != nil {
			t.Errorf("native HTTP backend: %v", err)
			http.Error(w, "backend failed", 500)
			return
		}
		headers, body, ok := bytes.Cut(output.Bytes(), []byte("\r\n\r\n"))
		if !ok {
			t.Errorf("backend CGI headers: %q", output.Bytes())
			http.Error(w, "bad headers", 500)
			return
		}
		status := 200
		for _, line := range strings.Split(string(headers), "\r\n") {
			key, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			if key == "Status" {
				fields := strings.Fields(value)
				if len(fields) > 0 {
					status, _ = strconv.Atoi(fields[0])
				}
			} else {
				w.Header().Add(key, strings.TrimSpace(value))
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if _, err := x509.ParseCertificate(server.Certificate().Raw); err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(base, "trusted-test-ca.pem")
	if err := os.WriteFile(ca, certificate, 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(base, "native-git-config")
	helper := "!" + quoteSSHArgument(filepath.ToSlash(os.Args[0])) + " -test.run=^TestGitCredentialHelper$ --"
	fixtureGit(t, base, "config", "--file", config, "credential.helper", helper)
	fixtureGit(t, base, "config", "--file", config, "http.sslCAInfo", filepath.ToSlash(ca))
	fixtureGit(t, base, "config", "--file", config, "credential.interactive", "true")
	fixtureGit(t, base, "config", "--file", config, "credential.trace", "true")
	fixtureGit(t, base, "config", "--file", config, "credential.traceSecrets", "true")
	fixtureGit(t, base, "config", "--file", config, "credential.debug", "true")
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("LEAFRUN_TEST_CREDENTIAL_HELPER", "1")
	return server.URL + "/repo.git", &accepted
}

func TestGitCancellationDuringNativeHTTPSFetch(t *testing.T) {
	temp := t.TempDir()
	started := make(chan struct{})
	var once sync.Once
	url, _ := privateHTTPFixtureGate(t, func(request *http.Request) {
		items, _ := os.ReadDir(temp)
		for _, item := range items {
			if _, err := os.Stat(filepath.Join(temp, item.Name(), "objects.git")); err == nil {
				once.Do(func() { close(started) })
				<-request.Context().Done()
				return
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := (GitSource{URL: url, Ref: "main", Options: GitOptions{TempDir: temp}}).Open(ctx)
		finished <- err
	}()
	select {
	case <-started:
		cancel()
	case <-ctx.Done():
		t.Fatal("native fetch did not reach cancellable transport")
	}
	err := <-finished
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("native fetch cancellation: %v", err)
	}
	items, err := os.ReadDir(temp)
	if err != nil || len(items) != 0 {
		t.Fatalf("cancelled transport owned cleanup: %v %v", items, err)
	}
}

func TestGitPrivateHTTPSNativeCredentialsTLSAndRedaction(t *testing.T) {
	url, accepted := privateHTTPFixture(t)
	temp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tree, err := (GitSource{URL: url, Ref: "main", Options: GitOptions{TempDir: temp}}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Load() < 2 {
		t.Fatal("native helper not used for resolve and acquisition")
	}
	if err := tree.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEAFRUN_TEST_CREDENTIAL_FAIL", "1")
	_, err = (GitSource{URL: url, Ref: "main", Options: GitOptions{TempDir: temp}}).Open(ctx)
	if err == nil {
		t.Fatal("failed helper accepted")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("native process cause inaccessible: %v", err)
	}
	for _, text := range []string{err.Error(), string(exitErr.Stderr)} {
		for _, secret := range []string{"synthetic-encoded-secret", "synthetic-password", "synthetic-token", "synthetic-url-secret"} {
			if strings.Contains(text, secret) {
				t.Fatalf("leaked credential %s: %s", secret, text)
			}
		}
	}
	entries, err := os.ReadDir(temp)
	if err != nil || len(entries) != 0 {
		t.Fatalf("transport cleanup: %v %v", entries, err)
	}
}

func TestGitUntrustedTLSCannotBeDisabledByScopedNativeConfig(t *testing.T) {
	url, _ := privateHTTPFixture(t)
	config := os.Getenv("GIT_CONFIG_GLOBAL")
	fixtureGit(t, filepath.Dir(config), "config", "--file", config, "--unset", "http.sslCAInfo")
	fixtureGit(t, filepath.Dir(config), "config", "--file", config, "http."+url+".sslVerify", "false")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tree, err := (GitSource{URL: url, Ref: "main"}).Open(ctx)
	if err == nil {
		defer tree.Close()
		t.Fatal("scoped native configuration disabled TLS verification")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "ssl") && !strings.Contains(strings.ToLower(err.Error()), "certificate") && !strings.Contains(strings.ToLower(err.Error()), "untrusted") {
		t.Fatalf("TLS failure unclear: %v", err)
	}
}
