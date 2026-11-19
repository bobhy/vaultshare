/**
 * Workspace ItemView displaying live sync telemetry, Full Scan progress,
 * priority queue/latency metrics, resolution counters, and control actions.
 *
 * @packageDocumentation
 */

import { ButtonComponent, ItemView, type WorkspaceLeaf } from 'obsidian';
import type VaultSharePlugin from '../main';
import type { SyncStatusSnapshot } from '../settings.generated';

/** Unique Obsidian view type identifier for the VaultShare sync status panel. */
export const VIEW_TYPE_SYNC_STATUS = 'vaultshare-sync-status';

const AUTO_REFRESH_SECONDS = 3;

/**
 * Displays live Go Wasm sync status telemetry and control buttons while open.
 */
export class SyncStatusView extends ItemView {
	private readonly plugin: VaultSharePlugin;
	private bodyEl: HTMLElement | null = null;
	private refreshBtn: ButtonComponent | null = null;
	private countdown = AUTO_REFRESH_SECONDS;
	private lastSnapshot: SyncStatusSnapshot | null = null;

	/** Creates a sync status workspace view bound to `plugin`. */
	constructor(leaf: WorkspaceLeaf, plugin: VaultSharePlugin) {
		super(leaf);
		this.plugin = plugin;
	}

	/** Returns the Obsidian view type string. */
	getViewType(): string {
		return VIEW_TYPE_SYNC_STATUS;
	}

	/** Returns the display title rendered in the workspace tab header. */
	getDisplayText(): string {
		return 'VaultShare sync status';
	}

	/** Returns the Lucide icon name rendered on the tab. */
	getIcon(): string {
		return 'activity';
	}

	/** Renders the status view, subscribes to status events, and starts auto-refresh polling. */
	async onOpen(): Promise<void> {
		const { contentEl } = this;
		contentEl.empty();
		contentEl.addClass('vaultshare-view');

		const header = contentEl.createDiv({ cls: 'vaultshare-view-header' });
		header.createEl('h4', { text: 'Sync status' });

		const toolbar = header.createDiv({ cls: 'vaultshare-toolbar' });
		this.refreshBtn = new ButtonComponent(toolbar)
			.setButtonText(`Refresh (${this.countdown}s)`)
			.onClick(() => {
				this.countdown = AUTO_REFRESH_SECONDS;
				void this.refreshSnapshot();
			});

		new ButtonComponent(toolbar)
			.setButtonText('Scan now')
			.onClick(() => {
				void this.plugin.startFullScan().then(() => this.refreshSnapshot());
			});

		new ButtonComponent(toolbar)
			.setButtonText('Flush and sync now')
			.setCta()
			.onClick(() => {
				void this.plugin.flushAndNotify().then(() => this.refreshSnapshot());
			});

		this.bodyEl = contentEl.createDiv({ cls: 'vaultshare-status-body' });

		const bridge = this.plugin.getBridge();
		if (bridge) {
			this.registerEvent(
				bridge.on('globalStatus', () => {
					void this.refreshSnapshot();
				}),
			);
		}

		this.registerInterval(
			window.setInterval(() => {
				this.countdown -= 1;
				if (this.countdown <= 0) {
					this.countdown = AUTO_REFRESH_SECONDS;
					void this.refreshSnapshot();
				} else {
					this.refreshBtn?.setButtonText(`Refresh (${this.countdown}s)`);
				}
			}, 1000),
		);

		await this.refreshSnapshot();
	}

	/** Clears temporary view DOM references on panel close. */
	onClose(): Promise<void> {
		this.bodyEl = null;
		this.refreshBtn = null;
		return Promise.resolve();
	}

	private async refreshSnapshot(): Promise<void> {
		const bridge = this.plugin.getBridge();
		if (!bridge || !this.bodyEl) {
			return;
		}
		this.refreshBtn?.setButtonText(`Refresh (${this.countdown}s)`);
		try {
			this.lastSnapshot = await bridge.getStatusSnapshot();
			this.renderBody();
		} catch (err: unknown) {
			this.bodyEl.empty();
			const msg = err instanceof Error ? err.message : String(err);
			this.bodyEl.createDiv({ cls: 'vaultshare-error', text: `Failed to load status: ${msg}` });
		}
	}

