/**
 * Persistent global status indicator (desktop status bar item and mobile ribbon)
 * displaying engine state (`running`, `paused`, `needsLogin`) and unresolved conflict count.
 *
 * @packageDocumentation
 */

import { Platform, type Plugin, setIcon } from 'obsidian';
import type { GlobalStatusEvent } from '../settings.generated';

/** Navigation callbacks triggered when clicking the global status indicator parts. */
export interface GlobalIndicatorCallbacks {
	onOpenStatus: () => void;
	onOpenSettings: () => void;
	onOpenReconcile: () => void;
}

/**
 * Renders the two-part VaultShare global status indicator on desktop and mirrors
 * status + conflict notifications on mobile where the status bar is hidden.
 */
export class GlobalStatusIndicator {
	private readonly callbacks: GlobalIndicatorCallbacks;
	private statusBtnEl: HTMLElement | null = null;
	private conflictBadgeEl: HTMLElement | null = null;
	private mobileRibbonEl: HTMLElement | null = null;
	private currentState: GlobalStatusEvent = {
		paused: false,
		needsLogin: true,
		conflictsTotal: 0,
	};

	/** Mounts desktop status bar item or mobile ribbon icon on the given plugin. */
	constructor(plugin: Plugin, callbacks: GlobalIndicatorCallbacks) {
		this.callbacks = callbacks;

		if (Platform.isMobile) {
			this.mobileRibbonEl = plugin.addRibbonIcon(
				'cloud-off',
				'VaultShare status',
				() => {
					if (this.currentState.conflictsTotal > 0) {
						this.callbacks.onOpenReconcile();
					} else if (this.currentState.needsLogin) {
						this.callbacks.onOpenSettings();
					} else {
						this.callbacks.onOpenStatus();
					}
				},
			);
		} else {
			const root = plugin.addStatusBarItem();
			root.addClass('vaultshare-status-bar');

			this.statusBtnEl = root.createEl('button', {
				cls: 'vaultshare-status-btn clickable-icon',
				attr: { 'aria-label': 'VaultShare sync status' },
			});
			this.statusBtnEl.addEventListener('click', () => {
				if (this.currentState.needsLogin) {
					this.callbacks.onOpenSettings();
				} else {
					this.callbacks.onOpenStatus();
				}
			});

			this.conflictBadgeEl = root.createEl('button', {
				cls: 'vaultshare-conflict-pill',
				attr: { 'aria-label': 'Reconcile conflicts' },
			});
			this.conflictBadgeEl.addEventListener('click', () => {
				this.callbacks.onOpenReconcile();
			});
		}

		this.update(this.currentState);
	}

	/** Updates indicator icons, tooltips, and conflict pill visibility from `GlobalStatusEvent`. */
	update(ev: GlobalStatusEvent): void {
		this.currentState = ev;
		const { iconName, label, cls } = describeEngineStatus(ev);

		if (this.statusBtnEl) {
			this.statusBtnEl.empty();
			setIcon(this.statusBtnEl, iconName);
			this.statusBtnEl.setAttribute('aria-label', label);
			this.statusBtnEl.setAttribute('title', label);
			this.statusBtnEl.classList.remove('is-paused', 'is-needs-login', 'is-running');
			this.statusBtnEl.addClass(cls);
		}

		if (this.conflictBadgeEl) {
			if (ev.conflictsTotal > 0) {
				this.conflictBadgeEl.removeClass('is-hidden');
				this.conflictBadgeEl.setText(`⚠ ${ev.conflictsTotal}`);
				this.conflictBadgeEl.setAttribute(
					'title',
					`${ev.conflictsTotal} file conflict(s) require resolution`,
				);
			} else {
				this.conflictBadgeEl.addClass('is-hidden');
			}
		}

		if (this.mobileRibbonEl) {
			const ribbonLabel =
				ev.conflictsTotal > 0
					? `${label} — ${ev.conflictsTotal} conflict(s)`
					: label;
			setIcon(this.mobileRibbonEl, ev.conflictsTotal > 0 ? 'alert-triangle' : iconName);
			this.mobileRibbonEl.setAttribute('aria-label', ribbonLabel);
		}
	}
}

function describeEngineStatus(ev: GlobalStatusEvent): {
	iconName: string;
	label: string;
	cls: string;
} {
	if (ev.needsLogin) {
		return {
			iconName: 'log-in',
			label: 'VaultShare: sign in required',
			cls: 'is-needs-login',
		};
	}
	if (ev.paused) {
		return {
			iconName: 'pause-circle',
			label: 'VaultShare: paused',
			cls: 'is-paused',
		};
	}
	return {
		iconName: 'refresh-cw',
		label: 'VaultShare: syncing',
		cls: 'is-running',
	};
}
