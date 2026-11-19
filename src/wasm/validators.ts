/**
 * Runtime boundary validators narrowing unknown JSON payloads returned by Go Wasm
 * without unsafe `any` casts.
 *
 * @packageDocumentation
 */

import type {
	ConflictCandidate,
	ConflictCounts,
	ConflictItem,
	FlushResult,
	FullScanStatus,
	GetConflictContentResult,
	GlobalStatusEvent,
	LatencyStats,
	LogEntry,
	QueueCounts,
	ResolutionCounters,
	SyncStatusSnapshot,
} from '../settings.generated';

function asRecord(value: unknown, label: string): Record<string, unknown> {
	if (typeof value !== 'object' || value === null || Array.isArray(value)) {
		throw new Error(`Expected object for ${label}`);
	}
	return value as Record<string, unknown>;
}

function expectBool(rec: Record<string, unknown>, key: string, label: string): boolean {
	const v = rec[key];
	if (typeof v !== 'boolean') {
		throw new Error(`Expected boolean ${label}.${key}`);
	}
	return v;
}

function expectNumber(rec: Record<string, unknown>, key: string, label: string): number {
	const v = rec[key];
	if (typeof v !== 'number' || Number.isNaN(v)) {
		throw new Error(`Expected number ${label}.${key}`);
	}
	return v;
}

function expectString(rec: Record<string, unknown>, key: string, label: string): string {
	const v = rec[key];
	if (typeof v !== 'string') {
		throw new Error(`Expected string ${label}.${key}`);
	}
	return v;
}

function optionalString(rec: Record<string, unknown>, key: string, label: string): string | undefined {
	const v = rec[key];
	if (v === undefined) {
		return undefined;
	}
	if (typeof v !== 'string') {
		throw new Error(`Expected optional string ${label}.${key}`);
	}
	return v;
}

function optionalBool(rec: Record<string, unknown>, key: string, label: string): boolean | undefined {
	const v = rec[key];
	if (v === undefined) {
		return undefined;
	}
	if (typeof v !== 'boolean') {
		throw new Error(`Expected optional boolean ${label}.${key}`);
	}
	return v;
}

/** Validates and returns a `GlobalStatusEvent`. */
export function parseGlobalStatusEvent(value: unknown): GlobalStatusEvent {
	const rec = asRecord(value, 'GlobalStatusEvent');
	return {
		paused: expectBool(rec, 'paused', 'GlobalStatusEvent'),
		needsLogin: expectBool(rec, 'needsLogin', 'GlobalStatusEvent'),
		conflictsTotal: expectNumber(rec, 'conflictsTotal', 'GlobalStatusEvent'),
	};
}

/** Validates and returns a `FlushResult`. */
export function parseFlushResult(value: unknown): FlushResult {
	const rec = asRecord(value, 'FlushResult');
	return {
		drained: expectBool(rec, 'drained', 'FlushResult'),
	};
}

function parseFullScanStatus(value: unknown): FullScanStatus {
	const rec = asRecord(value, 'FullScanStatus');
	return {
		active: expectBool(rec, 'active', 'FullScanStatus'),
		scanned: expectNumber(rec, 'scanned', 'FullScanStatus'),
		total: expectNumber(rec, 'total', 'FullScanStatus'),
		filesPerSec: expectNumber(rec, 'filesPerSec', 'FullScanStatus'),
	};
}

function parseQueueCounts(value: unknown): QueueCounts {
	const rec = asRecord(value, 'QueueCounts');
	return {
		high: expectNumber(rec, 'high', 'QueueCounts'),
		bulk: expectNumber(rec, 'bulk', 'QueueCounts'),
	};
}

function parseLatencyStats(value: unknown): LatencyStats {
	const rec = asRecord(value, 'LatencyStats');
	return {
		ewma: expectNumber(rec, 'ewma', 'LatencyStats'),
		last: expectNumber(rec, 'last', 'LatencyStats'),
		max: expectNumber(rec, 'max', 'LatencyStats'),
	};
}

function parseResolutionCounters(value: unknown): ResolutionCounters {
	const rec = asRecord(value, 'ResolutionCounters');
	return {
		merged: expectNumber(rec, 'merged', 'ResolutionCounters'),
		keptNewer: expectNumber(rec, 'keptNewer', 'ResolutionCounters'),
		keptOlder: expectNumber(rec, 'keptOlder', 'ResolutionCounters'),
		keptLocal: expectNumber(rec, 'keptLocal', 'ResolutionCounters'),
		keptDrive: expectNumber(rec, 'keptDrive', 'ResolutionCounters'),
		pushed: expectNumber(rec, 'pushed', 'ResolutionCounters'),
		pulled: expectNumber(rec, 'pulled', 'ResolutionCounters'),
		trashed: expectNumber(rec, 'trashed', 'ResolutionCounters'),
		renamed: expectNumber(rec, 'renamed', 'ResolutionCounters'),
		manualFlagged: expectNumber(rec, 'manualFlagged', 'ResolutionCounters'),
	};
}

