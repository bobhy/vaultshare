/**
 * Declarative settings tab and re-exported Go-backed settings types for VaultShare.
 *
 * @packageDocumentation
 */

import {
	type App,
	type ButtonComponent,
	type EventRef,
	Notice,
	PluginSettingTab,
	type SettingDefinitionItem,
} from 'obsidian';
import type VaultSharePlugin from './main';
import {
	DEFAULT_SETTINGS,
	type GlobalStatusEvent,
	type PluginSettings,
} from './settings.generated';

export { DEFAULT_SETTINGS, type PluginSettings };

/** Allowed conflict resolution strategies ordered after `merge` and before `manual`. */
export const MIDDLE_RESOLUTION_OPTIONS = [
	'keepNewest',
	'keepOldest',
	'keepLocal',
	'keepDrive',
] as const;

const RULE_LABELS: Record<string, string> = {
	merge: 'Auto three-way merge (text files)',
	keepNewest: 'Keep newest',
	keepOldest: 'Keep oldest',
	keepLocal: 'Keep local',
	keepDrive: 'Keep Drive',
	manual: 'Manual resolution (always last)',
};

const MAX_MIDDLE_RULES = 3;

/**
 * Normalizes a `syncResolution` rule list using the same invariants as Go's
 * `settings.NormalizeSyncResolution`: deduplicates, pins `merge` first if
 * present, caps middle strategies at 3, and pins `manual` last.
 */
export function normalizeSyncResolutionRules(rules: readonly string[]): string[] {
	let hasMerge = false;
	const middle: string[] = [];
	const seen = new Set<string>();

	for (const r of rules) {
		if (r === 'merge') {
			hasMerge = true;
		} else if (
			(MIDDLE_RESOLUTION_OPTIONS as readonly string[]).includes(r) &&
			!seen.has(r) &&
			middle.length < MAX_MIDDLE_RULES
		) {
			seen.add(r);
			middle.push(r);
		}
	}

	if (!hasMerge && middle.length === 0 && rules.length === 0) {
		return [...DEFAULT_SETTINGS.syncResolution];
	}
	const out: string[] = [];
	if (hasMerge) {
		out.push('merge');
	}
	out.push(...middle, 'manual');
	return out;
}

/**
 * Obsidian declarative settings tab exposing Google Drive account login,
 * trash safety checks, ordered sync resolution rules, and engine controls.
 */
export class VaultShareSettingTab extends PluginSettingTab {
	private readonly plugin: VaultSharePlugin;
	private statusRef: EventRef | null = null;

	/** Creates the settings tab bound to the active plugin instance. */
	constructor(app: App, plugin: VaultSharePlugin) {
		super(app, plugin);
		this.plugin = plugin;
	}

	/** Detaches any live global status listener when the settings tab closes. */
	hide(): void {
		const bridge = this.plugin.getBridge();
		if (bridge && this.statusRef) {
			bridge.offref(this.statusRef);
		}
		this.statusRef = null;
	}

