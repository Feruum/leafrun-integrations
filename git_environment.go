package integrations

import (
	"regexp"
	"strings"
)

// Native credentials remain opaque to this library. Preserve native helper,
// agent, home and configuration inputs, but remove repository overrides,
// injected command configuration, prompts, tracing and debugger pauses.
func gitEnvironment(inherited []string) []string {
	env := make([]string, 0, len(inherited)+20)
	for _, item := range inherited {
		key, _, _ := strings.Cut(item, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "GIT_TRACE") || strings.HasPrefix(upper, "GCM_TRACE") || strings.HasPrefix(upper, "GCM_DEBUG") || strings.HasPrefix(upper, "GIT_CONFIG_KEY_") || strings.HasPrefix(upper, "GIT_CONFIG_VALUE_") {
			continue
		}
		switch upper {
		case "GIT_DIR", "GIT_COMMON_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS", "GIT_NAMESPACE", "GIT_REPLACE_REF_BASE", "GIT_SHALLOW_FILE", "GIT_EXEC_PATH", "GIT_TEMPLATE_DIR", "GIT_SSH", "GIT_SSH_COMMAND", "GIT_SSH_VARIANT", "GIT_ASKPASS", "SSH_ASKPASS", "SSH_ASKPASS_REQUIRE", "GIT_TERMINAL_PROMPT", "GCM_INTERACTIVE", "GCM_GUI_PROMPT", "GCM_MODAL_PROMPT", "GIT_ALLOW_PROTOCOL", "GIT_PROTOCOL", "GIT_PROTOCOL_FROM_USER", "GIT_SSL_NO_VERIFY", "GIT_CURL_VERBOSE", "CURL_VERBOSE", "GIT_NO_REPLACE_OBJECTS":
			continue
		}
		env = append(env, item)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=", "SSH_ASKPASS_REQUIRE=never", "GCM_INTERACTIVE=0", "GCM_GUI_PROMPT=0", "GCM_MODAL_PROMPT=0", "GCM_TRACE=0", "GCM_TRACE_SECRETS=0", "GCM_TRACE_MSAUTH=0", "GCM_DEBUG=0", "GIT_TRACE=0", "GIT_TRACE_CURL=0", "GIT_TRACE_CURL_NO_DATA=1", "GIT_TRACE2=0", "GIT_TRACE2_EVENT=0", "GIT_TRACE2_PERF=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_ALLOW_PROTOCOL=https:ssh:file", "GIT_PROTOCOL_FROM_USER=0")
}

var transportURLSecret = regexp.MustCompile(`(?i)(https?://|ssh://)[^\s/@]+@`)
var transportClippedURLSecret = regexp.MustCompile(`(?im)(https?://|ssh://)[^\s/@]*:[^\s/@]*$`)
var transportAuthSecret = regexp.MustCompile(`(?im)((?:proxy-)?authorization\s*:\s*)[^\r\n]+`)
var transportNamedSecret = regexp.MustCompile(`(?i)((?:password|passwd|access[_-]?token|refresh[_-]?token|token|secret|oauth[_-]?token)\s*[=:]\s*)[^\s&;]+`)

func redactTransport(text string) string {
	text = transportURLSecret.ReplaceAllString(text, "${1}[redacted]@")
	text = transportClippedURLSecret.ReplaceAllString(text, "${1}[redacted]")
	text = transportAuthSecret.ReplaceAllString(text, "${1}[redacted]")
	return transportNamedSecret.ReplaceAllString(text, "${1}[redacted]")
}

// Git invokes OpenSSH using its native transport mechanism. This quoting is for
// Git's sshCommand configuration, never for a library-created shell process.
func quoteSSHArgument(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
