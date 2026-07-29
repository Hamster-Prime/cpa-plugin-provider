package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStockConfigYAMLContainsIsolatedPluginConfiguration(t *testing.T) {
	config := stockConfigYAML(
		18080,
		`/tmp/auth path`,
		`/tmp/plugins path`,
		`management-"secret"`,
		`client-secret`,
		`http://127.0.0.1:18081/v1`,
	)
	for _, wanted := range []string{
		`port: 18080`,
		`auth-dir: "/tmp/auth path"`,
		`dir: "/tmp/plugins path"`,
		`secret-key: "management-\"secret\""`,
		`multi-protocol-provider:`,
		`protocol: "openai-chat-completions"`,
		`disabled: true`,
		`name: "upstream-model"`,
		`alias: "test-model"`,
	} {
		if !strings.Contains(config, wanted) {
			t.Fatalf("config does not contain %q:\n%s", wanted, config)
		}
	}
}

func TestValidGUIResourceRequiresConfigurationShell(t *testing.T) {
	valid := []byte("<!doctype html><form id=\"provider-form\"><input id=\"management-key\"></form>" +
		"cpa-management-auth cpa-multi-protocol-provider-ready" + strings.Repeat("x", 1024))
	if !validGUIResource(valid) {
		t.Fatal("complete GUI resource was rejected")
	}
	if validGUIResource([]byte("<!doctype html><form id=\"provider-form\"></form>" + strings.Repeat("x", 1024))) {
		t.Fatal("GUI without management key was accepted")
	}
}

func TestWaitForIncludesLastTransientError(t *testing.T) {
	ctx, cancel := contextWithTimeout(t, 20*time.Millisecond)
	defer cancel()
	err := waitFor(ctx, time.Millisecond, "example", func() (bool, error) {
		return false, errSentinel
	})
	if err == nil || !strings.Contains(err.Error(), "last error: transient") {
		t.Fatalf("waitFor() error = %v", err)
	}
}

func TestRedactReplacesEveryCredentialOccurrence(t *testing.T) {
	got := redact("alpha secret beta secret", "secret")
	if got != "alpha [redacted] beta [redacted]" {
		t.Fatalf("redact() = %q", got)
	}
}

func TestValidateClientResponseAcceptsEachProtocolRoot(t *testing.T) {
	tests := []struct {
		format string
		body   string
	}{
		{"openai", `{"object":"chat.completion","choices":[{"message":{"role":"assistant","content":"stock e2e ok"}}]}`},
		{"openai-response", `{"object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"stock e2e ok"}]}]}`},
		{"claude", `{"type":"message","role":"assistant","content":[{"type":"text","text":"stock e2e ok"}]}`},
		{"gemini", `{"candidates":[{"content":{"role":"model","parts":[{"text":"stock e2e ok"}]}}]}`},
	}
	for _, test := range tests {
		t.Run(test.format, func(t *testing.T) {
			if err := validateClientResponse(test.format, []byte(test.body)); err != nil {
				t.Fatalf("validateClientResponse() error = %v", err)
			}
		})
	}
	if err := validateClientResponse("claude", []byte(tests[0].body)); err == nil {
		t.Fatal("OpenAI response was accepted as an Anthropic Messages response")
	}
}

func TestValidateExactTokenCountRootRejectsWrappersAndExtraFields(t *testing.T) {
	if err := validateExactTokenCountRoot(map[string]json.RawMessage{"input_tokens": json.RawMessage(`12`)}, "input_tokens"); err != nil {
		t.Fatalf("valid Anthropic token count rejected: %v", err)
	}
	if err := validateExactTokenCountRoot(map[string]json.RawMessage{"totalTokens": json.RawMessage(`9`)}, "totalTokens"); err != nil {
		t.Fatalf("valid Gemini token count rejected: %v", err)
	}
	invalid := []map[string]json.RawMessage{
		{"usage": json.RawMessage(`{"input_tokens":12}`)},
		{"input_tokens": json.RawMessage(`12`), "type": json.RawMessage(`"count"`)},
		{"input_tokens": json.RawMessage(`0`)},
	}
	for index, response := range invalid {
		if err := validateExactTokenCountRoot(response, "input_tokens"); err == nil {
			t.Fatalf("invalid token count response %d was accepted: %#v", index, response)
		}
	}
}