	/**
	 * Returns declarative setting definitions for Obsidian 1.13.0+ (rendered
	 * automatically and indexed in global settings search).
	 */
	getSettingDefinitions(): SettingDefinitionItem<keyof PluginSettings>[] {
		return [
			{
				type: 'group',
				heading: 'Account',
				items: [
					{
						name: 'Google Drive account',
						desc: 'Only files created by VaultShare are visible under the drive.file scope.',
						render: (setting) => {
							const statusEl = setting.descEl.createDiv({
								cls: 'vaultshare-settings-note',
							});
							let authBtn: ButtonComponent | null = null;
							let needsLogin = this.plugin.getGlobalStatus().needsLogin;

							const syncAuthUI = (ev: GlobalStatusEvent): void => {
								needsLogin = ev.needsLogin;
								statusEl.setText(
									needsLogin
										? 'Status: Sign in required'
										: 'Status: Connected to Google Drive',
								);
								if (!authBtn) {
									return;
								}
								if (needsLogin) {
									authBtn.setButtonText('Log in').setCta();
								} else {
									authBtn.setButtonText('Log out').removeCta();
								}
							};

							setting.addButton((button) => {
								authBtn = button;
								button.onClick(async () => {
									if (needsLogin) {
										this.plugin.startGoogleLogin();
									} else {
										await this.plugin.logoutFromGoogle();
									}
								});
							});

							syncAuthUI(this.plugin.getGlobalStatus());

							const bridge = this.plugin.getBridge();
							if (bridge) {
								if (this.statusRef) {
									bridge.offref(this.statusRef);
								}
								this.statusRef = bridge.on('globalStatus', (payload: unknown) => {
									syncAuthUI(payload as GlobalStatusEvent);
								});
							}
						},
					},
					{
						name: 'Trash safety check',
						desc: 'Checks whether Obsidian deletes files to the local .trash folder.',
						render: (setting) => {
							const noteEl = setting.descEl.createDiv({
								cls: 'vaultshare-settings-note',
								text: 'Checking Obsidian trash preference...',
							});
							void this.inspectTrashOption(noteEl);
						},
					},
				],
			},
			{
				type: 'group',
				heading: 'Sync',
				items: [
					{
						name: 'Sync root folder',
						desc: 'Folder name under My Drive / VaultShare. Changing this resets the local sync baseline.',
						render: (setting) => {
							let draft = this.plugin.settings.syncRootFolder;
							setting.addText((text) =>
								text
									.setPlaceholder(this.app.vault.getName())
									.setValue(draft)
									.onChange((val) => {
										draft = val;
									}),
							);
							let armed = false;
							setting.addButton((button) =>
								button.setButtonText('Apply root').onClick(async () => {
									const next = draft.trim();
									if (next === this.plugin.settings.syncRootFolder) {
										new Notice('Sync root folder is unchanged.', 0);
										return;
									}
									if (!armed) {
										armed = true;
										button.setButtonText('Confirm reset');
										button.setDestructive();
										return;
									}
									armed = false;
									button.setButtonText('Apply root');
									this.plugin.settings.syncRootFolder = next;
									await this.plugin.saveSettings();
									new Notice('Updated sync root folder.', 0);
								}),
							);
						},
					},
					{
						name: 'Sync resolution order',
						desc: 'Ordered rules attempted when local and Drive versions diverge (up to 3 middle strategies).',
						render: (setting) => {
							const container = setting.settingEl.createDiv({
								cls: 'vaultshare-resolution-editor',
							});
							this.renderResolutionEditor(container);
						},
					},
					{
						name: 'Disable text merge',
						desc: 'Prevent automatic text merging and skip directly to the next conflict resolution rule.',
						control: {
							type: 'toggle',
							key: 'disableTextMerge',
						},
					},
					{
						name: 'Engine actions',
						desc: 'Control sync execution or clear local IndexedDB sync state.',
						render: (setting) => {
							setting.addButton((btn) =>
								btn.setButtonText('Pause').onClick(async () => {
									await this.plugin.pauseSync();
								}),
							);
							setting.addButton((btn) =>
								btn.setButtonText('Resume').onClick(async () => {
									await this.plugin.resumeSync();
								}),
							);
							setting.addButton((btn) =>
								btn.setButtonText('Scan now').onClick(async () => {
									await this.plugin.startFullScan();
								}),
							);
							let armedClear = false;
							setting.addButton((btn) =>
								btn
									.setButtonText('Clear local state')
									.setDestructive()
									.onClick(async () => {
										if (!armedClear) {
											armedClear = true;
											btn.setButtonText('Confirm clear');
											return;
										}
										armedClear = false;
										btn.setButtonText('Clear local state');
										await this.plugin.clearAllState();
									}),
							);
						},
					},
				],
			},
			{
				type: 'group',
				heading: 'Advanced',
				items: [
					{
						name: 'Local edit hold-down (ms)',
						desc: 'Trailing debounce delay before queuing local file modifications.',
						control: {
							type: 'number',
							key: 'localDebounceMs',
							min: 500,
							step: 500,
						},
					},
					{
						name: 'Local edit max wait (ms)',
						desc: 'Maximum wait before syncing continuous typing.',
						control: {
							type: 'number',
							key: 'localMaxWaitMs',
							min: 1000,
							step: 1000,
						},
					},
					{
						name: 'Drive active poll interval (ms)',
						desc: 'Polling interval for Google Drive changes while Obsidian is active.',
						control: {
							type: 'number',
							key: 'drivePollActiveMs',
							min: 5000,
							step: 1000,
						},
					},
					{
						name: 'Drive idle poll interval (ms)',
						desc: 'Polling interval for Google Drive changes when idle or recovering offline.',
						control: {
							type: 'number',
							key: 'drivePollIdleMs',
							min: 10000,
							step: 5000,
						},
					},
					{
						name: 'Full scan audit interval (minutes)',
						desc: 'Interval between periodic full-vault audit scans.',
						control: {
							type: 'number',
							key: 'fullScanIntervalMin',
							min: 5,
							step: 5,
						},
					},
					{
						name: 'Max concurrent Drive requests',
						desc: 'Maximum number of parallel HTTP requests to Google Drive (1 to 10).',
						control: {
							type: 'slider',
							key: 'maxPendingRequests',
							min: 1,
							max: 10,
							step: 1,
						},
					},
					{
						name: 'Log severity',
						desc: 'Minimum severity retained in the log buffer.',
						control: {
							type: 'dropdown',
							key: 'logMinSeverity',
							options: {
								DEBUG: 'Debug',
								INFO: 'Info',
								WARNING: 'Warning',
								ERROR: 'Error',
							},
						},
					},
					{
						name: 'Log buffer size',
						desc: 'Maximum number of recent log entries kept in memory.',
						control: {
							type: 'number',
							key: 'logHorizon',
							min: 50,
							max: 5000,
							step: 50,
						},
					},
				],
			},
		];
	}

