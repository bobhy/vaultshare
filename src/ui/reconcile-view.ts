/**
 * Non-modal workspace view (`ReconcileView`) stepping through all unresolved
 * `markers`, `versions`, and `missing` conflicts with lazy candidate previews.
 *
 * @packageDocumentation
 */

import { ButtonComponent, ItemView, MarkdownView, Notice, TFile, type WorkspaceLeaf } from 'obsidian';
import type VaultSharePlugin from '../main';
import type {
	ConflictCandidate,
	ConflictItem,
	GetConflictContentResult,
} from '../settings.generated';

/** Unique Obsidian view type identifier for the VaultShare conflict reconciliation panel. */
export const VIEW_TYPE_RECONCILE = 'vaultshare-reconcile';

/**
 * Displays unresolved manual sync conflicts and provides non-modal resolution controls.
 */
export class ReconcileView extends ItemView {
	private readonly plugin: VaultSharePlugin;
	private listEl: HTMLElement | null = null;
	private detailEl: HTMLElement | null = null;
	private conflicts: ConflictItem[] = [];
	private selectedPath: string | null = null;
	private previewCache = new Map<string, GetConflictContentResult>();
	private busy = false;

	/** Creates a conflict reconciliation view bound to `plugin`. */
	constructor(leaf: WorkspaceLeaf, plugin: VaultSharePlugin) {
		super(leaf);
		this.plugin = plugin;
	}

	/** Returns the Obsidian view type string. */
	getViewType(): string {
		return VIEW_TYPE_RECONCILE;
	}

	/** Returns the display title rendered in the workspace tab header. */
	getDisplayText(): string {
		return 'VaultShare reconcile conflicts';
	}

	/** Returns the Lucide icon name rendered on the tab. */
	getIcon(): string {
		return 'alert-triangle';
	}

	/** Builds the reconcile split layout, subscribes to status changes, and loads conflicts. */
	async onOpen(): Promise<void> {
		const { contentEl } = this;
		contentEl.empty();
		contentEl.addClass('vaultshare-view');

		const header = contentEl.createDiv({ cls: 'vaultshare-view-header' });
		header.createEl('h4', { text: 'Reconcile conflicts' });

		const toolbar = header.createDiv({ cls: 'vaultshare-toolbar' });
		new ButtonComponent(toolbar)
			.setButtonText('Refresh')
			.onClick(() => {
				this.previewCache.clear();
				void this.loadConflicts();
			});

		const layout = contentEl.createDiv({ cls: 'vaultshare-reconcile-layout' });
		this.listEl = layout.createDiv({ cls: 'vaultshare-reconcile-list' });
		this.detailEl = layout.createDiv({ cls: 'vaultshare-reconcile-detail' });

		const bridge = this.plugin.getBridge();
		if (bridge) {
			this.registerEvent(
				bridge.on('globalStatus', () => {
					void this.loadConflicts();
				}),
			);
		}

		await this.loadConflicts();
	}

	/** Releases temporary DOM/cache references on close. */
	onClose(): Promise<void> {
		this.listEl = null;
		this.detailEl = null;
		this.previewCache.clear();
		return Promise.resolve();
	}

	private async loadConflicts(): Promise<void> {
		const bridge = this.plugin.getBridge();
		if (!bridge || !this.listEl || !this.detailEl) {
			return;
		}
		try {
			this.conflicts = await bridge.listConflicts();
			if (
				this.selectedPath &&
				!this.conflicts.some((c) => c.path === this.selectedPath)
			) {
				this.selectedPath = null;
			}
			if (!this.selectedPath && this.conflicts.length > 0) {
				this.selectedPath = this.conflicts[0]!.path;
			}
			this.render();
		} catch (err: unknown) {
			const msg = err instanceof Error ? err.message : String(err);
			this.detailEl.empty();
			this.detailEl.createDiv({
				cls: 'vaultshare-error',
				text: `Failed to load conflicts: ${msg}`,
			});
		}
	}

	private render(): void {
		if (!this.listEl || !this.detailEl) {
			return;
		}
		this.listEl.empty();
		this.detailEl.empty();

		if (this.conflicts.length === 0) {
			this.listEl.createDiv({ cls: 'vaultshare-muted', text: 'All clear.' });
			this.detailEl.createDiv({
				cls: 'vaultshare-card',
				text: 'No unresolved sync conflicts.',
			});
			return;
		}

		for (const item of this.conflicts) {
			const row = this.listEl.createDiv({
				cls:
					item.path === this.selectedPath
						? 'vaultshare-reconcile-item is-selected'
						: 'vaultshare-reconcile-item',
			});
			row.createDiv({ cls: 'vaultshare-reconcile-path', text: item.path });
			row.createSpan({ cls: 'vaultshare-badge', text: item.kind });
			row.addEventListener('click', () => {
				this.selectedPath = item.path;
				this.render();
			});
		}

		const activeIdx = this.conflicts.findIndex((c) => c.path === this.selectedPath);
		const active = activeIdx >= 0 ? this.conflicts[activeIdx]! : this.conflicts[0]!;
		this.renderDetail(active, activeIdx >= 0 ? activeIdx : 0);
	}

