package syncengine

import (
	"slices"
	"sync"
	"time"

	"vaultshare/go/settings"
)

const latencyEWMAAlpha = 0.2

func severityRank(sev string) int {
	switch sev {
	case settings.LogDebug:
		return 0
	case settings.LogInfo:
		return 1
	case settings.LogWarning:
		return 2
	case settings.LogError:
		return 3
	default:
		return 1
	}
}

// logRing retains bounded structured logs in memory and optionally streams new
// entries to EventSink while a LogView is subscribed.
type logRing struct {
	mu          sync.Mutex
	minSeverity string
	horizon     int
	stream      bool
	entries     []settings.LogEntry
	sink        EventSink
}

func newLogRing(minSev string, horizon int, sink EventSink) *logRing {
	return &logRing{
		minSeverity: minSev,
		horizon:     max(1, horizon),
		sink:        sink,
	}
}

func (l *logRing) Configure(minSev string, horizon int) {
	l.mu.Lock()
	l.minSeverity = minSev
	l.horizon = max(1, horizon)
	if len(l.entries) > l.horizon {
		l.entries = slices.Clone(l.entries[len(l.entries)-l.horizon:])
	}
	l.mu.Unlock()
}

func (l *logRing) SetStream(enabled bool) {
	l.mu.Lock()
	l.stream = enabled
	l.mu.Unlock()
}

func (l *logRing) Log(sev, msg, detail string, now time.Time) {
	l.mu.Lock()
	if severityRank(sev) < severityRank(l.minSeverity) {
		l.mu.Unlock()
		return
	}
	entry := settings.LogEntry{
		TimestampMs: now.UnixMilli(),
		Severity:    sev,
		Message:     msg,
		Detail:      detail,
	}
	l.entries = append(l.entries, entry)
	if len(l.entries) > l.horizon {
		l.entries = slices.Clone(l.entries[len(l.entries)-l.horizon:])
	}
	shouldStream := l.stream && l.sink != nil
	sink := l.sink
	l.mu.Unlock()

	if shouldStream {
		sink.EmitLog(entry)
	}
}

func (l *logRing) Snapshot(minSev string) []settings.LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	if minSev == "" {
		return slices.Clone(l.entries)
	}
	minRank := severityRank(minSev)
	out := make([]settings.LogEntry, 0, len(l.entries))
	for _, e := range l.entries {
		if severityRank(e.Severity) >= minRank {
			out = append(out, e)
		}
	}
	return out
}

// metricsTracker records High-priority latency (EWMA, last, max), Full Scan
// progress/throughput, and session-scoped resolution counts.
type metricsTracker struct {
	mu                 sync.Mutex
	latency            settings.LatencyStats
	hasLatencySample   bool
	fullScan           settings.FullScanStatus
	fullScanStartedAt  time.Time
	sessionResolutions settings.ResolutionCounters
}

func (m *metricsTracker) RecordHighPriLatency(d time.Duration) {
	ms := float64(d.Microseconds()) / 1000.0
	if ms < 0 {
		ms = 0
	}
	m.mu.Lock()
	m.latency.Last = ms
	if ms > m.latency.Max {
		m.latency.Max = ms
	}
	if !m.hasLatencySample {
		m.latency.EWMA = ms
		m.hasLatencySample = true
	} else {
		m.latency.EWMA = latencyEWMAAlpha*ms + (1.0-latencyEWMAAlpha)*m.latency.EWMA
	}
	m.mu.Unlock()
}

func (m *metricsTracker) StartFullScan(now time.Time) {
	m.mu.Lock()
	m.fullScan = settings.FullScanStatus{Active: true}
	m.fullScanStartedAt = now
	m.mu.Unlock()
}

func (m *metricsTracker) UpdateFullScan(scanned, total int, now time.Time) {
	m.mu.Lock()
	m.fullScan.Active = true
	m.fullScan.Scanned = scanned
	m.fullScan.Total = total
	elapsed := now.Sub(m.fullScanStartedAt).Seconds()
	if elapsed > 0.05 && scanned > 0 {
		m.fullScan.FilesPerSec = float64(scanned) / elapsed
	}
	m.mu.Unlock()
}

func (m *metricsTracker) FinishFullScan(now time.Time) {
	m.mu.Lock()
	m.fullScan.Active = false
	elapsed := now.Sub(m.fullScanStartedAt).Seconds()
	if elapsed > 0.05 && m.fullScan.Scanned > 0 {
		m.fullScan.FilesPerSec = float64(m.fullScan.Scanned) / elapsed
	}
	m.mu.Unlock()
}

func (m *metricsTracker) IncResolution(fn func(*settings.ResolutionCounters)) {
	m.mu.Lock()
	fn(&m.sessionResolutions)
	m.mu.Unlock()
}

func (m *metricsTracker) Snapshot() (settings.FullScanStatus, settings.LatencyStats, settings.ResolutionCounters) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fullScan, m.latency, m.sessionResolutions
}