	private async inspectTrashOption(noteEl: HTMLElement): Promise<void> {
		try {
			const raw = await this.app.vault.adapter.read(
				`${this.app.vault.configDir}/app.json`,
			);
			const parsed = JSON.parse(raw) as { trashOption?: unknown };
			if (parsed.trashOption === 'local') {
				noteEl.setText('Obsidian uses local .trash; delete detection is enabled.');
				noteEl.removeClass('vaultshare-warning');
				return;
			}
			noteEl.setText(
				'Warning: Obsidian is set to OS system trash or permanent deletion. Set Deleted files to Move to Obsidian trash for safer cross-device delete detection.',
			);
			noteEl.addClass('vaultshare-warning');
		} catch {
			noteEl.setText(
				'Warning: could not verify app.json trashOption (defaults to OS trash). Move to Obsidian trash is recommended.',
			);
			noteEl.addClass('vaultshare-warning');
		}
	}

	private renderResolutionEditor(container: HTMLElement): void {
		container.empty();
		const rules = normalizeSyncResolutionRules(this.plugin.settings.syncResolution);
		const hasMerge = rules[0] === 'merge';
		const middle = rules.filter((r) => r !== 'merge' && r !== 'manual');

		const saveRules = async (nextMerge: boolean, nextMiddle: string[]): Promise<void> => {
			const combined = [
				...(nextMerge ? ['merge'] : []),
				...nextMiddle,
				'manual',
			];
			this.plugin.settings.syncResolution = normalizeSyncResolutionRules(combined);
			await this.plugin.saveSettings();
			this.renderResolutionEditor(container);
		};

		const mergeRow = container.createDiv({ cls: 'vaultshare-res-row' });
		const mergeToggle = mergeRow.createEl('input', { attr: { type: 'checkbox' } });
		mergeToggle.checked = hasMerge;
		mergeToggle.addEventListener('change', () => {
			void saveRules(mergeToggle.checked, middle);
		});
		mergeRow.createSpan({ text: RULE_LABELS.merge! });

		middle.forEach((rule, idx) => {
			const row = container.createDiv({ cls: 'vaultshare-res-row' });
			row.createSpan({ text: `${idx + 1}. ${RULE_LABELS[rule] ?? rule}` });
			const actions = row.createDiv({ cls: 'vaultshare-toolbar' });

			if (idx > 0) {
				const upBtn = actions.createEl('button', { text: 'Up' });
				upBtn.addEventListener('click', () => {
					const copy = [...middle];
					[copy[idx - 1], copy[idx]] = [copy[idx]!, copy[idx - 1]!];
					void saveRules(hasMerge, copy);
				});
			}
			if (idx < middle.length - 1) {
				const downBtn = actions.createEl('button', { text: 'Down' });
				downBtn.addEventListener('click', () => {
					const copy = [...middle];
					[copy[idx], copy[idx + 1]] = [copy[idx + 1]!, copy[idx]!];
					void saveRules(hasMerge, copy);
				});
			}
			const rmBtn = actions.createEl('button', { text: 'Remove' });
			rmBtn.addEventListener('click', () => {
				const copy = middle.filter((_, i) => i !== idx);
				void saveRules(hasMerge, copy);
			});
		});

		const remaining = MIDDLE_RESOLUTION_OPTIONS.filter((opt) => !middle.includes(opt));
		if (middle.length < MAX_MIDDLE_RULES && remaining.length > 0) {
			const addRow = container.createDiv({ cls: 'vaultshare-res-row' });
			const select = addRow.createEl('select');
			for (const opt of remaining) {
				select.createEl('option', { value: opt, text: RULE_LABELS[opt] ?? opt });
			}
			const addBtn = addRow.createEl('button', { text: 'Add rule' });
			addBtn.addEventListener('click', () => {
				void saveRules(hasMerge, [...middle, select.value]);
			});
		}

		const manualRow = container.createDiv({ cls: 'vaultshare-res-row vaultshare-muted' });
		manualRow.createSpan({ text: RULE_LABELS.manual! });
	}
}
