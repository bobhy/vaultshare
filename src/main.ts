/**
 * Obsidian plugin entrypoint managing VaultShare V2 lifecycle, mutual-exclusion
 * checks, OAuth callback handling, settings persistence, and the Go Wasm bridge.
 *
 * @packageDocumentation
 */

import { MarkdownView, Notice, Plugin, TAbstractFile, TFile, TFolder } from 'obsidian';
import { OAUTH_PROTOCOL_ACTION, OAuthFlow, isLegacyVaultShareEnabled } from './auth/oauth';
import {
	DEFAULT_SETTINGS,
	VaultShareSettingTab,
	type PluginSettings,
} from './settings';
import type { GlobalStatusEvent, VaultEventPayload } from './settings.generated';
import {
	ConflictMarkerNavigator,
	conflictHighlightExtension,
} from './ui/conflict-marker-navigator';
import { GlobalStatusIndicator } from './ui/global-status-bar';
import { LogView, VIEW_TYPE_LOG_VIEW } from './ui/log-view';
import { OpenFileLocker, openFileLockExtension } from './ui/open-file-lock';
import { ReconcileView, VIEW_TYPE_RECONCILE } from './ui/reconcile-view';
import { SyncStatusView, VIEW_TYPE_SYNC_STATUS } from './ui/sync-status-view';
import { GoWasmBridge } from './wasm/bridge';

/**
 * VaultShare V2 Obsidian plugin entry point.
 */
export default class VaultSharePlugin extends Plugin {
	/** Plugin settings synchronized with the Go `settings.PluginSettings` struct. */
	settings!: PluginSettings;
	private readonly oauth = new OAuthFlow();
	private wasmBridge: GoWasmBridge | null = null;
	private statusIndicator: GlobalStatusIndicator | null = null;
	private currentStatus: GlobalStatusEvent = {
		paused: false,
		needsLogin: true,
		conflictsTotal: 0,
	};
	private markerNavigator!: ConflictMarkerNavigator;
	private dormant = false;

	/** Checks V1 mutual exclusion, loads settings, and defers Wasm init until layout ready. */
	async onload(): Promise<void> {
		if (await isLegacyVaultShareEnabled(this.app)) {
			this.dormant = true;
			new Notice(
				'VaultShare cannot run while the legacy vault-share plugin is enabled. Please disable vault-share and reload VaultShare.',
				0,
			);
			return;
		}

		await this.loadSettings();
		const locker = new OpenFileLocker(this.app);
		this.wasmBridge = new GoWasmBridge(this.app, (path, fn) =>
			locker.withLock(path, fn),
		);
		this.markerNavigator = new ConflictMarkerNavigator((filePath) => {
			if (this.wasmBridge) {
				void this.wasmBridge.clearMarkerConflict(filePath).catch(() => {
					// File may still contain markers or not yet be saved.
				});
			}
		});

		this.registerEditorExtension([
			openFileLockExtension,
			conflictHighlightExtension,
		]);

		this.registerView(VIEW_TYPE_SYNC_STATUS, (leaf) => new SyncStatusView(leaf, this));
		this.registerView(VIEW_TYPE_LOG_VIEW, (leaf) => new LogView(leaf, this));
		this.registerView(VIEW_TYPE_RECONCILE, (leaf) => new ReconcileView(leaf, this));

		this.statusIndicator = new GlobalStatusIndicator(this, {
			onOpenStatus: () => {
				void this.openSyncStatusView();
			},
			onOpenSettings: () => {
				this.openPluginSettings();
			},
			onOpenReconcile: () => {
				void this.openReconcileView();
			},
		});

		this.registerEvent(
			this.wasmBridge.on('globalStatus', (payload: unknown) => {
				this.applyGlobalStatus(payload as GlobalStatusEvent);
			}),
		);

		this.registerObsidianProtocolHandler(OAUTH_PROTOCOL_ACTION, (params) => {
			void this.handleOAuthCallback(params);
		});

		this.addSettingTab(new VaultShareSettingTab(this.app, this));
		this.registerCommands();

		this.registerDomEvent(activeDocument, 'visibilitychange', () => {
			if (activeDocument.visibilityState === 'hidden' && this.wasmBridge && !this.dormant) {
				void this.wasmBridge.flushHighPriority().catch(() => {
					// Best-effort flush when backgrounding mobile/desktop window.
				});
			}
		});

		this.app.workspace.onLayoutReady(() => {
			void this.startEngineAndWatchers();
		});
	}

	/** Shuts down the Go WebAssembly runtime when the plugin is unloaded. */
	onunload(): void {
		this.wasmBridge?.unload();
		this.wasmBridge = null;
	}

