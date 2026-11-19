//go:build js && wasm

package main

import (
	"context"
	"syscall/js"

	"vaultshare/go/gdrive"
)

// jsRequestUrlDo executes HTTP requests directly through obsidian.requestUrl via
// cached host handles and awaitPromise.
func jsRequestUrlDo(ctx context.Context, req *gdrive.HTTPRequest) (*gdrive.HTTPResponse, error) {
	h, err := getHostHandles()
	if err != nil {
		return nil, err
	}

	payload := h.objectCtor.New()
	payload.Set("method", req.Method)
	payload.Set("url", req.URL)
	payload.Set("throw", false)

	hdrObj := h.objectCtor.New()
	for k, v := range req.Headers {
		hdrObj.Set(k, v)
	}
	payload.Set("headers", hdrObj)

	if len(req.Body) > 0 {
		// Freshly allocated Uint8Array guarantees byteOffset === 0 for .buffer.
		u8 := h.uint8ArrayCtor.New(len(req.Body))
		js.CopyBytesToJS(u8, req.Body)
		payload.Set("body", u8.Get("buffer"))
	}

	resVal, err := awaitPromise(ctx, h.requestUrlFn.Invoke(payload))
	if err != nil {
		return nil, err
	}

	status := resVal.Get("status").Int()

	respHeaders := map[string]string{}
	hdrVal := resVal.Get("headers")
	if !hdrVal.IsUndefined() && !hdrVal.IsNull() {
		keys := h.objectCtor.Call("keys", hdrVal)
		n := keys.Length()
		for i := range n {
			k := keys.Index(i).String()
			respHeaders[k] = hdrVal.Get(k).String()
		}
	}

	var bodyBytes []byte
	ab := resVal.Get("arrayBuffer")
	if !ab.IsUndefined() && !ab.IsNull() {
		u8 := h.uint8ArrayCtor.New(ab)
		byteLen := u8.Get("byteLength").Int()
		if byteLen > 0 {
			bodyBytes = make([]byte, byteLen)
			js.CopyBytesToGo(bodyBytes, u8)
		}
	}

	return &gdrive.HTTPResponse{
		StatusCode: status,
		Headers:    respHeaders,
		Body:       bodyBytes,
	}, nil
}
