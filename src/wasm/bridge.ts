/**
 * Lazy-loading bridge between the Obsidian TypeScript plugin and the embedded Go Wasm module.
 *
 * @packageDocumentation
 */

import * as obsidian from 'obsidian';
import type { App } from 'obsidian';
import '../../build/wasm_exec.js';
import wasmBytes from '../../build/plugin.wasm';
import type {
	ConflictItem,
	FlushResult,
	GetConflictContentRequest,
	GetConflictContentResult,
	GlobalStatusEvent,
	LogEntry,
	PluginSettings,
	ResolveVersionConflictRequest,
	SyncStatusSnapshot,
	VaultEventPayload,
} from '../settings.generated';
import type { WasmBridgeApi, WasmWindowGlobals } from './types';
import {
	parseConflictItems,
	parseFlushResult,
	parseGetConflictContentResult,
	parseGlobalStatusEvent,
	parseLogEntries,
	parseLogEntry,
	parseSyncStatusSnapshot,
} from './validators';

const INIT_TIMEOUT_MS = 5000;
const POLL_INTERVAL_MS = 10;

/** Callback type supplied by UI open-file locking to serialize local file writes. */
export type OpenFileLockHandler = (path: string, fn: () => Promise<void>) => Promise<void>;

function wasmGlobals(): WasmWindowGlobals {
	return window as unknown as WasmWindowGlobals;
}

/**
 * Manages the lifecycle of the Go WebAssembly runtime and marshals Promise-based
 * RPC calls between Obsidian TypeScript code and Go.
 */
export class GoWasmBridge {
	private readyPromise: Promise<WasmBridgeApi> | null = null;
	private readonly app: App;
	private readonly events = new obsidian.Events();
	private lockHandler: OpenFileLockHandler;

	/** Creates a Wasm bridge bound to the active Obsidian `App`. */
	constructor(app: App, lockHandler?: OpenFileLockHandler) {
		this.app = app;
		this.lockHandler = lockHandler ?? (async (_path, fn) => {
			await fn();
		});
	}

	/** Replaces the open-file lock handler invoked by Go before local writes. */
	setOpenFileLockHandler(handler: OpenFileLockHandler): void {
		this.lockHandler = handler;
	}

	/** Subscribes to Go-emitted UI events (`globalStatus`, `logEntry`). */
	on(name: string, callback: (...data: unknown[]) => unknown): obsidian.EventRef {
		return this.events.on(name, callback);
	}

	/** Unsubscribes a previously registered event listener. */
	offref(ref: obsidian.EventRef): void {
		this.events.offref(ref);
	}

	/** Initializes the Wasm runtime, loads persisted state, and starts the Go sync engine. */
	async start(settings: PluginSettings): Promise<GlobalStatusEvent> {
		return parseGlobalStatusEvent(await this.invoke('start', settings));
	}

	/** Pushes updated plugin settings to the running Go engine. */
	async updateSettings(settings: PluginSettings): Promise<GlobalStatusEvent> {
		return parseGlobalStatusEvent(await this.invoke('updateSettings', settings));
	}

	/** Forwards a newly acquired OAuth refresh token to Go for persistence and sync resume. */
	async setRefreshToken(token: string): Promise<GlobalStatusEvent> {
		return parseGlobalStatusEvent(await this.invoke('setRefreshToken', { token }));
	}

	/** Clears local OAuth credentials in Go and enters `NeedsLogin`. */
	async logout(): Promise<GlobalStatusEvent> {
		return parseGlobalStatusEvent(await this.invoke('logout', {}));
	}

	/** Delivers an Obsidian Vault or Workspace event to Go's debouncer and priority queue. */
	async onVaultEvent(payload: VaultEventPayload): Promise<void> {
		await this.invoke('onVaultEvent', payload);
	}

	/** Flushes local debounces and waits for all High-priority sync tasks to drain. */
	async flushHighPriority(): Promise<FlushResult> {
		return parseFlushResult(await this.invoke('flushHighPriority', {}));
	}

	/** Pauses conflict resolution and persists pause state in IndexedDB. */
	async pause(): Promise<GlobalStatusEvent> {
		return parseGlobalStatusEvent(await this.invoke('pause', {}));
	}

	/** Resumes conflict resolution and wakes sync workers. */
	async resume(): Promise<GlobalStatusEvent> {
		return parseGlobalStatusEvent(await this.invoke('resume', {}));
	}

	/** Manually initiates a randomized Full Scan pass in the background. */
	async startFullScan(): Promise<void> {
		await this.invoke('startFullScan', {});
	}

