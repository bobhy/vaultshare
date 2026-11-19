/**
 * Unit tests for Wasm boundary validators and GoWasmBridge RPC marshaling.
 *
 * @packageDocumentation
 */

import { describe, expect, it } from 'vitest';
import {
	parseConflictItems,
	parseFlushResult,
	parseGetConflictContentResult,
	parseGlobalStatusEvent,
	parseLogEntries,
	parseLogEntry,
	parseSyncStatusSnapshot,
} from './validators';

describe('Wasm JSON validators', () => {
	it('parses valid GlobalStatusEvent and FlushResult payloads', () => {
		expect(
			parseGlobalStatusEvent({ paused: false, needsLogin: true, conflictsTotal: 3 }),
		).toEqual({ paused: false, needsLogin: true, conflictsTotal: 3 });

		expect(parseFlushResult({ drained: true })).toEqual({ drained: true });
	});

	it('rejects malformed GlobalStatusEvent payloads', () => {
		expect(() => parseGlobalStatusEvent(null)).toThrow(/Expected object/);
		expect(() => parseGlobalStatusEvent({ paused: 'no' })).toThrow(/GlobalStatusEvent\.paused/);
	});

	it('parses SyncStatusSnapshot with all telemetry fields', () => {
		const snap = parseSyncStatusSnapshot({
			paused: false,
			needsLogin: false,
			fullScan: { active: true, scanned: 12, total: 50, filesPerSec: 8.5 },
			queued: { high: 1, bulk: 4 },
			inFlight: 2,
			highPriLatencyMs: { ewma: 15.2, last: 12.0, max: 40.0 },
			sessionResolutions: {
				merged: 1,
				keptNewer: 2,
				keptOlder: 0,
				keptLocal: 1,
				keptDrive: 0,
				pushed: 5,
				pulled: 3,
				trashed: 1,
				renamed: 2,
				manualFlagged: 1,
			},
			conflicts: { markers: 1, versions: 2, missing: 0, total: 3 },
		});
		expect(snap.fullScan.filesPerSec).toBe(8.5);
		expect(snap.conflicts.total).toBe(3);
	});

	it('normalizes null slices to empty arrays for logs and conflicts', () => {
		expect(parseLogEntries(null)).toEqual([]);
		expect(parseConflictItems(null)).toEqual([]);
	});

	it('validates LogEntry and GetConflictContentResult', () => {
		const entry = parseLogEntry({
			timestampMs: 1700000000000,
			severity: 'WARNING',
			message: 'Drive mismatch',
			detail: 'note.md',
		});
		expect(entry.severity).toBe('WARNING');

		const preview = parseGetConflictContentResult({
			isBinary: true,
			mimeType: 'image/png',
			size: 2048,
		});
		expect(preview.isBinary).toBe(true);
		expect(preview.text).toBeUndefined();
	});
});