	private renderDetail(item: ConflictItem, idx: number): void {
		if (!this.detailEl) {
			return;
		}
		const card = this.detailEl.createDiv({ cls: 'vaultshare-card' });
		const navRow = card.createDiv({ cls: 'vaultshare-row' });
		navRow.createSpan({
			cls: 'vaultshare-muted',
			text: `Conflict ${idx + 1} of ${this.conflicts.length} • ${item.kind}`,
		});

		const navButtons = navRow.createDiv({ cls: 'vaultshare-toolbar' });
		if (this.conflicts.length > 1) {
			new ButtonComponent(navButtons)
				.setButtonText('Previous')
				.onClick(() => {
					const prev = (idx - 1 + this.conflicts.length) % this.conflicts.length;
					this.selectedPath = this.conflicts[prev]!.path;
					this.render();
				});
			new ButtonComponent(navButtons)
				.setButtonText('Next')
				.onClick(() => {
					const next = (idx + 1) % this.conflicts.length;
					this.selectedPath = this.conflicts[next]!.path;
					this.render();
				});
		}

		card.createEl('h5', { text: item.path });
		card.createDiv({ cls: 'vaultshare-muted', text: item.summary });

		if (item.kind === 'markers') {
			const markerActions = card.createDiv({ cls: 'vaultshare-toolbar' });
			new ButtonComponent(markerActions)
				.setButtonText('Open in editor')
				.setCta()
				.onClick(() => {
					void this.openMarkerInEditor(item.path);
				});
			new ButtonComponent(markerActions)
				.setButtonText('Mark resolved')
				.onClick(() => {
					void this.runClearMarkers(item.path);
				});
			return;
		}

		// Version / Missing conflict candidates.
		for (const cand of item.candidates) {
			this.renderCandidateCard(item, cand);
		}

		const deleteRow = this.detailEl.createDiv({ cls: 'vaultshare-toolbar' });
		new ButtonComponent(deleteRow)
			.setButtonText('Delete everywhere')
			.setDestructive()
			.onClick(() => {
				void this.resolveCandidate(item.path, 'delete');
			});
	}

	private renderCandidateCard(item: ConflictItem, cand: ConflictCandidate): void {
		if (!this.detailEl) {
			return;
		}
		const card = this.detailEl.createDiv({ cls: 'vaultshare-card' });
		const row = card.createDiv({ cls: 'vaultshare-row' });
		const title =
			cand.source === 'local'
				? 'Local copy'
				: `Drive copy${cand.fileId ? ` (${cand.fileId.slice(0, 8)}…)` : ''}`;
		row.createEl('strong', { text: title });

		const btns = row.createDiv({ cls: 'vaultshare-toolbar' });
		const cacheKey = `${item.path}::${cand.source}::${cand.fileId ?? ''}`;
		new ButtonComponent(btns)
			.setButtonText('Preview')
			.onClick(() => {
				void this.loadPreview(item.path, cand, cacheKey);
			});

		new ButtonComponent(btns)
			.setButtonText(cand.source === 'local' ? 'Keep local' : 'Keep Drive')
			.setCta()
			.onClick(() => {
				void this.resolveCandidate(item.path, cand.source, cand.fileId);
			});

		card.createDiv({
			cls: 'vaultshare-muted',
			text: `${cand.size} bytes • Modified ${cand.modifiedTime}${cand.trashed ? ' • Trashed' : ''}`,
		});

		const preview = this.previewCache.get(cacheKey);
		if (preview) {
			if (preview.isBinary) {
				card.createDiv({
					cls: 'vaultshare-muted',
					text: `Binary preview unavailable (${preview.mimeType}, ${preview.size} bytes).`,
				});
			} else {
				const pre = card.createEl('pre', { cls: 'vaultshare-preview-pre' });
				pre.setText(preview.text ?? '');
			}
		}
	}

	private async loadPreview(
		path: string,
		cand: ConflictCandidate,
		cacheKey: string,
	): Promise<void> {
		const bridge = this.plugin.getBridge();
		if (!bridge) {
			return;
		}
		try {
			const res = await bridge.getConflictContent({
				path,
				source: cand.source,
				fileId: cand.fileId,
			});
			this.previewCache.set(cacheKey, res);
			this.render();
		} catch (err: unknown) {
			const msg = err instanceof Error ? err.message : String(err);
			new Notice(`Failed to load preview: ${msg}`, 0);
		}
	}

	private async resolveCandidate(
		path: string,
		winner: string,
		driveFileId?: string,
	): Promise<void> {
		if (this.busy) {
			return;
		}
		const bridge = this.plugin.getBridge();
		if (!bridge) {
			return;
		}
		this.busy = true;
		try {
			await bridge.resolveVersionConflict({ path, winner, driveFileId });
			this.previewCache.clear();
			new Notice(`Resolved ${path} (${winner}).`, 0);
			await this.loadConflicts();
		} catch (err: unknown) {
			const msg = err instanceof Error ? err.message : String(err);
			new Notice(`Resolution failed: ${msg}`, 0);
		} finally {
			this.busy = false;
		}
	}

	private async runClearMarkers(path: string): Promise<void> {
		const bridge = this.plugin.getBridge();
		if (!bridge) {
			return;
		}
		try {
			await bridge.clearMarkerConflict(path);
			new Notice(`Cleared conflict marker flag on ${path}.`, 0);
			await this.loadConflicts();
		} catch (err: unknown) {
			const msg = err instanceof Error ? err.message : String(err);
			new Notice(`Cannot mark resolved: ${msg}`, 0);
		}
	}

	private async openMarkerInEditor(path: string): Promise<void> {
		const af = this.app.vault.getAbstractFileByPath(path);
		if (!(af instanceof TFile)) {
			new Notice(`File not found in vault: ${path}`, 0);
			return;
		}
		const leaf = this.app.workspace.getLeaf('tab');
		await leaf.openFile(af);
		if (leaf.view instanceof MarkdownView && leaf.view.editor) {
			this.plugin.getMarkerNavigator().navigateForward(leaf.view.editor, leaf.view);
		}
	}
}
