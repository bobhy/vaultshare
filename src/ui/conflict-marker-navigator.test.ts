/**
 * Unit tests for N-way conflict marker parsing, navigation, and resolution rules.
 *
 * @packageDocumentation
 */

import type { Editor } from 'obsidian';
import { describe, expect, it, vi } from 'vitest';
import {
	ConflictMarkerNavigator,
	parseConflictBlocks,
} from './conflict-marker-navigator';
import { formatLogMessage, severityRank } from './log-view';
import { normalizeSyncResolutionRules } from '../settings';

function makeMockEditor(initialLines: string[]): {
	editor: Editor;
	lines: string[];
	setCursorPos: (line: number) => void;
} {
	const lines = [...initialLines];
	let cursor = { line: 0, ch: 0 };

	const editor = {
		lineCount: () => lines.length,
		getLine: (n: number) => lines[n] ?? '',
		getCursor: () => cursor,
		setCursor: (pos: { line: number; ch: number }) => {
			cursor = pos;
		},
		scrollIntoView: vi.fn(),
		replaceRange: (
			text: string,
			from: { line: number; ch: number },
			to: { line: number; ch: number },
		) => {
			const before = (lines[from.line] ?? '').slice(0, from.ch);
			const after = (lines[to.line] ?? '').slice(to.ch);
			const inserted = `${before}${text}${after}`.split('\n');
			lines.splice(from.line, to.line - from.line + 1, ...inserted);
		},
	} as unknown as Editor;

	return {
		editor,
		lines,
		setCursorPos: (line: number) => {
			cursor = { line, ch: 0 };
		},
	};
}

describe('ConflictMarkerNavigator & parser', () => {
	it('parses both backtick-fenced and plain N-way conflict blocks', () => {
		const mdLines = [
			'intro',
			'```<<<',
			'```===== base',
			'original',
			'```===== A1',
			'local edit',
			'```===== A2',
			'drive edit',
			'```>>>',
			'between',
			'<<<<<<<',
			'======= A1',
			'plain local',
			'======= A2',
			'plain drive',
			'>>>>>>>',
		];
		const blocks = parseConflictBlocks(mdLines.length, (i) => mdLines[i] ?? '');
		expect(blocks).toHaveLength(2);
		expect(blocks[0]!.segments.map((s) => s.label)).toEqual(['base', 'A1', 'A2']);
		expect(blocks[1]!.segments.map((s) => s.label)).toEqual(['A1', 'A2']);
	});

	it('navigates forward/backward with wraparound and applies chosen resolution', () => {
		const mock = makeMockEditor([
			'top',
			'```<<<',
			'```===== A1',
			'first choice',
			'```===== A2',
			'second choice',
			'```>>>',
			'bottom',
		]);
		const nav = new ConflictMarkerNavigator();
		nav.navigateForward(mock.editor);
		expect(mock.editor.getCursor().line).toBe(1);

		const blocks = parseConflictBlocks(mock.editor.lineCount(), (n) =>
			mock.editor.getLine(n),
		);
		nav.applyResolution(mock.editor, blocks[0]!, blocks[0]!.segments[0]!.lines);
		expect(mock.lines).toEqual(['top', 'first choice', 'bottom']);
	});
});

describe('UI configuration helpers', () => {
	it('enforces Go NormalizeSyncResolution ordering invariants', () => {
		expect(normalizeSyncResolutionRules([])).toEqual(['merge', 'keepNewest', 'manual']);
		expect(
			normalizeSyncResolutionRules(['manual', 'keepLocal', 'merge', 'keepLocal', 'keepDrive', 'keepOldest', 'keepNewest']),
		).toEqual(['merge', 'keepLocal', 'keepDrive', 'keepOldest', 'manual']);
		expect(normalizeSyncResolutionRules(['keepDrive'])).toEqual(['keepDrive', 'manual']);
	});

	it('orders log severities monotonically', () => {
		expect(severityRank('DEBUG')).toBeLessThan(severityRank('INFO'));
		expect(severityRank('INFO')).toBeLessThan(severityRank('WARNING'));
		expect(severityRank('WARNING')).toBeLessThan(severityRank('ERROR'));
	});

	it('formats log entries on a single line separated by colon', () => {
		expect(
			formatLogMessage({
				timestampMs: 1,
				severity: 'INFO',
				message: 'Full Scan completed',
				detail: 'scanned=2',
			}),
		).toBe('Full Scan completed: scanned=2');
		expect(
			formatLogMessage({
				timestampMs: 2,
				severity: 'INFO',
				message: 'Pushed local edit to Drive',
				detail: 'Second note.md',
			}),
		).toBe('Pushed local edit to Drive: Second note.md');
		expect(
			formatLogMessage({
				timestampMs: 3,
				severity: 'WARNING',
				message: 'Drive error',
				detail: 'line 1\nline 2',
			}),
		).toBe('Drive error: line 1 line 2');
		expect(
			formatLogMessage({
				timestampMs: 4,
				severity: 'INFO',
				message: 'Conflict resolution paused',
			}),
		).toBe('Conflict resolution paused');
	});
});
