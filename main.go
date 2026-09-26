// Command cline-for-cpa is a CLIProxyAPI C ABI plugin that reverse-proxies
// OpenAI-compatible chat completions to api.cline.bot with an OpenCodex-T04 /
// cursor-for-cpa Stream Guard (first-frame / silence / heartbeat-only).
package main

/*
#include <stdint.h>
#include <stdlib.h>

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

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"fmt"
	"unsafe"

	"cline-for-cpa/plugin"
)

const abiVersion uint32 = 1

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, pluginTable *C.cliproxy_plugin_api) C.int {
	if pluginTable == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.SetHostCaller(callHost)
	pluginTable.abi_version = C.uint32_t(abiVersion)
	pluginTable.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	pluginTable.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	pluginTable.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, plugin.ErrorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var req []byte
	if request != nil && requestLen > 0 {
		req = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := plugin.HandleMethod(C.GoString(method), req)
	if err != nil {
		writeResponse(response, plugin.ErrorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func callHost(method string, request []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cReq *C.uint8_t
	if len(request) > 0 {
		cReq = (*C.uint8_t)(C.CBytes(request))
		defer C.free(unsafe.Pointer(cReq))
	}
	var buf C.cliproxy_buffer
	rc := C.call_host_api(cMethod, cReq, C.size_t(len(request)), &buf)
	if buf.ptr == nil {
		if rc != 0 {
			return nil, fmt.Errorf("host call %s failed with code %d", method, int(rc))
		}
		return nil, nil
	}
	out := C.GoBytes(buf.ptr, C.int(buf.len))
	C.free_host_buffer(buf.ptr, buf.len)
	if rc != 0 {
		return out, fmt.Errorf("host call %s failed with code %d", method, int(rc))
	}
	return out, nil
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