	/** Loads persisted settings merged with defaults generated from Go. */
	async loadSettings(): Promise<void> {
		this.settings = Object.assign(
			{},
			DEFAULT_SETTINGS,
			(await this.loadData()) as Partial<PluginSettings>,
		);
	}

	/** Persists the current plugin settings and notifies the Go engine if running. */
	async saveSettings(): Promise<void> {
		await this.saveData(this.settings);
		if (this.wasmBridge && !this.dormant) {
			try {
				await this.wasmBridge.updateSettings(this.settings);
			} catch (err: unknown) {
				console.error('Failed to push settings update to Go engine:', err);
			}
		}
	}

	/** Launches the Google OAuth 2.0 consent flow in the user's system browser. */
	startGoogleLogin(): void {
		if (this.dormant) {
			return;
		}
		this.oauth.startLogin();
	}

	/** Clears local OAuth credentials without revoking other devices' grants. */
	async logoutFromGoogle(): Promise<void> {
		if (!this.wasmBridge || this.dormant) {
			return;
		}
		try {
			const status = await this.wasmBridge.logout();
			this.applyGlobalStatus(status);
			new Notice('Logged out of Google Drive.', 0);
		} catch (err: unknown) {
			const msg = err instanceof Error ? err.message : String(err);
			new Notice(`VaultShare logout error: ${msg}`, 0);
		}
	}

	/** Exposes the active Wasm bridge instance for Milestone 6 workspace views. */
	getBridge(): GoWasmBridge | null {
		return this.wasmBridge;
	}

	/** Returns the latest known engine status (paused, needsLogin, conflictsTotal). */
	getGlobalStatus(): GlobalStatusEvent {
		return this.currentStatus;
	}

	/** Exposes the conflict marker navigator for editor and reconcile views. */
	getMarkerNavigator(): ConflictMarkerNavigator {
		return this.markerNavigator;
	}

	/** Opens or focuses the VaultShare sync status view. */
	async openSyncStatusView(): Promise<void> {
		await this.activateView(VIEW_TYPE_SYNC_STATUS);
	}

	/** Opens or focuses the VaultShare sync log view. */
	async openLogView(): Promise<void> {
		await this.activateView(VIEW_TYPE_LOG_VIEW);
	}

	/** Opens or focuses the non-modal Reconcile conflicts tab. */
	async openReconcileView(): Promise<void> {
		await this.activateView(VIEW_TYPE_RECONCILE);
	}

	/** Opens Obsidian Settings directly to VaultShare's configuration tab. */
	openPluginSettings(): void {
		const settingApi = (
			this.app as unknown as {
				setting?: { open: () => void; openTabById: (id: string) => void };
			}
		).setting;
		settingApi?.open();
		settingApi?.openTabById(this.manifest.id);
	}

	/** Pauses conflict resolution and persists pause state in IndexedDB. */
	async pauseSync(): Promise<GlobalStatusEvent | null> {
		if (!this.wasmBridge || this.dormant) {
			return null;
		}
		const status = await this.wasmBridge.pause();
		this.applyGlobalStatus(status);
		return status;
	}

	/** Resumes conflict resolution and wakes Go workers/polling. */
	async resumeSync(): Promise<GlobalStatusEvent | null> {
		if (!this.wasmBridge || this.dormant) {
			return null;
		}
		const status = await this.wasmBridge.resume();
		this.applyGlobalStatus(status);
		return status;
	}

	/** Triggers an immediate background Full Scan pass. */
	async startFullScan(): Promise<void> {
		if (!this.wasmBridge || this.dormant) {
			return;
		}
		await this.wasmBridge.startFullScan();
	}

	/** Cancels local debounces, flushes High-priority sync tasks, and notifies result. */
	async flushAndNotify(): Promise<boolean> {
		if (!this.wasmBridge || this.dormant) {
			return false;
		}
		const res = await this.wasmBridge.flushHighPriority();
		new Notice(
			res.drained ? 'Synced — safe to switch.' : 'Sync flush timed out or paused.',
			0,
		);
		return res.drained;
	}

	/** Clears all local Sync State DB records while retaining OAuth credentials. */
	async clearAllState(): Promise<void> {
		if (!this.wasmBridge || this.dormant) {
			return;
		}
		const status = await this.wasmBridge.clearAllState();
		this.applyGlobalStatus(status);
		new Notice('VaultShare local sync state cleared.', 0);
	}

	private async activateView(viewType: string): Promise<void> {
		const existing = this.app.workspace.getLeavesOfType(viewType);
		if (existing.length > 0 && existing[0]) {
			await this.app.workspace.revealLeaf(existing[0]);
			return;
		}
		const leaf = this.app.workspace.getLeaf('tab');
		await leaf.setViewState({ type: viewType, active: true });
		await this.app.workspace.revealLeaf(leaf);
	}

