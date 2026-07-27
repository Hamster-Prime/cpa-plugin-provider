package executor

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestStatusErrorRequestScopeClassification(t *testing.T) {
	tests := []struct {
		name string
		err  statusError
		want bool
	}{
		{
			name: "local request validation",
			err:  newRequestScopedStatusError(http.StatusBadRequest, []byte("request body must be valid JSON")),
			want: true,
		},
		{
			name: "openai invalid request",
			err:  newUpstreamStatusError(http.StatusBadRequest, []byte(`{"error":{"type":"invalid_request_error","code":"invalid_value","message":"Invalid input"}}`)),
			want: true,
		},
		{
			name: "anthropic bad request",
			err:  newUpstreamStatusError(http.StatusBadRequest, []byte(`{"type":"error","error":{"type":"bad_request_error","message":"Bad input"}}`)),
			want: true,
		},
		{
			name: "gemini invalid argument",
			err:  newUpstreamStatusError(http.StatusBadRequest, []byte(`{"error":{"status":"INVALID_ARGUMENT","message":"Bad input"}}`)),
			want: true,
		},
		{
			name: "gemini invalid api key overrides invalid argument",
			err:  newUpstreamStatusError(http.StatusBadRequest, []byte(`{"error":{"status":"INVALID_ARGUMENT","details":[{"reason":"API_KEY_INVALID"}]}}`)),
		},
		{
			name: "model unsupported overrides invalid request",
			err:  newUpstreamStatusError(http.StatusBadRequest, []byte(`{"error":{"type":"invalid_request_error","code":"model_not_supported","message":"requested model is not supported"}}`)),
		},
		{
			name: "unknown plain bad request",
			err:  newUpstreamStatusError(http.StatusBadRequest, []byte("bad request")),
		},
		{
			name: "unauthorized",
			err:  newUpstreamStatusError(http.StatusUnauthorized, []byte(`{"error":{"type":"authentication_error"}}`)),
		},
		{
			name: "forbidden",
			err:  newUpstreamStatusError(http.StatusForbidden, []byte(`{"error":{"status":"PERMISSION_DENIED"}}`)),
		},
		{
			name: "rate limited",
			err:  newUpstreamStatusError(http.StatusTooManyRequests, []byte(`{"error":{"type":"rate_limit_error"}}`)),
		},
		{
			name: "unprocessable delegated to host classification",
			err:  newUpstreamStatusError(http.StatusUnprocessableEntity, []byte(`{"error":{"type":"invalid_request_error"}}`)),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.IsRequestScoped(); got != tt.want {
				t.Fatalf("IsRequestScoped() = %v, want %v; error=%v", got, tt.want, tt.err)
			}
		})
	}
}

func TestUpstreamStatusErrorRedactsSecretsWithoutChangingClassification(t *testing.T) {
	const (
		apiKey       = `api-key-"private"`
		customHeader = "custom-header-private"
	)
	body := []byte(`{"error":{"type":"invalid_request_error","message":"credentials api-key-\"private\" and custom-header-private were rejected"}}`)
	err := newUpstreamStatusError(http.StatusBadRequest, body, apiKey, customHeader)

	if err.StatusCode() != http.StatusBadRequest {
		t.Fatalf("StatusCode() = %d, want %d", err.StatusCode(), http.StatusBadRequest)
	}
	if !err.IsRequestScoped() {
		t.Fatal("secret redaction changed request-scope classification")
	}
	message := err.Error()
	if strings.Contains(message, apiKey) || strings.Contains(message, `api-key-\"private\"`) || strings.Contains(message, customHeader) {
		t.Fatalf("Error() leaked a sensitive value: %q", message)
	}
	if strings.Count(message, redactedSensitiveValue) != 2 {
		t.Fatalf("Error() = %q, want two redaction markers", message)
	}
}

func TestUpstreamStatusErrorHidesBodiesAtSizeLimit(t *testing.T) {
	t.Run("secret crosses truncation boundary", func(t *testing.T) {
		const secret = "cross-boundary-private"
		body := bytes.Repeat([]byte("x"), maxErrorBodyBytes-len(secret)/2)
		body = append(body, secret...)

		err := newUpstreamStatusError(http.StatusBadGateway, body, secret)
		want := "upstream request failed with status 502"
		if got := err.Error(); got != want {
			t.Fatalf("Error() = %q, want %q", got, want)
		}
	})

	t.Run("exact limit still uses body for classification", func(t *testing.T) {
		body := []byte(`{"error":{"type":"invalid_request_error","message":"bad input"}}`)
		body = append(body, bytes.Repeat([]byte(" "), maxErrorBodyBytes-len(body))...)

		err := newUpstreamStatusError(http.StatusBadRequest, body)
		if !err.IsRequestScoped() {
			t.Fatal("at-limit body was not used for request-scope classification")
		}
		want := "upstream request failed with status 400"
		if got := err.Error(); got != want {
			t.Fatalf("Error() = %q, want %q", got, want)
		}
	})
}
