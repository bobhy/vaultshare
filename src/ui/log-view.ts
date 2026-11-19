/**
 * Workspace ItemView displaying live-streamed and buffered VaultShare sync logs
 * with severity filtering, pause/resume toggle, and log buffer clear.
 *
 * @packageDocumentation
 */

import { ButtonComponent, DropdownComponent, ItemView, type WorkspaceLeaf } from 'obsidian';
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
			.setButtonText('Clear')
			.onClick(() => {
				this.entries = [];
				this.renderEntries();
			});

		this.listEl = contentEl.createDiv({ cls: 'vaultshare-log-list' });

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
			}),
		);

		try {
			const [snap, initial] = await Promise.all([
				bridge.getStatusSnapshot(),
				bridge.getLogs('DEBUG'),
			]);
			this.paused = snap.paused;
			this.pauseBtn.setButtonText(this.paused ? 'Resume' : 'Pause');
			this.entries = initial;
			this.renderEntries();
			await bridge.setLogStream(true);
		} catch (err: unknown) {
			const msg = err instanceof Error ? err.message : String(err);
			this.listEl.createDiv({ cls: 'vaultshare-error', text: `Failed to load logs: ${msg}` });
		}
	}

	/** Disables live log streaming from Go when the panel closes. */
	async onClose(): Promise<void> {
		const bridge = this.plugin.getBridge();
		if (bridge) {
			try {
				await bridge.setLogStream(false);
			} catch {
				// Bridge may already be unloading.
			}
		}
		this.listEl = null;
		this.pauseBtn = null;
	}

	private renderEntries(): void {
		if (!this.listEl) {
			return;
		}
		this.listEl.empty();
		const minRank = severityRank(this.minSeverity);
		const filtered = this.entries.filter((e) => severityRank(e.severity) >= minRank);
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
			row.createSpan({ cls: 'vaultshare-log-time', text: ts });
			row.createSpan({ cls: 'vaultshare-log-sev', text: e.severity });
			row.createSpan({ cls: 'vaultshare-log-msg', text: e.message });
			if (e.detail) {
				row.createDiv({ cls: 'vaultshare-log-detail', text: e.detail });
			}
		}
	}
}
