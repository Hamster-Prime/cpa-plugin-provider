package main

/*
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int MultiProtocolProviderPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void MultiProtocolProviderPluginFree(void*, size_t);
extern void MultiProtocolProviderPluginShutdown(void);

static int multi_protocol_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return api->call(api->host_ctx, method, request, request_len, response);
}

static void multi_protocol_free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
	api->free_buffer(ptr, len);
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"unsafe"

	pluginpkg "github.com/Hamster-Prime/cpa-plugin-provider/internal/plugin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

var abiState = struct {
	sync.RWMutex
	host   *C.cliproxy_host_api
	plugin *pluginpkg.ProviderPlugin
}{}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if host == nil || api == nil || host.call == nil || host.free_buffer == nil {
		return 1
	}
	abiState.Lock()
	abiState.host = host
	abiState.Unlock()
	api.abi_version = C.uint32_t(pluginabi.ABIVersion)
	api.call = C.cliproxy_plugin_call_fn(C.MultiProtocolProviderPluginCall)
	api.free_buffer = C.cliproxy_plugin_free_fn(C.MultiProtocolProviderPluginFree)
	api.shutdown = C.cliproxy_plugin_shutdown_fn(C.MultiProtocolProviderPluginShutdown)
	return 0
}

//export MultiProtocolProviderPluginCall
func MultiProtocolProviderPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeABIResponse(response, abiErrorEnvelope("invalid_method", "method is required", 0))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := handleABIMethod(context.Background(), C.GoString(method), requestBytes)
	if err != nil {
		writeABIResponse(response, abiErrorFromError("plugin_error", err))
		return 1
	}
	writeABIResponse(response, raw)
	return 0
}

//export MultiProtocolProviderPluginFree
func MultiProtocolProviderPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export MultiProtocolProviderPluginShutdown
func MultiProtocolProviderPluginShutdown() {
	abiState.Lock()
	abiState.plugin = nil
	abiState.host = nil
	abiState.Unlock()
}

func currentPlugin() (*pluginpkg.ProviderPlugin, error) {
	abiState.RLock()
	defer abiState.RUnlock()
	if abiState.plugin == nil {
		return nil, fmt.Errorf("plugin is not registered")
	}
	return abiState.plugin, nil
}

func callHost[T any](method string, request any) (T, error) {
	var zero T
	abiState.RLock()
	host := abiState.host
	abiState.RUnlock()
	if host == nil || host.call == nil || host.free_buffer == nil {
		return zero, fmt.Errorf("host callback is unavailable")
	}
	rawRequest, err := json.Marshal(request)
	if err != nil {
		return zero, err
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var requestPtr *C.uint8_t
	if len(rawRequest) > 0 {
		requestPtr = (*C.uint8_t)(unsafe.Pointer(&rawRequest[0]))
	}
	var response C.cliproxy_buffer
	code := C.multi_protocol_call_host(host, cMethod, requestPtr, C.size_t(len(rawRequest)), &response)
	if response.ptr != nil {
		defer C.multi_protocol_free_host_buffer(host, response.ptr, response.len)
	}
	if code != 0 {
		return zero, fmt.Errorf("host callback %s failed with code %d", method, int(code))
	}
	if response.ptr == nil || response.len == 0 {
		return zero, nil
	}
	rawResponse := C.GoBytes(response.ptr, C.int(response.len))
	var envelope pluginabi.Envelope
	if err = json.Unmarshal(rawResponse, &envelope); err != nil {
		return zero, fmt.Errorf("decode host callback %s: %w", method, err)
	}
	if !envelope.OK {
		if envelope.Error != nil {
			return zero, fmt.Errorf("%s", envelope.Error.Message)
		}
		return zero, fmt.Errorf("host callback %s failed", method)
	}
	var output T
	if len(envelope.Result) > 0 {
		if err = json.Unmarshal(envelope.Result, &output); err != nil {
			return zero, fmt.Errorf("decode host callback %s result: %w", method, err)
		}
	}
	return output, nil
}

func abiOKEnvelope(value any) ([]byte, error) {
	result, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: result})
}

func abiResult(value any, err error) ([]byte, error) {
	if err != nil {
		return abiErrorFromError("plugin_error", err), nil
	}
	return abiOKEnvelope(value)
}

func abiErrorFromError(code string, err error) []byte {
	if err == nil {
		return abiErrorEnvelope(code, "", 0)
	}
	status := 0
	var provider interface{ StatusCode() int }
	if errors.As(err, &provider) {
		status = provider.StatusCode()
	}
	var requestScoped interface{ IsRequestScoped() bool }
	if errors.As(err, &requestScoped) && requestScoped.IsRequestScoped() {
		code = "request_scoped"
	}
	return abiErrorEnvelope(code, err.Error(), status)
}

func abiErrorEnvelope(code, message string, status int) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{
		OK: false,
		Error: &pluginabi.Error{
			Code:       code,
			Message:    message,
			HTTPStatus: status,
		},
	})
	return raw
}

func writeABIResponse(response *C.cliproxy_buffer, data []byte) {
	if response == nil || len(data) == 0 {
		return
	}
	ptr := C.malloc(C.size_t(len(data)))
	if ptr == nil {
		return
	}
	C.memcpy(ptr, unsafe.Pointer(&data[0]), C.size_t(len(data)))
	response.ptr = ptr
	response.len = C.size_t(len(data))
}