function parseConflictCounts(value: unknown): ConflictCounts {
	const rec = asRecord(value, 'ConflictCounts');
	return {
		markers: expectNumber(rec, 'markers', 'ConflictCounts'),
		versions: expectNumber(rec, 'versions', 'ConflictCounts'),
		missing: expectNumber(rec, 'missing', 'ConflictCounts'),
		total: expectNumber(rec, 'total', 'ConflictCounts'),
	};
}

/** Validates and returns a `SyncStatusSnapshot`. */
export function parseSyncStatusSnapshot(value: unknown): SyncStatusSnapshot {
	const rec = asRecord(value, 'SyncStatusSnapshot');
	return {
		paused: expectBool(rec, 'paused', 'SyncStatusSnapshot'),
		needsLogin: expectBool(rec, 'needsLogin', 'SyncStatusSnapshot'),
		fullScan: parseFullScanStatus(rec.fullScan),
		queued: parseQueueCounts(rec.queued),
		inFlight: expectNumber(rec, 'inFlight', 'SyncStatusSnapshot'),
		highPriLatencyMs: parseLatencyStats(rec.highPriLatencyMs),
		sessionResolutions: parseResolutionCounters(rec.sessionResolutions),
		conflicts: parseConflictCounts(rec.conflicts),
	};
}

/** Validates and returns a single `LogEntry`. */
export function parseLogEntry(value: unknown): LogEntry {
	const rec = asRecord(value, 'LogEntry');
	return {
		timestampMs: expectNumber(rec, 'timestampMs', 'LogEntry'),
		severity: expectString(rec, 'severity', 'LogEntry'),
		message: expectString(rec, 'message', 'LogEntry'),
		detail: optionalString(rec, 'detail', 'LogEntry'),
	};
}

/** Validates and returns an array of `LogEntry` items (normalizing null to `[]`). */
export function parseLogEntries(value: unknown): LogEntry[] {
	if (value === null) {
		return [];
	}
	if (!Array.isArray(value)) {
		throw new Error('Expected array for LogEntry[]');
	}
	return value.map((item) => parseLogEntry(item));
}

function parseConflictCandidate(value: unknown): ConflictCandidate {
	const rec = asRecord(value, 'ConflictCandidate');
	return {
		source: expectString(rec, 'source', 'ConflictCandidate'),
		fileId: optionalString(rec, 'fileId', 'ConflictCandidate'),
		name: expectString(rec, 'name', 'ConflictCandidate'),
		mimeType: expectString(rec, 'mimeType', 'ConflictCandidate'),
		modifiedTime: expectString(rec, 'modifiedTime', 'ConflictCandidate'),
		size: expectNumber(rec, 'size', 'ConflictCandidate'),
		trashed: optionalBool(rec, 'trashed', 'ConflictCandidate'),
		md5: optionalString(rec, 'md5', 'ConflictCandidate'),
	};
}

/** Validates and returns an array of `ConflictItem` entries (normalizing null to `[]`). */
export function parseConflictItems(value: unknown): ConflictItem[] {
	if (value === null) {
		return [];
	}
	if (!Array.isArray(value)) {
		throw new Error('Expected array for ConflictItem[]');
	}
	return value.map((item) => {
		const rec = asRecord(item, 'ConflictItem');
		if (!Array.isArray(rec.candidates)) {
			throw new Error('Expected array ConflictItem.candidates');
		}
		return {
			path: expectString(rec, 'path', 'ConflictItem'),
			kind: expectString(rec, 'kind', 'ConflictItem'),
			summary: expectString(rec, 'summary', 'ConflictItem'),
			detectedAtMs: expectNumber(rec, 'detectedAtMs', 'ConflictItem'),
			candidates: rec.candidates.map((c) => parseConflictCandidate(c)),
		};
	});
}

/** Validates and returns a `GetConflictContentResult`. */
export function parseGetConflictContentResult(value: unknown): GetConflictContentResult {
	const rec = asRecord(value, 'GetConflictContentResult');
	return {
		text: optionalString(rec, 'text', 'GetConflictContentResult'),
		isBinary: expectBool(rec, 'isBinary', 'GetConflictContentResult'),
		mimeType: expectString(rec, 'mimeType', 'GetConflictContentResult'),
		size: expectNumber(rec, 'size', 'GetConflictContentResult'),
	};
}
