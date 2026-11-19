/**
 * Shared TypeScript interfaces for the Go WebAssembly runtime bridge and host handles.
 *
 * @packageDocumentation
 */

import type { App } from 'obsidian';

/** Shape of the global `Go` class installed by Go's `wasm_exec.js`. */
export interface GoRuntimeInstance {
	importObject: WebAssembly.Imports;
	run(instance: WebAssembly.Instance): Promise<void>;
}

/** Constructor for Go's `wasm_exec.js` runtime class. */
export type GoRuntimeConstructor = new () => GoRuntimeInstance;

/** Functions exported onto `window.__vaultshare` by `go/cmd/plugin`. */
export interface WasmBridgeApi {
	invoke(method: string, payloadJson: string): Promise<string>;
	shutdown(): void;
}

/** Single-root host capabilities injected once onto `window.__vaultshareHost`. */
export interface WasmHostBridge {
	app: App;
	obsidian: typeof import('obsidian');
	withOpenFileLock: (path: string, fn: () => Promise<void>) => Promise<void>;
	emitEvent: (kind: string, jsonPayload: string) => void;
}

/** Window extension exposing the Go runtime constructor and plugin Wasm bridge. */
export interface WasmWindowGlobals {
	Go?: GoRuntimeConstructor;
	__vaultshare?: WasmBridgeApi;
	__vaultshareHost?: WasmHostBridge;
}
