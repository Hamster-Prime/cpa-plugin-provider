package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

type abiStatusError struct {
	status        int
	requestScoped bool
}

func (e abiStatusError) Error() string         { return http.StatusText(e.status) }
func (e abiStatusError) StatusCode() int       { return e.status }
func (e abiStatusError) IsRequestScoped() bool { return e.requestScoped }

func TestWaitForHostHTTPStreamReadClosesBlockedStreamOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	readReleased := make(chan struct{})
	closed := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := waitForHostHTTPStreamRead(ctx, func() (abiHostHTTPStreamReadResponse, error) {
			<-readReleased
			return abiHostHTTPStreamReadResponse{}, nil
		}, func() {
			close(closed)
			close(readReleased)
		})
		result <- err
	}()
	cancel()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("blocked host stream was not closed")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waitForHostHTTPStreamRead() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled host stream read did not return")
	}
}

func TestHandleRegisterDeclaresDynamicCapabilities(t *testing.T) {
	rawConfig := []byte("protocol: openai-responses\nbase-url: https://api.example.com/v1\nmodels:\n  - name: gpt-test\n")
	request, err := json.Marshal(abiLifecycleRequest{ConfigYAML: rawConfig, SchemaVersion: pluginabi.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleRegister(request)
	if err != nil {
		t.Fatalf("handleRegister() error = %v", err)
	}
	defer MultiProtocolProviderPluginShutdown()

	var envelope pluginabi.Envelope
	if err = json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !envelope.OK {
		t.Fatalf("registration failed: %#v", envelope.Error)
	}
	var registration abiRegistration
	if err = json.Unmarshal(envelope.Result, &registration); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if registration.SchemaVersion != pluginabi.SchemaVersion || registration.Metadata.Version != pluginVersion {
		t.Fatalf("registration metadata = %#v", registration)
	}
	if !registration.Capabilities.AuthProvider || !registration.Capabilities.ModelProvider || !registration.Capabilities.Executor ||
		!registration.Capabilities.ThinkingApplier || !registration.Capabilities.ManagementAPI {
		t.Fatalf("missing capability: %#v", registration.Capabilities)
	}
	wantInputFormats := []string{"codex", "openai-image"}
	wantOutputFormats := []string{"openai", "openai-response", "claude", "gemini", "openai-image"}
	if !equalStringSlices(registration.Capabilities.ExecutorInputFormats, wantInputFormats) ||
		!equalStringSlices(registration.Capabilities.ExecutorOutputFormats, wantOutputFormats) {
		t.Fatalf("formats = %#v / %#v", registration.Capabilities.ExecutorInputFormats, registration.Capabilities.ExecutorOutputFormats)
	}
}

func TestManagementRegistrationDoesNotSerializeHandlers(t *testing.T) {
	if _, err := handleRegister([]byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	defer MultiProtocolProviderPluginShutdown()
	raw, err := handleABIMethod(t.Context(), pluginabi.MethodManagementRegister, []byte(`{}`))
	if err != nil {
		t.Fatalf("management.register error = %v", err)
	}
	if string(raw) == "" || containsBytes(raw, []byte("authStore")) {
		t.Fatalf("management registration leaked handler state: %s", raw)
	}
}

func TestUnknownMethodReturnsErrorEnvelope(t *testing.T) {
	if _, err := handleRegister([]byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	defer MultiProtocolProviderPluginShutdown()
	raw, err := handleABIMethod(t.Context(), "unknown.method", nil)
	if err != nil {
		t.Fatalf("unknown method returned Go error: %v", err)
	}
	var envelope pluginabi.Envelope
	if err = json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "unknown_method" {
		t.Fatalf("envelope = %#v", envelope)
	}
}

func TestABIErrorPreservesRequestScopeWithoutMisclassifyingAuthFailures(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode string
	}{
		{name: "invalid request", err: abiStatusError{status: http.StatusBadRequest, requestScoped: true}, wantCode: "request_scoped"},
		{name: "unauthorized", err: abiStatusError{status: http.StatusUnauthorized}, wantCode: "plugin_error"},
		{name: "rate limited", err: abiStatusError{status: http.StatusTooManyRequests}, wantCode: "plugin_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var envelope pluginabi.Envelope
			if err := json.Unmarshal(abiErrorFromError("plugin_error", tt.err), &envelope); err != nil {
				t.Fatalf("decode envelope: %v", err)
			}
			if envelope.Error == nil || envelope.Error.Code != tt.wantCode {
				t.Fatalf("envelope error = %#v, want code %q", envelope.Error, tt.wantCode)
			}
		})
	}
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func containsBytes(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for index := 0; index+len(needle) <= len(haystack); index++ {
		match := true
		for offset := range needle {
			if haystack[index+offset] != needle[offset] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
