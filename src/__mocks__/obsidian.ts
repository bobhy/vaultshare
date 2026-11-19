/**
 * Minimal Obsidian API mock for Vitest unit tests running in Node.js.
 *
 * @packageDocumentation
 */

import type { RequestUrlParam, RequestUrlResponse } from 'obsidian';

/** Mirrors Obsidian's `Platform` flags for unit tests. */
export const Platform = {
	isIosApp: false,
	isMobile: false,
};

/** Minimal `Notice` mock for unit tests. */
export class Notice {
	readonly message: string;
	readonly timeout?: number;

	/** Records the notice message and optional duration. */
	constructor(message: string, timeout?: number) {
		this.message = message;
		this.timeout = timeout;
	}
}

/** Mirrors Obsidian's `normalizePath` for flat and nested POSIX vault paths. */
export function normalizePath(path: string): string {
	return path
		.replace(/\\/g, '/')
		.replace(/\/+/g, '/')
		.replace(/^\/+|\/+$/g, '');
}

/** Minimal `Events` mock supporting `on`, `offref`, and `trigger`. */
export class Events {
	private readonly listeners = new Map<string, Set<(...data: unknown[]) => unknown>>();

	/** Registers a callback for the named event and returns an `EventRef` handle. */
	on(name: string, callback: (...data: unknown[]) => unknown): { name: string; callback: (...data: unknown[]) => unknown } {
		let set = this.listeners.get(name);
		if (!set) {
			set = new Set();
			this.listeners.set(name, set);
		}
		set.add(callback);
		return { name, callback };
	}

	/** Removes a previously registered event reference. */
	offref(ref: { name: string; callback: (...data: unknown[]) => unknown }): void {
		this.listeners.get(ref.name)?.delete(ref.callback);
	}

	/** Invokes all listeners registered for the named event. */
	trigger(name: string, ...data: unknown[]): void {
		const set = this.listeners.get(name);
		if (!set) {
			return;
		}
		for (const cb of set) {
			cb(...data);
		}
	}
}

/** Minimal `PluginSettingTab` stub for tests importing settings modules. */
export class PluginSettingTab {
	app: unknown;
	plugin: unknown;

	/** Stores references for mocked tab construction. */
	constructor(app: unknown, plugin: unknown) {
		this.app = app;
		this.plugin = plugin;
	}
}

/** Minimal `ItemView` stub for unit tests. */
export class ItemView {}

/** Minimal `MarkdownView` stub for unit tests. */
export class MarkdownView {}

/** Minimal `TextFileView` stub for unit tests. */
export class TextFileView {}

/** Minimal `TFile` stub for unit tests. */
export class TFile {}

/** No-op `setIcon` mock setting a data attribute on the element when present. */
export function setIcon(el: { setAttribute?: (k: string, v: string) => void }, icon: string): void {
	el.setAttribute?.('data-icon', icon);
}

/** Fallback stub for `requestUrl`; unit tests inject a mock requester directly. */
export function requestUrl(_request: RequestUrlParam): Promise<RequestUrlResponse> {
	return Promise.reject(new Error('requestUrl mock not configured'));
}
