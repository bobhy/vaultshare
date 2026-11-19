/**
 * CodeMirror 6 read-only compartment lock and visual synchronizing overlay used
 * when Go Wasm writes to an actively open Obsidian note.
 *
 * @packageDocumentation
 */

import { type App, TextFileView } from 'obsidian';
import { Compartment, type Extension } from '@codemirror/state';
import { EditorView } from '@codemirror/view';

const lockCompartment = new Compartment();

/**
 * CM6 editor extension that must be registered once during `Plugin.onload`
 * so open leaves can be toggled read-only during Go Wasm file writes.
 */
export const openFileLockExtension: Extension = lockCompartment.of(
	EditorView.editable.of(true),
);

interface LockedLeafHandle {
	cm?: EditorView;
	overlayEl: HTMLElement;
	view: TextFileView;
}

/**
 * Acquires an editor read-only lock, visual overlay, and flush-save across all
 * split-pane leaves displaying `path` before executing a Go Wasm write callback.
 */
export class OpenFileLocker {
	private readonly app: App;

	/** Creates an `OpenFileLocker` bound to the Obsidian workspace. */
	constructor(app: App) {
		this.app = app;
	}

	/**
	 * Locks every open leaf matching `path` (if any), flushes Obsidian's 2 s save
	 * buffer before `fn()` executes, and restores editability in `finally`.
	 */
	async withLock(path: string, fn: () => Promise<void>): Promise<void> {
		const handles: LockedLeafHandle[] = [];
		this.app.workspace.iterateAllLeaves((leaf) => {
			const view = leaf.view;
			if (view instanceof TextFileView && view.file?.path === path) {
				const cm = getEditorViewFromTextFileView(view);
				if (cm) {
					cm.dispatch({
						effects: lockCompartment.reconfigure(EditorView.editable.of(false)),
					});
				}
				const overlayEl = attachSyncOverlay(view.containerEl);
				handles.push({ cm, overlayEl, view });
			}
		});

		if (handles.length === 0) {
			await fn();
			return;
		}

		try {
			for (const handle of handles) {
				await handle.view.save();
			}
			await fn();
		} finally {
			for (const handle of handles) {
				handle.overlayEl.remove();
				handle.cm?.dispatch({
					effects: lockCompartment.reconfigure(EditorView.editable.of(true)),
				});
			}
		}
	}
}

function getEditorViewFromTextFileView(view: TextFileView): EditorView | undefined {
	const maybeWithEditor = view as unknown as {
		editor?: { cm?: EditorView };
	};
	return maybeWithEditor.editor?.cm;
}

function attachSyncOverlay(containerEl: HTMLElement): HTMLElement {
	const overlay = containerEl.createDiv({ cls: 'vaultshare-lock-overlay' });
	overlay.createDiv({ cls: 'vaultshare-lock-spinner' });
	overlay.createSpan({ cls: 'vaultshare-lock-label', text: 'Synchronizing...' });
	return overlay;
}
