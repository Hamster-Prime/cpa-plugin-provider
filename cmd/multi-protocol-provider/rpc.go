package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Hamster-Prime/cpa-plugin-provider/internal/management"
	pluginpkg "github.com/Hamster-Prime/cpa-plugin-provider/internal/plugin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type abiLifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type abiRegistration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  abiCapabilities    `json:"capabilities"`
}

type abiCapabilities struct {
	AuthProvider          bool                         `json:"auth_provider"`
	ModelProvider         bool                         `json:"model_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	ThinkingApplier       bool                         `json:"thinking_applier"`
	ManagementAPI         bool                         `json:"management_api"`
}

type abiIdentifierResponse struct {
	Identifier string `json:"identifier"`
}

type abiAuthLoginStartRequest struct {
	pluginapi.AuthLoginStartRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type abiAuthLoginPollRequest struct {
	pluginapi.AuthLoginPollRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type abiAuthRefreshRequest struct {
	pluginapi.AuthRefreshRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type abiAuthModelRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type abiExecutorRequest struct {
	pluginapi.ExecutorRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
	StreamID       string `json:"stream_id,omitempty"`
}

type abiExecutorHTTPRequest struct {
	pluginapi.ExecutorHTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type abiThinkingApplyRequest struct {
	pluginapi.ThinkingApplyRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type abiManagementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type abiManagementRegistrationResponse struct {
	Routes    []pluginapi.ManagementRoute `json:"routes,omitempty"`
	Resources []pluginapi.ResourceRoute   `json:"resources,omitempty"`
}

type abiExecutorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

type abiHostHTTPRequest struct {
	pluginapi.HTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type abiHostHTTPStreamResponse struct {
	StatusCode int                         `json:"status_code"`
	Headers    http.Header                 `json:"headers,omitempty"`
	StreamID   string                      `json:"stream_id,omitempty"`
	Chunks     []pluginapi.HTTPStreamChunk `json:"chunks,omitempty"`
}

type abiHostHTTPStreamReadRequest struct {
	StreamID string `json:"stream_id"`
}

type abiHostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type abiHostHTTPStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
}

type abiHostStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
}

type abiHostStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

type abiHostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type abiHostAuthGetRequest struct {
	pluginapi.HostAuthGetRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type abiHostAuthSaveRequest struct {
	pluginapi.HostAuthSaveRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type abiEmptyResponse struct{}

func handleABIMethod(ctx context.Context, method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return handleRegister(request)
	case pluginabi.MethodPluginShutdown:
		MultiProtocolProviderPluginShutdown()
		return abiOKEnvelope(abiEmptyResponse{})
	}
	p, err := currentPlugin()
	if err != nil {
		return nil, err
	}

	switch method {
	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier, pluginabi.MethodThinkingIdentifier:
		return abiOKEnvelope(abiIdentifierResponse{Identifier: p.Identifier()})
	case pluginabi.MethodAuthParse:
		var req pluginapi.AuthParseRequest
		if err = decodeRequest(request, &req); err != nil {
			return nil, err
		}
		resp, callErr := p.ParseAuth(ctx, req)
		return abiResult(resp, callErr)
	case pluginabi.MethodAuthLoginStart:
		var rpcReq abiAuthLoginStartRequest
		if err = decodeRequest(request, &rpcReq); err != nil {
			return nil, err
		}
		req := rpcReq.AuthLoginStartRequest
		req.HTTPClient = abiHostHTTPClient{callbackID: rpcReq.HostCallbackID}
		resp, callErr := p.StartLogin(ctx, req)
		return abiResult(resp, callErr)
	case pluginabi.MethodAuthLoginPoll:
		var rpcReq abiAuthLoginPollRequest
		if err = decodeRequest(request, &rpcReq); err != nil {
			return nil, err
		}
		req := rpcReq.AuthLoginPollRequest
		req.HTTPClient = abiHostHTTPClient{callbackID: rpcReq.HostCallbackID}
		resp, callErr := p.PollLogin(ctx, req)
		return abiResult(resp, callErr)
	case pluginabi.MethodAuthRefresh:
		var rpcReq abiAuthRefreshRequest
		if err = decodeRequest(request, &rpcReq); err != nil {
			return nil, err
		}
		req := rpcReq.AuthRefreshRequest
		req.HTTPClient = abiHostHTTPClient{callbackID: rpcReq.HostCallbackID}
		resp, callErr := p.RefreshAuth(ctx, req)
		return abiResult(resp, callErr)
	case pluginabi.MethodModelStatic:
		var req pluginapi.StaticModelRequest
		if err = decodeRequest(request, &req); err != nil {
			return nil, err
		}
		resp, callErr := p.StaticModels(ctx, req)
		return abiResult(resp, callErr)
	case pluginabi.MethodModelForAuth:
		var rpcReq abiAuthModelRequest
		if err = decodeRequest(request, &rpcReq); err != nil {
			return nil, err
		}
		req := rpcReq.AuthModelRequest
		req.HTTPClient = abiHostHTTPClient{callbackID: rpcReq.HostCallbackID}
		resp, callErr := p.ModelsForAuth(ctx, req)
		return abiResult(resp, callErr)
	case pluginabi.MethodExecutorExecute:
		var rpcReq abiExecutorRequest
		if err = decodeRequest(request, &rpcReq); err != nil {
			return nil, err
		}
		req := rpcReq.ExecutorRequest
		req.HTTPClient = abiHostHTTPClient{callbackID: rpcReq.HostCallbackID}
		resp, callErr := p.Execute(ctx, req)
		return abiResult(resp, callErr)
	case pluginabi.MethodExecutorExecuteStream:
		var rpcReq abiExecutorRequest
		if err = decodeRequest(request, &rpcReq); err != nil {
			return nil, err
		}
		req := rpcReq.ExecutorRequest
		req.HTTPClient = abiHostHTTPClient{callbackID: rpcReq.HostCallbackID}
		resp, callErr := p.ExecuteStream(ctx, req)
		if callErr != nil {
			return abiResult(abiExecutorStreamResponse{}, callErr)
		}
		marshaled, callErr := marshalABIStreamResponse(ctx, rpcReq.StreamID, resp)
		return abiResult(marshaled, callErr)
	case pluginabi.MethodExecutorCountTokens:
		var rpcReq abiExecutorRequest
		if err = decodeRequest(request, &rpcReq); err != nil {
			return nil, err
		}
		req := rpcReq.ExecutorRequest
		req.HTTPClient = abiHostHTTPClient{callbackID: rpcReq.HostCallbackID}
		resp, callErr := p.CountTokens(ctx, req)
		return abiResult(resp, callErr)
	case pluginabi.MethodExecutorHTTPRequest:
		var rpcReq abiExecutorHTTPRequest
		if err = decodeRequest(request, &rpcReq); err != nil {
			return nil, err
		}
		req := rpcReq.ExecutorHTTPRequest
		req.HTTPClient = abiHostHTTPClient{callbackID: rpcReq.HostCallbackID}
		resp, callErr := p.HttpRequest(ctx, req)
		return abiResult(resp, callErr)
	case pluginabi.MethodThinkingApply:
		var rpcReq abiThinkingApplyRequest
		if err = decodeRequest(request, &rpcReq); err != nil {
			return nil, err
		}
		resp, callErr := p.ApplyThinking(ctx, rpcReq.ThinkingApplyRequest)
		return abiResult(resp, callErr)
	case pluginabi.MethodManagementRegister:
		var req pluginapi.ManagementRegistrationRequest
		if err = decodeRequest(request, &req); err != nil {
			return nil, err
		}
		resp, callErr := p.RegisterManagement(ctx, req)
		return abiResult(sanitizeManagementRegistration(resp), callErr)
	case pluginabi.MethodManagementHandle:
		var rpcReq abiManagementRequest
		if err = decodeRequest(request, &rpcReq); err != nil {
			return nil, err
		}
		requestCtx := management.WithHTTPClient(ctx, abiHostHTTPClient{callbackID: rpcReq.HostCallbackID})
		resp, callErr := p.HandleManagement(requestCtx, rpcReq.ManagementRequest)
		return abiResult(resp, callErr)
	default:
		return abiErrorEnvelope("unknown_method", "unknown method: "+method, 0), nil
	}
}

func handleRegister(request []byte) ([]byte, error) {
	var req abiLifecycleRequest
	if err := decodeRequest(request, &req); err != nil {
		return nil, err
	}
	registered, instance, err := pluginpkg.Build(req.ConfigYAML, pluginpkg.Dependencies{AuthStore: abiHostAuthStore{}})
	if err != nil {
		return nil, err
	}
	registered.Metadata.Version = pluginVersion
	abiState.Lock()
	abiState.plugin = instance
	abiState.Unlock()
	caps := registered.Capabilities
	return abiOKEnvelope(abiRegistration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata:      registered.Metadata,
		Capabilities: abiCapabilities{
			AuthProvider:          caps.AuthProvider != nil,
			ModelProvider:         caps.ModelProvider != nil,
			Executor:              caps.Executor != nil,
			ExecutorModelScope:    caps.ExecutorModelScope,
			ExecutorInputFormats:  append([]string(nil), caps.ExecutorInputFormats...),
			ExecutorOutputFormats: append([]string(nil), caps.ExecutorOutputFormats...),
			ThinkingApplier:       caps.ThinkingApplier != nil,
			ManagementAPI:         caps.ManagementAPI != nil,
		},
	})
}

func decodeRequest(raw []byte, target any) error {
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode ABI request: %w", err)
	}
	return nil
}

func sanitizeManagementRegistration(resp pluginapi.ManagementRegistrationResponse) abiManagementRegistrationResponse {
	routes := make([]pluginapi.ManagementRoute, len(resp.Routes))
	copy(routes, resp.Routes)
	for index := range routes {
		routes[index].Handler = nil
	}
	resources := make([]pluginapi.ResourceRoute, len(resp.Resources))
	copy(resources, resp.Resources)
	for index := range resources {
		resources[index].Handler = nil
	}
	return abiManagementRegistrationResponse{Routes: routes, Resources: resources}
}

type abiHostHTTPClient struct {
	callbackID string
}

func (c abiHostHTTPClient) Do(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	return callHost[pluginapi.HTTPResponse](pluginabi.MethodHostHTTPDo, abiHostHTTPRequest{
		HTTPRequest: req, HostCallbackID: c.callbackID,
	})
}

func (c abiHostHTTPClient) DoStream(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	resp, err := callHost[abiHostHTTPStreamResponse](pluginabi.MethodHostHTTPDoStream, abiHostHTTPRequest{
		HTTPRequest: req, HostCallbackID: c.callbackID,
	})
	if err != nil {
		return pluginapi.HTTPStreamResponse{}, err
	}
	if resp.StreamID != "" {
		chunks := make(chan pluginapi.HTTPStreamChunk)
		go readHostHTTPStream(ctx, resp.StreamID, chunks)
		return pluginapi.HTTPStreamResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Chunks: chunks}, nil
	}
	chunks := make(chan pluginapi.HTTPStreamChunk, len(resp.Chunks))
	for _, chunk := range resp.Chunks {
		chunks <- chunk
	}
	close(chunks)
	return pluginapi.HTTPStreamResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Chunks: chunks}, nil
}

func readHostHTTPStream(ctx context.Context, streamID string, output chan<- pluginapi.HTTPStreamChunk) {
	defer close(output)
	defer closeHostHTTPStream(streamID)
	emit := func(chunk pluginapi.HTTPStreamChunk) bool {
		select {
		case output <- chunk:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		resp, err := waitForHostHTTPStreamRead(ctx, func() (abiHostHTTPStreamReadResponse, error) {
			return callHost[abiHostHTTPStreamReadResponse](pluginabi.MethodHostHTTPStreamRead, abiHostHTTPStreamReadRequest{StreamID: streamID})
		}, func() {
			closeHostHTTPStream(streamID)
		})
		if err != nil {
			emit(pluginapi.HTTPStreamChunk{Err: err})
			return
		}
		if resp.Error != "" {
			emit(pluginapi.HTTPStreamChunk{Err: fmt.Errorf("%s", resp.Error)})
			return
		}
		if len(resp.Payload) > 0 {
			if !emit(pluginapi.HTTPStreamChunk{Payload: append([]byte(nil), resp.Payload...)}) {
				return
			}
		}
		if resp.Done {
			return
		}
	}
}

type hostHTTPStreamReadResult struct {
	response abiHostHTTPStreamReadResponse
	err      error
}

func waitForHostHTTPStreamRead(ctx context.Context, read func() (abiHostHTTPStreamReadResponse, error), closeStream func()) (abiHostHTTPStreamReadResponse, error) {
	result := make(chan hostHTTPStreamReadResult, 1)
	go func() {
		response, err := read()
		result <- hostHTTPStreamReadResult{response: response, err: err}
	}()
	select {
	case completed := <-result:
		return completed.response, completed.err
	case <-ctx.Done():
		if closeStream != nil {
			closeStream()
		}
		return abiHostHTTPStreamReadResponse{}, ctx.Err()
	}
}

func closeHostHTTPStream(streamID string) {
	_, _ = callHost[abiEmptyResponse](pluginabi.MethodHostHTTPStreamClose, abiHostHTTPStreamCloseRequest{StreamID: streamID})
}

func marshalABIStreamResponse(ctx context.Context, streamID string, resp pluginapi.ExecutorStreamResponse) (abiExecutorStreamResponse, error) {
	if streamID == "" {
		chunks := make([]pluginapi.ExecutorStreamChunk, 0)
		for chunk := range resp.Chunks {
			chunks = append(chunks, chunk)
		}
		return abiExecutorStreamResponse{Headers: resp.Headers, Chunks: chunks}, nil
	}
	go pumpABIStream(ctx, streamID, resp.Chunks)
	return abiExecutorStreamResponse{Headers: resp.Headers}, nil
}

func pumpABIStream(ctx context.Context, streamID string, chunks <-chan pluginapi.ExecutorStreamChunk) {
	errorMessage := ""
	defer func() {
		_, _ = callHost[abiEmptyResponse](pluginabi.MethodHostStreamClose, abiHostStreamCloseRequest{StreamID: streamID, Error: errorMessage})
	}()
	for {
		select {
		case <-ctx.Done():
			errorMessage = ctx.Err().Error()
			return
		case chunk, ok := <-chunks:
			if !ok {
				return
			}
			if chunk.Err != nil {
				errorMessage = chunk.Err.Error()
				return
			}
			if len(chunk.Payload) == 0 {
				continue
			}
			_, err := callHost[abiEmptyResponse](pluginabi.MethodHostStreamEmit, abiHostStreamEmitRequest{StreamID: streamID, Payload: chunk.Payload})
			if err != nil {
				errorMessage = err.Error()
				return
			}
		}
	}
}

type abiHostAuthStore struct{}

func (abiHostAuthStore) ListAuth(context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	resp, err := callHost[abiHostAuthListResponse](pluginabi.MethodHostAuthList, struct{}{})
	return resp.Files, err
}

func (abiHostAuthStore) GetAuth(_ context.Context, req pluginapi.HostAuthGetRequest) (pluginapi.HostAuthGetResponse, error) {
	return callHost[pluginapi.HostAuthGetResponse](pluginabi.MethodHostAuthGet, abiHostAuthGetRequest{HostAuthGetRequest: req})
}

func (abiHostAuthStore) SaveAuth(_ context.Context, req pluginapi.HostAuthSaveRequest) (pluginapi.HostAuthSaveResponse, error) {
	return callHost[pluginapi.HostAuthSaveResponse](pluginabi.MethodHostAuthSave, abiHostAuthSaveRequest{HostAuthSaveRequest: req})
}

var _ pluginapi.HostHTTPClient = abiHostHTTPClient{}
var _ management.HostAuthStore = abiHostAuthStore{}
