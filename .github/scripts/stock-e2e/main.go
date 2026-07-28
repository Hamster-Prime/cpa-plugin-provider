package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pluginID               = "multi-protocol-provider"
	credentialFile         = pluginID + ".json"
	publicModel            = "stock/test-model"
	upstreamModel          = "upstream-model"
	protocolOpenAIChat     = "openai-chat-completions"
	protocolOpenAIResponse = "openai-responses"
	maxHTTPBody            = 4 << 20
)

type options struct {
	hostPath   string
	pluginPath string
	timeout    time.Duration
}

func main() {
	hostPath := flag.String("host", "", "path to the stock CLIProxyAPI server binary")
	pluginPath := flag.String("plugin", "", "path to the compiled plugin shared library")
	timeout := flag.Duration("timeout", 90*time.Second, "overall smoke-test timeout")
	flag.Parse()
	if strings.TrimSpace(*hostPath) == "" || strings.TrimSpace(*pluginPath) == "" {
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if err := run(ctx, options{hostPath: *hostPath, pluginPath: *pluginPath, timeout: *timeout}); err != nil {
		fmt.Fprintf(os.Stderr, "stock CPA plugin smoke test failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("stock CPA plugin smoke test passed")
}

func run(ctx context.Context, opts options) error {
	if opts.timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	hostPath, err := regularFilePath(opts.hostPath)
	if err != nil {
		return fmt.Errorf("host binary: %w", err)
	}
	pluginPath, err := regularFilePath(opts.pluginPath)
	if err != nil {
		return fmt.Errorf("plugin library: %w", err)
	}

	root, err := os.MkdirTemp("", "cpa-stock-e2e-")
	if err != nil {
		return fmt.Errorf("create temporary runtime: %w", err)
	}
	defer os.RemoveAll(root)
	authDir := filepath.Join(root, "auth")
	pluginsDir := filepath.Join(root, "plugins")
	if err = os.MkdirAll(authDir, 0o700); err != nil {
		return fmt.Errorf("create auth directory: %w", err)
	}
	if err = os.MkdirAll(pluginsDir, 0o700); err != nil {
		return fmt.Errorf("create plugin directory: %w", err)
	}
	installedPlugin := filepath.Join(pluginsDir, pluginID+sharedLibraryExtension(pluginPath))
	if err = copyRegularFile(pluginPath, installedPlugin, 0o755); err != nil {
		return fmt.Errorf("install test plugin: %w", err)
	}

	upstream := newMockUpstream()
	defer upstream.Close()
	port, err := availablePort()
	if err != nil {
		return fmt.Errorf("allocate host port: %w", err)
	}
	managementKey, err := randomSecret("management")
	if err != nil {
		return err
	}
	clientKey, err := randomSecret("client")
	if err != nil {
		return err
	}
	primaryKey, err := randomSecret("upstream-primary")
	if err != nil {
		return err
	}
	backupKey, err := randomSecret("upstream-backup")
	if err != nil {
		return err
	}
	baseURL := upstream.URL + "/v1"
	configPath := filepath.Join(root, "config.yaml")
	config := stockConfigYAML(port, authDir, pluginsDir, managementKey, clientKey, baseURL)
	if err = os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		return fmt.Errorf("write stock host config: %w", err)
	}

	process, err := startHost(hostPath, root, configPath)
	if err != nil {
		return err
	}
	api := newAPIClient(fmt.Sprintf("http://127.0.0.1:%d", port), managementKey, clientKey)
	smokeErr := exercisePlugin(ctx, api, upstream, authDir, baseURL, primaryKey, backupKey)
	stopErr := process.stop(5 * time.Second)
	if smokeErr != nil {
		logs := process.logs.tail(24 << 10)
		logs = redact(logs, managementKey, clientKey, primaryKey, backupKey)
		if strings.TrimSpace(logs) != "" {
			return fmt.Errorf("%w\n\nCPA host log tail:\n%s", smokeErr, logs)
		}
		return smokeErr
	}
	if stopErr != nil {
		return fmt.Errorf("stop stock host: %w", stopErr)
	}
	return nil
}

func exercisePlugin(ctx context.Context, api *apiClient, upstream *mockUpstream, authDir, baseURL, primaryKey, backupKey string) error {
	if err := waitFor(ctx, 100*time.Millisecond, "plugin registration", func() (bool, error) {
		var response pluginListResponse
		if err := api.managementJSON(ctx, http.MethodGet, "/v0/management/plugins", nil, &response); err != nil {
			return false, err
		}
		for _, plugin := range response.Plugins {
			if plugin.ID == pluginID {
				return plugin.Registered && plugin.EffectiveEnabled, nil
			}
		}
		return false, fmt.Errorf("plugin %q is not listed", pluginID)
	}); err != nil {
		return err
	}

	resource, headers, err := api.management(ctx, http.MethodGet, "/v0/resource/plugins/"+pluginID+"/provider", nil)
	if err != nil {
		return fmt.Errorf("load provider GUI resource: %w", err)
	}
	if contentType := strings.ToLower(headers.Get("Content-Type")); !strings.Contains(contentType, "text/html") {
		return fmt.Errorf("provider GUI content type = %q, want text/html", contentType)
	}
	if !validGUIResource(resource) {
		return fmt.Errorf("provider GUI resource is incomplete (%d bytes)", len(resource))
	}

	if err = saveProviderKeys(ctx, api, protocolOpenAIChat, baseURL, primaryKey, backupKey); err != nil {
		return err
	}
	credentialPath := filepath.Join(authDir, credentialFile)
	if err = verifyCredentialFile(credentialPath, protocolOpenAIChat, baseURL, primaryKey, backupKey); err != nil {
		return err
	}

	if err = api.managementJSON(ctx, http.MethodPatch, "/v0/management/plugins/"+pluginID+"/config", map[string]any{"disabled": false}, nil); err != nil {
		return fmt.Errorf("enable configured provider: %w", err)
	}
	if err = waitForProviderState(ctx, api, protocolOpenAIChat, false); err != nil {
		return err
	}
	if err = exerciseClientMatrix(ctx, api, upstream, "OpenAI Chat upstream", "/v1/chat/completions", upstreamWireOpenAI, primaryKey, backupKey); err != nil {
		return err
	}

	if err = api.managementJSON(ctx, http.MethodPatch, "/v0/management/plugins/"+pluginID+"/config", map[string]any{"disabled": true}, nil); err != nil {
		return fmt.Errorf("disable provider before destination change: %w", err)
	}
	if err = waitForProviderState(ctx, api, protocolOpenAIChat, true); err != nil {
		return err
	}
	if err = saveProviderKeys(ctx, api, protocolOpenAIResponse, baseURL, primaryKey, backupKey); err != nil {
		return err
	}
	if err = verifyCredentialFile(credentialPath, protocolOpenAIResponse, baseURL, primaryKey, backupKey); err != nil {
		return err
	}
	if err = api.managementJSON(ctx, http.MethodPatch, "/v0/management/plugins/"+pluginID+"/config", map[string]any{
		"protocol": protocolOpenAIResponse,
		"disabled": false,
	}, nil); err != nil {
		return fmt.Errorf("switch provider to OpenAI Responses and enable it: %w", err)
	}
	if err = waitForProviderState(ctx, api, protocolOpenAIResponse, false); err != nil {
		return err
	}
	return exerciseClientMatrix(ctx, api, upstream, "OpenAI Responses upstream", "/v1/responses", upstreamWireCodex, primaryKey, backupKey)
}

func saveProviderKeys(ctx context.Context, api *apiClient, protocol, baseURL, primaryKey, backupKey string) error {
	request := map[string]any{
		"destination": map[string]any{
			"protocol": protocol,
			"base-url": baseURL,
		},
		"keys": []map[string]any{
			{"id": "primary", "label": "Primary", "api-key": primaryKey, "priority": 100},
			{"id": "backup", "label": "Backup", "api-key": backupKey, "priority": -100},
		},
	}
	var response struct {
		Status string        `json:"status"`
		Keys   []keyResponse `json:"keys"`
	}
	if err := api.managementJSON(ctx, http.MethodPut, "/v0/management/plugins/"+pluginID+"/keys", request, &response); err != nil {
		return fmt.Errorf("save two provider keys for %s: %w", protocol, err)
	}
	if response.Status != "ok" || !sameStrings(keyIDs(response.Keys), []string{"primary", "backup"}) {
		return fmt.Errorf("unexpected %s key save response: status=%q keys=%v", protocol, response.Status, keyIDs(response.Keys))
	}
	return nil
}

func waitForProviderState(ctx context.Context, api *apiClient, protocol string, disabled bool) error {
	description := fmt.Sprintf("provider reload for %s (disabled=%t)", protocol, disabled)
	return waitFor(ctx, 100*time.Millisecond, description, func() (bool, error) {
		var state struct {
			Config struct {
				Protocol string `json:"protocol"`
				Disabled bool   `json:"disabled"`
			} `json:"config"`
			Destination struct {
				Protocol string `json:"protocol"`
			} `json:"credential-destination"`
		}
		if err := api.managementJSON(ctx, http.MethodGet, "/v0/management/plugins/"+pluginID+"/state", nil, &state); err != nil {
			return false, err
		}
		if state.Config.Protocol != protocol || state.Config.Disabled != disabled || state.Destination.Protocol != protocol {
			return false, nil
		}
		if disabled {
			return true, nil
		}
		var models struct {
			Data []modelResponse `json:"data"`
		}
		if err := api.clientJSON(ctx, http.MethodGet, "/v1/models", nil, &models); err != nil {
			return false, err
		}
		return containsModel(models.Data, publicModel), nil
	})
}

type upstreamWireFormat string

const (
	upstreamWireOpenAI upstreamWireFormat = "openai-chat"
	upstreamWireCodex  upstreamWireFormat = "codex"
)

type clientEntrypoint struct {
	name    string
	format  string
	path    string
	request any
}

func exerciseClientMatrix(ctx context.Context, api *apiClient, upstream *mockUpstream, phase, upstreamPath string, wire upstreamWireFormat, primaryKey, backupKey string) error {
	entries := []clientEntrypoint{
		{
			name:   "OpenAI Chat Completions",
			format: "openai",
			path:   "/v1/chat/completions",
			request: map[string]any{
				"model":    publicModel + "(high)",
				"messages": []map[string]string{{"role": "user", "content": "stock host smoke test"}},
				"stream":   false,
			},
		},
		{
			name:   "OpenAI Responses",
			format: "openai-response",
			path:   "/v1/responses",
			request: map[string]any{
				"model":  publicModel,
				"input":  "stock host smoke test",
				"stream": false,
			},
		},
		{
			name:   "Anthropic Messages",
			format: "claude",
			path:   "/v1/messages",
			request: map[string]any{
				"model":      publicModel,
				"max_tokens": 64,
				"messages":   []map[string]string{{"role": "user", "content": "stock host smoke test"}},
				"stream":     false,
			},
		},
		{
			name:   "Gemini generateContent",
			format: "gemini",
			path:   "/v1beta/models/" + publicModel + ":generateContent",
			request: map[string]any{
				"contents": []map[string]any{{
					"role":  "user",
					"parts": []map[string]string{{"text": "stock host smoke test"}},
				}},
			},
		},
	}

	for index := range entries {
		entry := entries[index]
		body, headers, err := api.client(ctx, http.MethodPost, entry.path, entry.request)
		if err != nil {
			return fmt.Errorf("%s: route %s request: %w", phase, entry.name, err)
		}
		if contentType := strings.ToLower(headers.Get("Content-Type")); !strings.Contains(contentType, "application/json") {
			return fmt.Errorf("%s: %s response content type = %q, want application/json", phase, entry.name, contentType)
		}
		if err = validateClientResponse(entry.format, body); err != nil {
			return fmt.Errorf("%s: %s response schema: %w", phase, entry.name, err)
		}
		observation, err := awaitUpstream(ctx, upstream)
		if err != nil {
			return fmt.Errorf("%s: %s: %w", phase, entry.name, err)
		}
		if err = validateUpstreamObservation(observation, upstreamPath, wire, primaryKey, backupKey, index == 0); err != nil {
			return fmt.Errorf("%s: %s upstream request: %w", phase, entry.name, err)
		}
	}

	if err := verifyTokenCountSchemas(ctx, api); err != nil {
		return fmt.Errorf("%s: %w", phase, err)
	}
	return nil
}

func validateClientResponse(format string, body []byte) error {
	switch format {
	case "openai":
		var response struct {
			Object  string `json:"object"`
			Choices []struct {
				Message struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			return err
		}
		if response.Object != "chat.completion" || len(response.Choices) != 1 || response.Choices[0].Message.Role != "assistant" || response.Choices[0].Message.Content != "stock e2e ok" {
			return fmt.Errorf("unexpected OpenAI Chat root or choice: %s", compactBody(body, 1024))
		}
	case "openai-response":
		var response struct {
			Object string `json:"object"`
			Status string `json:"status"`
			Output []struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			return err
		}
		if response.Object != "response" || response.Status != "completed" || len(response.Output) != 1 || response.Output[0].Type != "message" || response.Output[0].Role != "assistant" || len(response.Output[0].Content) != 1 || response.Output[0].Content[0].Type != "output_text" || response.Output[0].Content[0].Text != "stock e2e ok" {
			return fmt.Errorf("unexpected OpenAI Responses root or output: %s", compactBody(body, 1024))
		}
	case "claude":
		var response struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			return err
		}
		if response.Type != "message" || response.Role != "assistant" || len(response.Content) != 1 || response.Content[0].Type != "text" || response.Content[0].Text != "stock e2e ok" {
			return fmt.Errorf("unexpected Anthropic Messages root or content: %s", compactBody(body, 1024))
		}
	case "gemini":
		var response struct {
			Candidates []struct {
				Content struct {
					Role  string `json:"role"`
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			return err
		}
		if len(response.Candidates) != 1 || response.Candidates[0].Content.Role != "model" || len(response.Candidates[0].Content.Parts) != 1 || response.Candidates[0].Content.Parts[0].Text != "stock e2e ok" {
			return fmt.Errorf("unexpected Gemini root or candidate: %s", compactBody(body, 1024))
		}
	default:
		return fmt.Errorf("unsupported client response format %q", format)
	}
	return nil
}

func validateUpstreamObservation(observation upstreamObservation, expectedPath string, wire upstreamWireFormat, primaryKey, backupKey string, requireThinking bool) error {
	if observation.Path != expectedPath {
		return fmt.Errorf("path = %q, want %q", observation.Path, expectedPath)
	}
	if observation.Authorization != "Bearer "+primaryKey {
		return fmt.Errorf("selected the wrong key: authorization=%q", redact(observation.Authorization, primaryKey, backupKey))
	}
	if observation.Model != upstreamModel {
		return fmt.Errorf("model = %q, want %q", observation.Model, upstreamModel)
	}
	if observation.Stream {
		return fmt.Errorf("stream = true for a non-stream request")
	}
	switch wire {
	case upstreamWireOpenAI:
		if !observation.MessagesArray || observation.InputArray || observation.ContentsArray {
			return fmt.Errorf("root schema is not OpenAI Chat (messages=%t input=%t contents=%t)", observation.MessagesArray, observation.InputArray, observation.ContentsArray)
		}
	case upstreamWireCodex:
		if !observation.InputArray || observation.MessagesArray || observation.ContentsArray {
			return fmt.Errorf("root schema is not Codex (messages=%t input=%t contents=%t)", observation.MessagesArray, observation.InputArray, observation.ContentsArray)
		}
	default:
		return fmt.Errorf("unknown expected upstream wire format %q", wire)
	}
	if requireThinking && observation.ReasoningEffort != "high" {
		return fmt.Errorf("thinking suffix was not applied: effort=%q", observation.ReasoningEffort)
	}
	return nil
}

func verifyTokenCountSchemas(ctx context.Context, api *apiClient) error {
	var claude map[string]json.RawMessage
	if err := api.clientJSON(ctx, http.MethodPost, "/v1/messages/count_tokens", map[string]any{
		"model":    publicModel,
		"messages": []map[string]string{{"role": "user", "content": "count these tokens"}},
	}, &claude); err != nil {
		return fmt.Errorf("route Anthropic count_tokens request: %w", err)
	}
	if err := validateExactTokenCountRoot(claude, "input_tokens"); err != nil {
		return fmt.Errorf("Anthropic count_tokens schema: %w", err)
	}

	var gemini map[string]json.RawMessage
	if err := api.clientJSON(ctx, http.MethodPost, "/v1beta/models/"+publicModel+":countTokens", map[string]any{
		"contents": []map[string]any{{
			"role":  "user",
			"parts": []map[string]string{{"text": "count these tokens"}},
		}},
	}, &gemini); err != nil {
		return fmt.Errorf("route Gemini countTokens request: %w", err)
	}
	if err := validateExactTokenCountRoot(gemini, "totalTokens"); err != nil {
		return fmt.Errorf("Gemini countTokens schema: %w", err)
	}
	return nil
}

func validateExactTokenCountRoot(response map[string]json.RawMessage, key string) error {
	if len(response) != 1 {
		return fmt.Errorf("root keys = %v, want only %q", sortedJSONKeys(response), key)
	}
	raw, ok := response[key]
	if !ok {
		return fmt.Errorf("root keys = %v, want only %q", sortedJSONKeys(response), key)
	}
	var count int64
	if err := json.Unmarshal(raw, &count); err != nil || count <= 0 {
		return fmt.Errorf("%s = %s, want a positive integer", key, compactBody(raw, 128))
	}
	return nil
}

func sortedJSONKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

type pluginListResponse struct {
	Plugins []struct {
		ID               string `json:"id"`
		Registered       bool   `json:"registered"`
		EffectiveEnabled bool   `json:"effective_enabled"`
	} `json:"plugins"`
}

type keyResponse struct {
	ID string `json:"id"`
}

type modelResponse struct {
	ID string `json:"id"`
}

func keyIDs(keys []keyResponse) []string {
	out := make([]string, len(keys))
	for i := range keys {
		out[i] = keys[i].ID
	}
	return out
}

func containsModel(models []modelResponse, wanted string) bool {
	for _, model := range models {
		if model.ID == wanted {
			return true
		}
	}
	return false
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func validGUIResource(body []byte) bool {
	lower := bytes.ToLower(body)
	return len(body) >= 1024 &&
		bytes.Contains(lower, []byte("<!doctype html")) &&
		bytes.Contains(lower, []byte("management-key")) &&
		bytes.Contains(lower, []byte("provider-form"))
}

type credentialOnDisk struct {
	Type        string `json:"type"`
	Destination struct {
		Protocol string `json:"protocol"`
		BaseURL  string `json:"base-url"`
	} `json:"destination"`
	Keys []struct {
		ID     string `json:"id"`
		APIKey string `json:"api-key"`
	} `json:"keys"`
}

func verifyCredentialFile(path, protocol, baseURL, primaryKey, backupKey string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat saved credential file: %w", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("credential file mode = %o, want no group/other permissions", info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read saved credential file: %w", err)
	}
	var credential credentialOnDisk
	if err = json.Unmarshal(raw, &credential); err != nil {
		return fmt.Errorf("decode saved credential file: %w", err)
	}
	if credential.Type != pluginID || credential.Destination.Protocol != protocol || credential.Destination.BaseURL != baseURL {
		return fmt.Errorf("saved credential destination does not match the provider")
	}
	if len(credential.Keys) != 2 || credential.Keys[0].ID != "primary" || credential.Keys[0].APIKey != primaryKey || credential.Keys[1].ID != "backup" || credential.Keys[1].APIKey != backupKey {
		return fmt.Errorf("saved credential pool does not contain both submitted keys")
	}
	return nil
}

type upstreamObservation struct {
	Authorization   string
	Path            string
	Model           string
	ReasoningEffort string
	Stream          bool
	MessagesArray   bool
	InputArray      bool
	ContentsArray   bool
}

type mockUpstream struct {
	*httptest.Server
	observations chan upstreamObservation
}

func newMockUpstream() *mockUpstream {
	observations := make(chan upstreamObservation, 32)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || (r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/v1/responses") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxHTTPBody+1))
		if err != nil || len(raw) > maxHTTPBody {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		var payload struct {
			Model           string `json:"model"`
			ReasoningEffort string `json:"reasoning_effort"`
			Reasoning       struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
			Stream   bool            `json:"stream"`
			Messages json.RawMessage `json:"messages"`
			Input    json.RawMessage `json:"input"`
			Contents json.RawMessage `json:"contents"`
		}
		if err = json.Unmarshal(raw, &payload); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		reasoningEffort := payload.ReasoningEffort
		if reasoningEffort == "" {
			reasoningEffort = payload.Reasoning.Effort
		}
		observation := upstreamObservation{
			Authorization:   r.Header.Get("Authorization"),
			Path:            r.URL.Path,
			Model:           payload.Model,
			ReasoningEffort: reasoningEffort,
			Stream:          payload.Stream,
			MessagesArray:   nonEmptyJSONArray(payload.Messages),
			InputArray:      nonEmptyJSONArray(payload.Input),
			ContentsArray:   nonEmptyJSONArray(payload.Contents),
		}
		select {
		case observations <- observation:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/responses" {
			_, _ = io.WriteString(w, `{"id":"resp-stock-e2e","object":"response","created_at":1,"status":"completed","model":"upstream-model","output":[{"id":"msg-stock-e2e","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"stock e2e ok","annotations":[]}]}],"parallel_tool_calls":true,"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl-stock-e2e","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"stock e2e ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	})
	server := httptest.NewServer(handler)
	return &mockUpstream{Server: server, observations: observations}
}

func nonEmptyJSONArray(raw json.RawMessage) bool {
	var values []json.RawMessage
	return len(raw) != 0 && json.Unmarshal(raw, &values) == nil && len(values) != 0
}

func awaitUpstream(ctx context.Context, server *mockUpstream) (upstreamObservation, error) {
	select {
	case observation := <-server.observations:
		return observation, nil
	case <-ctx.Done():
		return upstreamObservation{}, fmt.Errorf("wait for mock upstream request: %w", ctx.Err())
	}
}

func stockConfigYAML(port int, authDir, pluginsDir, managementKey, clientKey, baseURL string) string {
	quote := strconv.Quote
	return fmt.Sprintf(`host: "127.0.0.1"
port: %d
remote-management:
  allow-remote: false
  secret-key: %s
  disable-control-panel: true
auth-dir: %s
api-keys:
  - %s
debug: true
plugins:
  enabled: true
  dir: %s
  configs:
    %s:
      enabled: true
      name: "Stock CPA E2E"
      protocol: "openai-chat-completions"
      base-url: %s
      prefix: "stock"
      models:
        - name: %s
          alias: "test-model"
          display-name: "Stock E2E Model"
          input-modalities:
            - "text"
            - "image"
          output-modalities:
            - "text"
          thinking:
            min: 0
            max: 8192
            zero-allowed: true
            dynamic-allowed: true
            levels:
              - "low"
              - "medium"
              - "high"
      disabled: true
`, port, quote(managementKey), quote(authDir), quote(clientKey), quote(pluginsDir), pluginID, quote(baseURL), quote(upstreamModel))
}

func waitFor(ctx context.Context, interval time.Duration, description string, check func() (bool, error)) error {
	var lastErr error
	for {
		ok, err := check()
		if ok {
			return nil
		}
		if err != nil {
			lastErr = err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if lastErr != nil {
				return fmt.Errorf("wait for %s: %w (last error: %v)", description, ctx.Err(), lastErr)
			}
			return fmt.Errorf("wait for %s: %w", description, ctx.Err())
		case <-timer.C:
		}
	}
}

type apiClient struct {
	baseURL       string
	managementKey string
	clientKey     string
	http          *http.Client
}

func newAPIClient(baseURL, managementKey, clientKey string) *apiClient {
	return &apiClient{
		baseURL:       strings.TrimRight(baseURL, "/"),
		managementKey: managementKey,
		clientKey:     clientKey,
		http: &http.Client{
			Timeout: 20 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("unexpected redirect")
			},
		},
	}
}

func (c *apiClient) managementJSON(ctx context.Context, method, path string, input, output any) error {
	body, _, err := c.do(ctx, method, path, input, c.managementKey, true)
	if err != nil || output == nil {
		return err
	}
	if err = json.Unmarshal(body, output); err != nil {
		return fmt.Errorf("decode %s %s response: %w", method, path, err)
	}
	return nil
}

func (c *apiClient) clientJSON(ctx context.Context, method, path string, input, output any) error {
	body, _, err := c.client(ctx, method, path, input)
	if err != nil || output == nil {
		return err
	}
	if err = json.Unmarshal(body, output); err != nil {
		return fmt.Errorf("decode %s %s response: %w", method, path, err)
	}
	return nil
}

func (c *apiClient) client(ctx context.Context, method, path string, input any) ([]byte, http.Header, error) {
	return c.do(ctx, method, path, input, c.clientKey, false)
}

func (c *apiClient) management(ctx context.Context, method, path string, input any) ([]byte, http.Header, error) {
	return c.do(ctx, method, path, input, c.managementKey, true)
}

func (c *apiClient) do(ctx context.Context, method, path string, input any, key string, management bool) ([]byte, http.Header, error) {
	var requestBody io.Reader
	if input != nil {
		body, err := json.Marshal(input)
		if err != nil {
			return nil, nil, fmt.Errorf("encode %s %s request: %w", method, path, err)
		}
		requestBody = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, requestBody)
	if err != nil {
		return nil, nil, fmt.Errorf("create %s %s request: %w", method, path, err)
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if management {
		request.Header.Set("X-Management-Key", key)
	} else {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxHTTPBody+1))
	if err != nil {
		return nil, response.Header.Clone(), fmt.Errorf("read %s %s response: %w", method, path, err)
	}
	if len(body) > maxHTTPBody {
		return nil, response.Header.Clone(), fmt.Errorf("%s %s response exceeds %d bytes", method, path, maxHTTPBody)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, response.Header.Clone(), fmt.Errorf("%s %s returned HTTP %d: %s", method, path, response.StatusCode, compactBody(body, 2048))
	}
	return body, response.Header.Clone(), nil
}

type synchronizedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *synchronizedBuffer) Write(payload []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(payload)
}

func (b *synchronizedBuffer) tail(limit int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	payload := b.buf.Bytes()
	if len(payload) > limit {
		payload = payload[len(payload)-limit:]
	}
	return string(append([]byte(nil), payload...))
}

type hostProcess struct {
	cmd      *exec.Cmd
	logs     *synchronizedBuffer
	done     chan struct{}
	waitMu   sync.Mutex
	waitErr  error
	stopOnce sync.Once
	stopErr  error
}

func startHost(hostPath, workingDir, configPath string) (*hostProcess, error) {
	logs := &synchronizedBuffer{}
	cmd := exec.Command(hostPath, "-config", configPath, "-local-model")
	cmd.Dir = workingDir
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start stock CPA host: %w", err)
	}
	process := &hostProcess{cmd: cmd, logs: logs, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		process.waitMu.Lock()
		process.waitErr = err
		process.waitMu.Unlock()
		close(process.done)
	}()
	return process, nil
}

func (p *hostProcess) stop(timeout time.Duration) error {
	p.stopOnce.Do(func() {
		select {
		case <-p.done:
			p.stopErr = p.exitError("host exited before shutdown")
			return
		default:
		}
		if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
			if killErr := p.cmd.Process.Kill(); killErr != nil {
				p.stopErr = fmt.Errorf("signal host: %v; kill host: %w", err, killErr)
				return
			}
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-p.done:
			return
		case <-timer.C:
			if err := p.cmd.Process.Kill(); err != nil {
				p.stopErr = fmt.Errorf("kill host after shutdown timeout: %w", err)
				return
			}
			<-p.done
		}
	})
	return p.stopErr
}

func (p *hostProcess) exitError(prefix string) error {
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	if p.waitErr == nil {
		return fmt.Errorf("%s with status 0", prefix)
	}
	return fmt.Errorf("%s: %w", prefix, p.waitErr)
}

func availablePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func regularFilePath(path string) (string, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a regular file", absolute)
	}
	return absolute, nil
}

func sharedLibraryExtension(path string) string {
	extension := strings.ToLower(filepath.Ext(path))
	switch extension {
	case ".so", ".dylib", ".dll":
		return extension
	default:
		if runtime.GOOS == "windows" {
			return ".dll"
		}
		if runtime.GOOS == "darwin" {
			return ".dylib"
		}
		return ".so"
	}
}

func copyRegularFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	return closeErr
}

func randomSecret(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate %s test credential: %w", prefix, err)
	}
	return prefix + "-" + hex.EncodeToString(raw[:]), nil
}

func compactBody(body []byte, limit int) string {
	text := strings.TrimSpace(string(body))
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > limit {
		return text[:limit] + "..."
	}
	return text
}

func redact(text string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text
}
