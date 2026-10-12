package integrations

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestGitRejectsOptionsAndMissingRefs(t *testing.T) {
	repo, _ := gitFixture(t)
	for _, ref := range []string{"", "-main", "HEAD", "main~1", "main^{commit}", "main:main.tex", "@{0}", "bad\nref", "deadbee"} {
		_, err := (GitSource{URL: repo, Ref: ref}).Open(context.Background())
		if !errors.Is(err, ErrOptions) && ref != "deadbee" {
			t.Fatalf("%q: %v", ref, err)
		}
		if err == nil {
			t.Fatalf("accepted %q", ref)
		}
	}
	fixtureGit(t, repo, "tag", "main")
	if _, err := (GitSource{URL: repo, Ref: "main"}).Open(context.Background()); !errors.Is(err, ErrOptions) {
		t.Fatalf("ambiguous: %v", err)
	}
	for _, ref := range []string{"absent", "0123456789012345678901234567890123456789"} {
		_, err := (GitSource{URL: repo, Ref: ref}).Open(context.Background())
		if err == nil {
			t.Fatal("missing accepted")
		}
	}
	for _, url := range []string{"https://user:synthetic-token@example.invalid/repo", "http://example.invalid/repo", "git://example.invalid/repo", "ext::command", "-remote"} {
		_, err := (GitSource{URL: url, Ref: "main"}).Open(context.Background())
		if !errors.Is(err, ErrOptions) {
			t.Fatalf("URL %q: %v", url, err)
		}
	}
	if _, err := (GitSource{URL: repo, Ref: "refs/heads/main", SourceRoot: "missing"}).Open(context.Background()); !errors.Is(err, ErrSourceRoot) {
		t.Fatalf("root: %v", err)
	}
}
func TestGitNoninteractiveEnvironmentAndRedaction(t *testing.T) {
	env := gitEnvironment([]string{"HOME=/synthetic/home", "SSH_AUTH_SOCK=/synthetic/agent", "GIT_TRACE=1", "GIT_TRACE_CURL=/secret", "GCM_TRACE_SECRETS=true", "GCM_DEBUG=1", "GCM_INTERACTIVE=1", "GIT_ASKPASS=gui", "GIT_DIR=wrong", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=unsafe"})
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, want := range []string{"HOME=/synthetic/home", "SSH_AUTH_SOCK=/synthetic/agent", "GCM_INTERACTIVE=0", "GIT_TERMINAL_PROMPT=0", "GCM_TRACE_SECRETS=0"} {
		if !strings.Contains(joined, "\n"+want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	for _, bad := range []string{"/secret", "=gui", "=wrong", "=unsafe", "GIT_CONFIG_COUNT"} {
		if strings.Contains(joined, bad) {
			t.Errorf("inherited %s", bad)
		}
	}
	input := "fatal https://user:synthetic-secret@example.invalid/repo Authorization: Basic c3ludGhldGlj\npassword=synthetic-password\naccess_token: synthetic-token\n"
	output := redactTransport(input)
	for _, secret := range []string{"synthetic-secret", "c3ludGhldGlj", "synthetic-password", "synthetic-token"} {
		if strings.Contains(output, secret) {
			t.Fatalf("leaked %s: %s", secret, output)
		}
	}
}

func TestGitEnvironmentDoesNotEnableCurlVerbose(t *testing.T) {
	for _, item := range gitEnvironment([]string{"GIT_CURL_VERBOSE=1", "CURL_VERBOSE=1"}) {
		if strings.HasPrefix(item, "GIT_CURL_VERBOSE=") || strings.HasPrefix(item, "CURL_VERBOSE=") {
			t.Fatalf("curl tracing is enabled by variable presence: %s", item)
		}
	}
}

func TestGitRedactsTruncatedTransportDiagnostics(t *testing.T) {
	for _, data := range []string{"fatal https://user:synthetic-secret", "Authorization: Bearer synthetic-secret", "password=synthetic-secret"} {
		if output := redactTransport(data); strings.Contains(output, "synthetic-secret") {
			t.Fatalf("truncated diagnostic leaked: %s", output)
		}
	}
}
