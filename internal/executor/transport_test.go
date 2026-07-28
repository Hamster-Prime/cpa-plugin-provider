package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	providerauth "github.com/Hamster-Prime/cpa-plugin-provider/internal/auth"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestSafeTransportAllowsSameOriginRedirect(t *testing.T) {
	var finalAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/chat/completions" {
			http.Redirect(response, request, "/final", http.StatusTemporaryRedirect)
			return
		}
		finalAuthorization = request.Header.Get("Authorization")
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"model":"upstream-model","choices":[]}`))
	}))
	defer server.Close()

	cfg := testConfig(server.URL+"/v1", config.ProtocolOpenAIChat, false)
	_, err := New(cfg).Execute(context.Background(), pluginapi.ExecutorRequest{
		Model:       "public-model",
		Payload:     []byte(`{"messages":[]}`),
		StorageJSON: credentialJSON(cfg, "same-origin-secret"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if finalAuthorization != "Bearer same-origin-secret" {
		t.Fatalf("redirected Authorization = %q", finalAuthorization)
	}
}

func TestSafeTransportStopsCrossOriginRedirectBeforeCredentialLeak(t *testing.T) {
	var destinationRequests atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		destinationRequests.Add(1)
		if request.Header.Get("X-Api-Key") != "" {
			t.Errorf("cross-origin request leaked X-Api-Key")
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()

	source := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, destination.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	cfg := testConfig(source.URL+"/v1", config.ProtocolAnthropic, false)
	_, err := New(cfg).Execute(context.Background(), pluginapi.ExecutorRequest{
		Model:       "public-model",
		Payload:     []byte(`{"messages":[]}`),
		StorageJSON: credentialJSON(cfg, "cross-origin-secret"),
	})
	if err == nil {
		t.Fatal("Execute() followed or accepted a cross-origin redirect")
	}
	if destinationRequests.Load() != 0 {
		t.Fatalf("cross-origin destination requests = %d, want 0", destinationRequests.Load())
	}
}

func TestPerKeyProxyOverridesHostProxyFallback(t *testing.T) {
	var keyProxyRequests atomic.Int32
	keyProxy := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		keyProxyRequests.Add(1)
		if request.URL.Host != "upstream.example" || request.Header.Get("Authorization") != "Bearer proxy-secret" {
			t.Errorf("proxied request URL/header = %s / %#v", request.URL, request.Header)
		}
		_, _ = io.WriteString(response, `{"model":"upstream-model","choices":[]}`)
	}))
	defer keyProxy.Close()

	var hostProxyRequests atomic.Int32
	hostProxy := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		hostProxyRequests.Add(1)
		response.WriteHeader(http.StatusBadGateway)
	}))
	defer hostProxy.Close()

	cfg := testConfig("http://upstream.example/v1", config.ProtocolOpenAIChat, false)
	storage := credentialJSONWithProxy(cfg, "proxy-secret", keyProxy.URL, hostProxy.URL)
	_, err := New(cfg).Execute(context.Background(), pluginapi.ExecutorRequest{
		Model:       "public-model",
		Payload:     []byte(`{"messages":[]}`),
		StorageJSON: storage,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if keyProxyRequests.Load() != 1 || hostProxyRequests.Load() != 0 {
		t.Fatalf("proxy requests = key %d, host %d", keyProxyRequests.Load(), hostProxyRequests.Load())
	}
}

func TestHostProxyFallbackIsUsedWithoutPerKeyProxy(t *testing.T) {
	requests := make(chan struct{}, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests <- struct{}{}
		_, _ = io.WriteString(response, `{"model":"upstream-model","choices":[]}`)
	}))
	defer proxy.Close()

	cfg := testConfig("http://upstream.example/v1", config.ProtocolOpenAIChat, false)
	_, err := New(cfg).Execute(context.Background(), pluginapi.ExecutorRequest{
		Model:       "public-model",
		Payload:     []byte(`{"messages":[]}`),
		StorageJSON: credentialJSONWithProxy(cfg, "secret", "", proxy.URL),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("host proxy fallback was not used")
	}
}

func TestSafeTransportRejectsOversizedNonStreamingResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte("123456789"))
	}))
	defer server.Close()

	client, err := NewHTTPClientWithResponseLimit("direct", 8)
	if err != nil {
		t.Fatalf("NewHTTPClientWithResponseLimit() error = %v", err)
	}
	_, err = client.Do(context.Background(), pluginapi.HTTPRequest{Method: http.MethodGet, URL: server.URL})
	if !errors.Is(err, ErrResponseBodyTooLarge) {
		t.Fatalf("Do() error = %v, want ErrResponseBodyTooLarge", err)
	}
}

func credentialJSONWithProxy(cfg config.Config, key, proxyURL, hostProxyURL string) []byte {
	raw, err := json.Marshal(map[string]any{
		"type":           "multi-protocol-provider",
		"destination":    providerauth.DestinationForConfig(cfg),
		"api-key":        key,
		"proxy-url":      proxyURL,
		"host-proxy-url": hostProxyURL,
	})
	if err != nil {
		panic(err)
	}
	return raw
}
