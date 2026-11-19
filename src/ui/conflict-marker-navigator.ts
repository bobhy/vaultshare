/**
 * Editor-side support for VaultShare N-way conflict markers (both Markdown
 * backtick-fenced markers and plain Git-style markers).
 *
 * @packageDocumentation
 */

import { type Editor, type MarkdownView, Notice, Platform, setIcon } from 'obsidian';
import { type Extension, type Range, StateEffect, StateField } from '@codemirror/state';
import { Decoration, type DecorationSet, EditorView, type Panel, showPanel } from '@codemirror/view';

/** Opening conflict marker for `.md` files (backtick-fenced so Obsidian hides/fences it cleanly). */
export const MARKER_OPEN_BACKTICK = '```<<<';
/** Closing conflict marker for `.md` files. */
export const MARKER_CLOSE_BACKTICK = '```>>>';
/** Segment header pattern for backtick-fenced conflict markers (` ```===== <label> `). */
export const SEGMENT_BACKTICK_RE = /^```===== (.+)$/;

/** Opening conflict marker for plain-text non-Markdown files. */
export const MARKER_OPEN_PLAIN = '<<<<<<<';
/** Closing conflict marker for plain-text non-Markdown files. */
export const MARKER_CLOSE_PLAIN = '>>>>>>>';
/** Segment header pattern for plain conflict markers (`======= <label>`). */
export const SEGMENT_PLAIN_RE = /^======= (.+)$/;

/** One labelled segment of a conflict region: `base` or an alternative `A<n>`. */
export interface ConflictSegment {
	label: string;
	lines: string[];
}

/** A parsed conflict region spanning `startLine..endLine` inclusive. */
export interface ConflictBlock {
	startLine: number;
	endLine: number;
	segments: ConflictSegment[];
}

/** Callbacks the resolution panel invokes when its buttons are activated. */
export interface ConflictBannerCallbacks {
	onResolve(lines: string[]): void;
	onNavigate(direction: 'forward' | 'backward'): void;
	onClose(): void;
}

const setConflictHighlight = StateEffect.define<Range<Decoration>[] | null>();

const conflictHighlightField = StateField.define<DecorationSet>({
	create: () => Decoration.none,
	update(deco, tr) {
		deco = deco.map(tr.changes);
		for (const effect of tr.effects) {
			if (effect.is(setConflictHighlight)) {
				deco = effect.value === null ? Decoration.none : Decoration.set(effect.value, true);
			}
		}
		return deco;
	},
	provide: (f) => EditorView.decorations.from(f),
});

const setConflictPanel = StateEffect.define<((view: EditorView) => Panel) | null>();

const conflictPanelField = StateField.define<((view: EditorView) => Panel) | null>({
	create: () => null,
	update(value, tr) {
		for (const effect of tr.effects) {
			if (effect.is(setConflictPanel)) {
				return effect.value;
			}
		}
		return value;
	},
	provide: (f) => showPanel.from(f),
});

/**
 * CodeMirror 6 extension bundle enabling rounded-rectangle conflict highlights
 * and the floating CM6 conflict resolution panel.
 */
export const conflictHighlightExtension: Extension = [
	conflictHighlightField,
	conflictPanelField,
];

/**
 * Parses all N-way conflict blocks (supporting both backtick-fenced and plain
 * markers) in document order.
 */
export function parseConflictBlocks(
	lineCount: number,
	getLine: (index: number) => string,
): ConflictBlock[] {
	const blocks: ConflictBlock[] = [];
	let i = 0;
	while (i < lineCount) {
		const line = getLine(i);
		let closeMarker: string | null = null;
		let segRegex: RegExp | null = null;
		if (line === MARKER_OPEN_BACKTICK) {
			closeMarker = MARKER_CLOSE_BACKTICK;
			segRegex = SEGMENT_BACKTICK_RE;
		} else if (line === MARKER_OPEN_PLAIN) {
			closeMarker = MARKER_CLOSE_PLAIN;
			segRegex = SEGMENT_PLAIN_RE;
		}
		if (!closeMarker || !segRegex) {
			i++;
			continue;
		}

		const startLine = i;
		const segments: ConflictSegment[] = [];
		let current: ConflictSegment | null = null;
		let endLine = -1;

		for (let j = i + 1; j < lineCount; j++) {
			const inner = getLine(j);
			if (inner === closeMarker) {
				if (current) {
					segments.push(current);
				}
				endLine = j;
				break;
			}
			if (inner === MARKER_OPEN_BACKTICK || inner === MARKER_OPEN_PLAIN) {
				break;
			}
			const match = segRegex.exec(inner);
			if (match?.[1]) {
				if (current) {
					segments.push(current);
				}
				current = { label: match[1], lines: [] };
			} else if (current) {
				current.lines.push(inner);
			}
		}

		if (endLine !== -1 && segments.length >= 2) {
			blocks.push({ startLine, endLine, segments });
			i = endLine + 1;
		} else {
			i++;
		}
	}
	return blocks;
}

/**
 * Navigates between N-way conflict blocks inside an Obsidian Markdown/text editor
 * and displays the inline CM6 resolution panel.
 */
export class ConflictMarkerNavigator {
	private activeEditorView: EditorView | null = null;
	private readonly onAllResolved?: (filePath: string) => void;

	/** Creates a navigator with an optional hook invoked when all blocks in a file are resolved. */
	constructor(onAllResolved?: (filePath: string) => void) {
		this.onAllResolved = onAllResolved;
	}

	/** Jumps to the next conflict block after the cursor (wrapping at EOF). */
	navigateForward(editor: Editor, view?: MarkdownView): void {
		this.navigate(editor, 'forward', view);
	}

	/** Jumps to the previous conflict block before the cursor (wrapping at BOF). */
	navigateBackward(editor: Editor, view?: MarkdownView): void {
		this.navigate(editor, 'backward', view);
	}

	/** Replaces `block` lines with the user-chosen `replacement` lines. */
	applyResolution(editor: Editor, block: ConflictBlock, replacement: string[]): void {
		const from = { line: block.startLine, ch: 0 };
		const to = { line: block.endLine, ch: editor.getLine(block.endLine).length };
		editor.replaceRange(replacement.join('\n'), from, to);
		editor.setCursor({ line: block.startLine, ch: 0 });
	}

	private navigate(editor: Editor, direction: 'forward' | 'backward', view?: MarkdownView): void {
		const blocks = parseConflictBlocks(editor.lineCount(), (n) => editor.getLine(n));
		if (blocks.length === 0) {
			this.dismissBanner();
			new Notice('No conflicts found.', 0);
			return;
		}

		const cursorLine = editor.getCursor().line;
		const targetIndex = findTargetBlockIndex(blocks, cursorLine, direction);
		const target = blocks[targetIndex]!;
		const alreadyInside =
			blocks.length === 1 && cursorLine >= target.startLine && cursorLine <= target.endLine;

		if (alreadyInside) {
			new Notice('No other conflicts.', 0);
		} else {
			editor.setCursor({ line: target.startLine, ch: 0 });
			editor.scrollIntoView(
				{ from: { line: target.startLine, ch: 0 }, to: { line: target.endLine, ch: 0 } },
				true,
			);
		}

		if (view) {
			this.showBanner(editor, target, view, targetIndex, blocks.length);
		}
		this.setHighlight(editor, target);
	}

	private showBanner(
		editor: Editor,
		block: ConflictBlock,
		view: MarkdownView,
		index: number,
		total: number,
	): void {
		const cm = cmOf(editor);
		if (!cm) {
			return;
		}
		this.dismissBanner();
		this.activeEditorView = cm;

		const isMobile = Platform.isMobile || Platform.isIosApp;
		cm.dispatch({
			effects: setConflictPanel.of(() =>
				buildConflictBanner(block, index, total, isMobile, {
					onResolve: (lines) => {
						this.applyResolution(editor, block, lines);
						const remaining = parseConflictBlocks(editor.lineCount(), (n) =>
							editor.getLine(n),
						);
						if (remaining.length === 0) {
							this.dismissBanner();
							new Notice('All conflicts resolved.', 0);
							const filePath = view.file?.path;
							if (filePath && this.onAllResolved) {
								this.onAllResolved(filePath);
							}
						} else {
							this.navigate(editor, 'forward', view);
						}
					},
					onNavigate: (dir) => {
						this.navigate(editor, dir, view);
					},
					onClose: () => {
						this.dismissBanner();
					},
				}),
			),
		});
	}

	private setHighlight(editor: Editor, block: ConflictBlock): void {
		const cm = cmOf(editor);
		if (!cm) {
			return;
		}
		const doc = cm.state.doc;
		const decorations: Range<Decoration>[] = [];
		for (let lineNo = block.startLine + 1; lineNo <= block.endLine + 1; lineNo++) {
			if (lineNo > doc.lines) {
				break;
			}
			const cls = ['vaultshare-conflict-line'];
			if (lineNo === block.startLine + 1) {
				cls.push('vaultshare-conflict-line-first');
			}
			if (lineNo === block.endLine + 1) {
				cls.push('vaultshare-conflict-line-last');
			}
			decorations.push(
				Decoration.line({ attributes: { class: cls.join(' ') } }).range(
					doc.line(lineNo).from,
				),
			);
		}
		cm.dispatch({ effects: setConflictHighlight.of(decorations) });
	}

	private dismissBanner(): void {
		if (this.activeEditorView) {
			this.activeEditorView.dispatch({
				effects: [setConflictPanel.of(null), setConflictHighlight.of(null)],
			});
			this.activeEditorView = null;
		}
	}
}

function findTargetBlockIndex(
	blocks: ConflictBlock[],
	cursorLine: number,
	direction: 'forward' | 'backward',
): number {
	if (direction === 'forward') {
		const idx = blocks.findIndex((b) => b.startLine > cursorLine);
		return idx === -1 ? 0 : idx;
	}
	for (let i = blocks.length - 1; i >= 0; i--) {
		if (blocks[i]!.startLine < cursorLine) {
			return i;
		}
	}
	return blocks.length - 1;
}

function cmOf(editor: Editor): EditorView | undefined {
	return (editor as unknown as { cm?: EditorView }).cm;
}

function bindActivate(el: HTMLElement, handler: () => void): void {
	el.addEventListener('click', handler);
	el.addEventListener('touchend', (ev) => {
		ev.preventDefault();
		handler();
	});
}

/**
 * Pure builder for the conflict marker resolution panel DOM and mobile/desktop
 * docking side (`top: !isMobile`).
 */
export function buildConflictBanner(
	block: ConflictBlock,
	index: number,
	total: number,
	isMobile: boolean,
	callbacks: ConflictBannerCallbacks,
): { dom: HTMLElement; top: boolean } {
	const dom = createDiv({ cls: 'vaultshare-conflict-banner' });
	dom.createSpan({
		cls: 'vaultshare-conflict-banner-label',
		text: `Conflict ${index + 1} of ${total}`,
	});

	for (const seg of block.segments) {
		const btn = dom.createEl('button', {
			cls: 'vaultshare-conflict-banner-btn',
			text: seg.label,
		});
		bindActivate(btn, () => {
			callbacks.onResolve(seg.lines);
		});
	}

	dom.createSpan({ cls: 'vaultshare-conflict-banner-sep' });

	const addNavBtn = (iconName: string, label: string, dir: 'forward' | 'backward'): void => {
		const btn = dom.createEl('button', {
			cls: 'vaultshare-conflict-banner-nav',
			attr: { 'aria-label': label },
		});
		setIcon(btn, iconName);
		bindActivate(btn, () => {
			callbacks.onNavigate(dir);
		});
	};
	addNavBtn('chevron-up', 'Previous conflict', 'backward');
	addNavBtn('chevron-down', 'Next conflict', 'forward');

	const closeBtn = dom.createEl('button', {
		cls: 'vaultshare-conflict-banner-close',
		text: '×',
		attr: { 'aria-label': 'Close' },
	});
	bindActivate(closeBtn, () => {
		callbacks.onClose();
	});

	return { dom, top: !isMobile };
}