	private registerCommands(): void {
		this.addCommand({
			id: 'reconcile-conflicts',
			name: 'Reconcile conflicts',
			callback: () => {
				void this.openReconcileView();
			},
		});
		this.addCommand({
			id: 'open-sync-status',
			name: 'Open sync status',
			callback: () => {
				void this.openSyncStatusView();
			},
		});
		this.addCommand({
			id: 'open-sync-log',
			name: 'Open sync log',
			callback: () => {
				void this.openLogView();
			},
		});
		this.addCommand({
			id: 'find-next-conflict',
			name: 'Find next conflict',
			editorCallback: (editor, view) => {
				const mdView = view instanceof MarkdownView ? view : undefined;
				this.markerNavigator.navigateForward(editor, mdView);
			},
		});
		this.addCommand({
			id: 'find-previous-conflict',
			name: 'Find previous conflict',
			editorCallback: (editor, view) => {
				const mdView = view instanceof MarkdownView ? view : undefined;
				this.markerNavigator.navigateBackward(editor, mdView);
			},
		});
		this.addCommand({
			id: 'pause-sync',
			name: 'Pause sync',
			callback: () => {
				void this.pauseSync();
			},
		});
		this.addCommand({
			id: 'resume-sync',
			name: 'Resume sync',
			callback: () => {
				void this.resumeSync();
			},
		});
		this.addCommand({
			id: 'run-full-scan',
			name: 'Run full scan',
			callback: () => {
				void this.startFullScan();
			},
		});
		this.addCommand({
			id: 'flush-pending-sync',
			name: 'Flush pending sync',
			callback: () => {
				void this.flushAndNotify();
			},
		});
	}

	private async startEngineAndWatchers(): Promise<void> {
		if (!this.wasmBridge || this.dormant) {
			return;
		}
		try {
			const status = await this.wasmBridge.start(this.settings);
			this.applyGlobalStatus(status);
			this.registerVaultWatchers();
		} catch (err: unknown) {
			const msg = err instanceof Error ? err.message : String(err);
			console.error('VaultShare Wasm startup failed:', err);
			new Notice(`VaultShare failed to start: ${msg}`, 0);
		}
	}

	private registerVaultWatchers(): void {
		this.registerEvent(
			this.app.vault.on('create', (file) => {
				this.forwardAbstractFileEvent('create', file);
			}),
		);
		this.registerEvent(
			this.app.vault.on('modify', (file) => {
				this.forwardAbstractFileEvent('modify', file);
			}),
		);
		this.registerEvent(
			this.app.vault.on('delete', (file) => {
				this.forwardAbstractFileEvent('delete', file);
			}),
		);
		this.registerEvent(
			this.app.vault.on('rename', (file, oldPath) => {
				this.forwardAbstractFileEvent('rename', file, oldPath);
			}),
		);
		this.registerEvent(
			this.app.workspace.on('file-open', (file) => {
				if (!file) {
					return;
				}
				this.forwardAbstractFileEvent('open', file);
			}),
		);
	}

	private forwardAbstractFileEvent(
		kind: VaultEventPayload['kind'],
		file: TAbstractFile,
		oldPath?: string,
	): void {
		if (!this.wasmBridge || this.dormant) {
			return;
		}
		const isFolder = file instanceof TFolder;
		const payload: VaultEventPayload = {
			kind,
			path: file.path,
			oldPath,
			isFolder,
		};
		if (file instanceof TFile) {
			payload.size = file.stat.size;
			payload.mtimeMs = file.stat.mtime;
		}
		void this.wasmBridge.onVaultEvent(payload).catch((err: unknown) => {
			console.error(`VaultShare failed to forward ${kind} for ${file.path}:`, err);
		});
	}

	private applyGlobalStatus(ev: GlobalStatusEvent): void {
		this.currentStatus = ev;
		this.statusIndicator?.update(ev);
	}

	private async handleOAuthCallback(params: Record<string, string | undefined>): Promise<void> {
		if (!this.wasmBridge || this.dormant) {
			return;
		}
		try {
			const refreshToken = this.oauth.handleAuthCallback(params);
			const status = await this.wasmBridge.setRefreshToken(refreshToken);
			this.applyGlobalStatus(status);
			new Notice('Connected to Google Drive.', 0);
		} catch (err: unknown) {
			const msg = err instanceof Error ? err.message : String(err);
			new Notice(`Google Drive login failed: ${msg}`, 0);
		}
	}
}
