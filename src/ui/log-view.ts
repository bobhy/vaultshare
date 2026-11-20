/**
 * Workspace ItemView displaying live-streamed and buffered VaultShare sync logs
 * with severity filtering, pause/resume toggle, and log buffer clear.
 *
 * @packageDocumentation
 */

import { ButtonComponent, DropdownComponent, ItemView, Notice, type WorkspaceLeaf } from 'obsidian';
import type VaultSharePlugin from '../main';
import type { GlobalStatusEvent, LogEntry } from '../settings.generated';

/** Unique Obsidian view type identifier for the VaultShare sync log panel. */
export const VIEW_TYPE_LOG_VIEW = 'vaultshare-log-view';

/** Severity level rank helper matching Go's `severityRank` semantics. */
export function severityRank(sev: string): number {
	switch (sev) {
		case 'DEBUG':
			return 0;
		case 'INFO':
			return 1;
		case 'WARNING':
			return 2;
		case 'ERROR':
			return 3;
		default:
			return 1;
	}
}

/**
 * Formats a `LogEntry` into a single-line message, joining non-empty details
 * with `": "` and collapsing embedded newlines into spaces.
 */
export function formatLogMessage(entry: LogEntry): string {
	const msg = entry.message.replace(/\s+/g, ' ').trim();
	const detail = entry.detail ? entry.detail.replace(/\s+/g, ' ').trim() : '';
	return detail ? `${msg}: ${detail}` : msg;
}

/** Formats a full single-line log record (`HH:MM:SS SEVERITY message: detail`) for clipboard copy. */
export function formatLogLine(entry: LogEntry): string {
	const ts = new Date(entry.timestampMs).toLocaleTimeString();
	return `${ts} ${entry.severity} ${formatLogMessage(entry)}`;
}

/**
 * Displays structured sync engine logs fetched from Go's ring buffer and pushed
 * live while open.
 */
export class LogView extends ItemView {
	private readonly plugin: VaultSharePlugin;
	private listEl: HTMLElement | null = null;
	private pauseBtn: ButtonComponent | null = null;
	private minSeverity = 'INFO';
	private entries: LogEntry[] = [];
	private paused = false;
	private streamConnected = false;
	private connecting = false;

	/** Creates a sync log workspace view bound to `plugin`. */
	constructor(leaf: WorkspaceLeaf, plugin: VaultSharePlugin) {
		super(leaf);
		this.plugin = plugin;
	}

	/** Returns the Obsidian view type string. */
	getViewType(): string {
		return VIEW_TYPE_LOG_VIEW;
	}

	/** Returns the display title rendered in the workspace tab header. */
	getDisplayText(): string {
		return 'VaultShare sync log';
	}

	/** Returns the Lucide icon name rendered on the tab. */
	getIcon(): string {
		return 'scroll-text';
	}

	/** Hydrates the Go log ring buffer and enables live log streaming while open. */
	async onOpen(): Promise<void> {
		const { contentEl } = this;
		contentEl.empty();
		contentEl.addClass('vaultshare-view');

		const header = contentEl.createDiv({ cls: 'vaultshare-view-header' });
		header.createEl('h4', { text: 'Sync log' });

		const toolbar = header.createDiv({ cls: 'vaultshare-toolbar' });
		new DropdownComponent(toolbar)
			.addOptions({
				DEBUG: 'Debug',
				INFO: 'Info',
				WARNING: 'Warning',
				ERROR: 'Error',
			})
			.setValue(this.minSeverity)
			.onChange((val) => {
				this.minSeverity = val;
				this.renderEntries();
			});

		this.pauseBtn = new ButtonComponent(toolbar)
			.setButtonText('Pause')
			.onClick(() => {
				if (this.paused) {
					void this.plugin.resumeSync();
				} else {
					void this.plugin.pauseSync();
				}
			});

		new ButtonComponent(toolbar)
			.setButtonText('Copy')
			.onClick(async () => {
				const filtered = this.filteredEntries();
				const text = filtered.reverse().map((e) => formatLogLine(e)).join('\n');
				if (!text) {
					new Notice('No log entries to copy.', 0);
					return;
				}
				try {
					await navigator.clipboard.writeText(text);
					new Notice(`Copied ${filtered.length} log line(s) to clipboard.`, 0);
				} catch (err: unknown) {
					const msg = err instanceof Error ? err.message : String(err);
					new Notice(`Failed to copy logs: ${msg}`, 0);
				}
			});

		new ButtonComponent(toolbar)
			.setButtonText('Clear')
			.onClick(() => {
				this.entries = [];
				this.renderEntries();
			});

		this.listEl = contentEl.createDiv({ cls: 'vaultshare-log-list' });
		this.streamConnected = false;
		this.connecting = false;

		const bridge = this.plugin.getBridge();
		if (!bridge) {
			this.listEl.createDiv({ cls: 'vaultshare-muted', text: 'Engine not started yet.' });
			return;
		}

		this.registerEvent(
			bridge.on('logEntry', (payload: unknown) => {
				const entry = payload as LogEntry;
				this.entries.push(entry);
				if (this.entries.length > 500) {
					this.entries = this.entries.slice(this.entries.length - 500);
				}
				this.renderEntries();
			}),
		);
		this.registerEvent(
			bridge.on('globalStatus', (payload: unknown) => {
				const ev = payload as GlobalStatusEvent;
				this.paused = ev.paused;
				this.pauseBtn?.setButtonText(this.paused ? 'Resume' : 'Pause');
				if (!this.streamConnected) {
					void this.connectToStream();
				}
			}),
		);

		await this.connectToStream();
	}