	/** Fetches an O(1) telemetry snapshot for `SyncStatusView`. */
	async getStatusSnapshot(): Promise<SyncStatusSnapshot> {
		return parseSyncStatusSnapshot(await this.invoke('getStatusSnapshot', {}));
	}

	/** Hydrates buffered logs from Go filtered by optional minimum severity. */
	async getLogs(minSeverity?: string): Promise<LogEntry[]> {
		return parseLogEntries(await this.invoke('getLogs', { minSeverity }));
	}

	/** Toggles live `logEntry` pushes while `LogView` is open or closed. */
	async setLogStream(enabled: boolean): Promise<void> {
		await this.invoke('setLogStream', { enabled });
	}

	/** Lists all pending manual conflicts for `ReconcileView`. */
	async listConflicts(): Promise<ConflictItem[]> {
		return parseConflictItems(await this.invoke('listConflicts', {}));
	}

	/** Loads local or Drive candidate content for non-modal conflict preview. */
	async getConflictContent(req: GetConflictContentRequest): Promise<GetConflictContentResult> {
		return parseGetConflictContentResult(await this.invoke('getConflictContent', req));
	}

	/** Resolves a version or missing conflict with the user's explicit choice. */
	async resolveVersionConflict(req: ResolveVersionConflictRequest): Promise<void> {
		await this.invoke('resolveVersionConflict', req);
	}

	/** Verifies conflict markers were removed and clears a `markers` conflict flag. */
	async clearMarkerConflict(path: string): Promise<void> {
		await this.invoke('clearMarkerConflict', { path });
	}

	/** Clears all local Sync State DB records while retaining OAuth credentials. */
	async clearAllState(): Promise<GlobalStatusEvent> {
		return parseGlobalStatusEvent(await this.invoke('clearAllState', {}));
	}

	/** Shuts down the Go Wasm runtime and releases registered JS callbacks. */
	unload(): void {
		const globals = wasmGlobals();
		globals.__vaultshare?.shutdown();
		delete globals.__vaultshareHost;
		this.readyPromise = null;
	}

	private async invoke(method: string, payload: unknown): Promise<unknown> {
		const api = await this.ensureInitialized();
		const rawJson = await api.invoke(method, JSON.stringify(payload));
		return JSON.parse(rawJson) as unknown;
	}

	private ensureInitialized(): Promise<WasmBridgeApi> {
		if (!this.readyPromise) {
			this.readyPromise = this.init().catch((err: unknown) => {
				this.readyPromise = null;
				throw err;
			});
		}
		return this.readyPromise;
	}

	private async init(): Promise<WasmBridgeApi> {
		const globals = wasmGlobals();
		const GoCtor = globals.Go;
		if (!GoCtor) {
			throw new Error('Go wasm_exec.js runtime is not loaded');
		}

		globals.__vaultshareHost = {
			app: this.app,
			obsidian,
			withOpenFileLock: async (path: string, fn: () => Promise<void>): Promise<void> => {
				await this.lockHandler(path, fn);
			},
			emitEvent: (kind: string, jsonPayload: string): void => {
				const parsed: unknown = JSON.parse(jsonPayload);
				if (kind === 'notice' && typeof parsed === 'object' && parsed !== null) {
					const msg = (parsed as { message?: unknown }).message;
					if (typeof msg === 'string') {
						new obsidian.Notice(msg, 0);
					}
					return;
				}
				if (kind === 'globalStatus') {
					this.events.trigger(kind, parseGlobalStatusEvent(parsed));
					return;
				}
				if (kind === 'logEntry') {
					this.events.trigger(kind, parseLogEntry(parsed));
					return;
				}
				this.events.trigger(kind, parsed);
			},
		};

		const go = new GoCtor();
		const result = await WebAssembly.instantiate(
			wasmBytes as BufferSource,
			go.importObject,
		);

		void go.run(result.instance).catch((err: unknown) => {
			console.error('Go Wasm runtime exited with error:', err);
		});

		return await waitForBridge(INIT_TIMEOUT_MS);
	}
}

async function waitForBridge(timeoutMs: number): Promise<WasmBridgeApi> {
	const deadline = Date.now() + timeoutMs;
	while (Date.now() < deadline) {
		const api = wasmGlobals().__vaultshare;
		if (api) {
			return api;
		}
		await new Promise<void>((resolve) => {
			window.setTimeout(resolve, POLL_INTERVAL_MS);
		});
	}
	throw new Error('Timed out waiting for Go Wasm bridge initialization');
}