	private renderBody(): void {
		if (!this.bodyEl || !this.lastSnapshot) {
			return;
		}
		const snap = this.lastSnapshot;
		this.bodyEl.empty();

		// State & primary CTA row.
		const stateCard = this.bodyEl.createDiv({ cls: 'vaultshare-card' });
		const stateRow = stateCard.createDiv({ cls: 'vaultshare-row' });
		const stateText = snap.needsLogin
			? 'Authorization required'
			: snap.paused
				? 'Paused'
				: 'Running';
		stateRow.createSpan({ cls: 'vaultshare-state-badge', text: stateText });

		const actionsEl = stateRow.createDiv({ cls: 'vaultshare-toolbar' });
		if (snap.needsLogin) {
			new ButtonComponent(actionsEl)
				.setButtonText('Log in')
				.setCta()
				.onClick(() => {
					this.plugin.startGoogleLogin();
				});
		} else if (snap.paused) {
			new ButtonComponent(actionsEl)
				.setButtonText('Resume')
				.setCta()
				.onClick(() => {
					void this.plugin.resumeSync().then(() => this.refreshSnapshot());
				});
		} else {
			new ButtonComponent(actionsEl)
				.setButtonText('Pause')
				.onClick(() => {
					void this.plugin.pauseSync().then(() => this.refreshSnapshot());
				});
		}

		// Full scan progress.
		const scanCard = this.bodyEl.createDiv({ cls: 'vaultshare-card' });
		scanCard.createEl('h5', { text: 'Full scan' });
		const pct =
			snap.fullScan.total > 0
				? Math.min(100, Math.round((snap.fullScan.scanned / snap.fullScan.total) * 100))
				: 0;
		const progressEl = scanCard.createEl('progress');
		progressEl.max = 100;
		progressEl.value = snap.fullScan.active ? pct : 0;
		const scanLabel = snap.fullScan.active
			? `${snap.fullScan.scanned} / ${snap.fullScan.total} (${snap.fullScan.filesPerSec.toFixed(1)} files/s)`
			: snap.fullScan.scanned > 0
				? `Idle — last scan checked ${snap.fullScan.scanned} files (${snap.fullScan.filesPerSec.toFixed(1)} files/s)`
				: 'Idle';
		scanCard.createDiv({ cls: 'vaultshare-muted', text: scanLabel });

		// Queues & latency.
		const queueCard = this.bodyEl.createDiv({ cls: 'vaultshare-card' });
		queueCard.createEl('h5', { text: 'Queues and latency' });
		queueCard.createDiv({
			text: `High: ${snap.queued.high} • Bulk: ${snap.queued.bulk} • In flight: ${snap.inFlight}`,
		});
		const latEl = queueCard.createDiv({
			cls: 'vaultshare-muted',
			text: `High-priority latency EWMA: ${snap.highPriLatencyMs.ewma.toFixed(1)} ms`,
		});
		latEl.setAttribute(
			'title',
			`Last: ${snap.highPriLatencyMs.last.toFixed(1)} ms • Max: ${snap.highPriLatencyMs.max.toFixed(1)} ms`,
		);

		// Conflicts.
		const confCard = this.bodyEl.createDiv({ cls: 'vaultshare-card' });
		confCard.createEl('h5', { text: 'Conflicts' });
		const confRow = confCard.createDiv({ cls: 'vaultshare-row' });
		confRow.createSpan({
			text: `Markers: ${snap.conflicts.markers} • Versions: ${snap.conflicts.versions} • Missing: ${snap.conflicts.missing}`,
		});
		if (snap.conflicts.total > 0) {
			new ButtonComponent(confRow)
				.setButtonText(`Reconcile (${snap.conflicts.total})`)
				.setCta()
				.onClick(() => {
					void this.plugin.openReconcileView();
				});
		}

		// Session resolution counters.
		const resCard = this.bodyEl.createDiv({ cls: 'vaultshare-card' });
		resCard.createEl('h5', { text: 'Session resolutions' });
		const r = snap.sessionResolutions;
		const grid = resCard.createDiv({ cls: 'vaultshare-metrics-grid' });
		const pairs: Array<[string, number]> = [
			['Merged', r.merged],
			['Pushed', r.pushed],
			['Pulled', r.pulled],
			['Renamed', r.renamed],
			['Trashed', r.trashed],
			['Kept newer', r.keptNewer],
			['Kept older', r.keptOlder],
			['Kept local', r.keptLocal],
			['Kept Drive', r.keptDrive],
			['Manual flagged', r.manualFlagged],
		];
		for (const [label, count] of pairs) {
			const cell = grid.createDiv({ cls: 'vaultshare-metric-cell' });
			cell.createSpan({ cls: 'vaultshare-muted', text: label });
			cell.createSpan({ text: String(count) });
		}
	}
}