	/** Disables live log streaming from Go when the panel closes. */
	async onClose(): Promise<void> {
		const wasConnected = this.streamConnected;
		this.streamConnected = false;
		this.connecting = false;
		const bridge = this.plugin.getBridge();
		if (bridge && wasConnected && this.plugin.isEngineStarted()) {
			try {
				await bridge.setLogStream(false);
			} catch {
				// Bridge may already be unloading.
			}
		}
		this.listEl = null;
		this.pauseBtn = null;
	}

	private async connectToStream(): Promise<void> {
		if (this.streamConnected || this.connecting || !this.listEl) {
			return;
		}
		const bridge = this.plugin.getBridge();
		if (!bridge || !this.plugin.isEngineStarted()) {
			this.listEl.empty();
			this.listEl.createDiv({
				cls: 'vaultshare-muted',
				text: 'Waiting for sync engine to start...',
			});
			return;
		}

		this.connecting = true;
		try {
			const [snap, initial] = await Promise.all([
				bridge.getStatusSnapshot(),
				bridge.getLogs('DEBUG'),
			]);
			if (!this.listEl) {
				return;
			}
			this.paused = snap.paused;
			this.pauseBtn?.setButtonText(this.paused ? 'Resume' : 'Pause');
			this.entries = initial;
			this.renderEntries();
			await bridge.setLogStream(true);
			this.streamConnected = true;
		} catch (err: unknown) {
			if (this.listEl) {
				this.listEl.empty();
				const msg = err instanceof Error ? err.message : String(err);
				this.listEl.createDiv({ cls: 'vaultshare-error', text: `Failed to load logs: ${msg}` });
			}
		} finally {
			this.connecting = false;
		}
	}

	private filteredEntries(): LogEntry[] {
		const minRank = severityRank(this.minSeverity);
		return this.entries.filter((e) => severityRank(e.severity) >= minRank);
	}

	private renderEntries(): void {
		if (!this.listEl) {
			return;
		}
		this.listEl.empty();
		const filtered = this.filteredEntries();
		if (filtered.length === 0) {
			this.listEl.createDiv({ cls: 'vaultshare-muted', text: 'No matching log entries.' });
			return;
		}

		for (let i = filtered.length - 1; i >= 0; i--) {
			const e = filtered[i]!;
			const row = this.listEl.createDiv({
				cls: `vaultshare-log-row sev-${e.severity.toLowerCase()}`,
			});
			const ts = new Date(e.timestampMs).toLocaleTimeString();
			const msg = e.message.replace(/\s+/g, ' ').trim();
			const detail = e.detail ? e.detail.replace(/\s+/g, ' ').trim() : '';
			row.createSpan({ cls: 'vaultshare-log-time', text: ts });
			row.createSpan({ cls: 'vaultshare-log-sev', text: e.severity });
			row.createSpan({
				cls: 'vaultshare-log-msg',
				text: detail ? `${msg}: ` : msg,
			});
			if (detail) {
				row.createSpan({ cls: 'vaultshare-log-detail', text: detail });
			}
			row.setAttribute('title', formatLogMessage(e));
		}
	}
}
