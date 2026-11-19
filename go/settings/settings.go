// Package settings defines VaultShare V2's canonical plugin configuration,
// normalization rules, and Go↔TypeScript RPC/UI data-transfer types.
//
// Structs defined here serve as the single source of truth for codegen in
// go/cmd/gents, which emits matching TypeScript interfaces and defaults into
// src/settings.generated.ts.
package settings

import (
	"slices"
	"strings"
)

// Conflict resolution strategy identifiers stored in PluginSettings.SyncResolution.
const (
	ResolutionMerge      = "merge"
	ResolutionKeepNewest = "keepNewest"
	ResolutionKeepOldest = "keepOldest"
	ResolutionKeepLocal  = "keepLocal"
	ResolutionKeepDrive  = "keepDrive"
	ResolutionManual     = "manual"
)

// Log severity levels ordered from most verbose to most critical.
const (
	LogDebug   = "DEBUG"
	LogInfo    = "INFO"
	LogWarning = "WARNING"
	LogError   = "ERROR"
)

// Concurrency and log buffer bounds.
const (
	MinPendingRequests      = 1
	MaxPendingRequestsLimit = 10
	MinLogHorizon           = 50
	MaxLogHorizon           = 5000
	MaxMiddleResolutions    = 2
)

// PluginSettings holds all user-configurable VaultShare settings shared between
// the Obsidian TypeScript UI and the Go sync engine.
type PluginSettings struct {
	SyncRootFolder      string   `json:"syncRootFolder"`
	SyncResolution      []string `json:"syncResolution"`
	DisableTextMerge    bool     `json:"disableTextMerge"`
	LocalDebounceMs     int      `json:"localDebounceMs"`
	LocalMaxWaitMs      int      `json:"localMaxWaitMs"`
	DrivePollActiveMs   int      `json:"drivePollActiveMs"`
	DrivePollIdleMs     int      `json:"drivePollIdleMs"`
	FullScanIntervalMin int      `json:"fullScanIntervalMin"`
	MaxPendingRequests  int      `json:"maxPendingRequests"`
	LogMinSeverity      string   `json:"logMinSeverity"`
	LogHorizon          int      `json:"logHorizon"`
}

// DefaultSettings returns the canonical default settings for VaultShare V2.
func DefaultSettings() PluginSettings {
	return PluginSettings{
		SyncRootFolder:      "",
		SyncResolution:      []string{ResolutionMerge, ResolutionKeepNewest, ResolutionManual},
		DisableTextMerge:    false,
		LocalDebounceMs:     10000,
		LocalMaxWaitMs:      60000,
		DrivePollActiveMs:   30000,
		DrivePollIdleMs:     600000,
		FullScanIntervalMin: 1440,
		MaxPendingRequests:  3,
		LogMinSeverity:      LogInfo,
		LogHorizon:          500,
	}
}

// NormalizeSettings applies defaults, clamps numeric bounds, and enforces
// SyncResolution ordering invariants (Merge first if present, Manual always last).
func NormalizeSettings(cfg PluginSettings) PluginSettings {
	def := DefaultSettings()
	cfg.SyncRootFolder = strings.TrimSpace(cfg.SyncRootFolder)
	cfg.SyncResolution = NormalizeSyncResolution(cfg.SyncResolution)

	if cfg.LocalDebounceMs <= 0 {
		cfg.LocalDebounceMs = def.LocalDebounceMs
	}
	if cfg.LocalMaxWaitMs <= 0 {
		cfg.LocalMaxWaitMs = def.LocalMaxWaitMs
	}
	if cfg.LocalMaxWaitMs < cfg.LocalDebounceMs {
		cfg.LocalMaxWaitMs = cfg.LocalDebounceMs
	}
	if cfg.DrivePollActiveMs <= 0 {
		cfg.DrivePollActiveMs = def.DrivePollActiveMs
	}
	if cfg.DrivePollIdleMs <= 0 {
		cfg.DrivePollIdleMs = def.DrivePollIdleMs
	}
	if cfg.DrivePollIdleMs < cfg.DrivePollActiveMs {
		cfg.DrivePollIdleMs = cfg.DrivePollActiveMs
	}
	if cfg.FullScanIntervalMin <= 0 {
		cfg.FullScanIntervalMin = def.FullScanIntervalMin
	}

	if cfg.MaxPendingRequests <= 0 {
		cfg.MaxPendingRequests = def.MaxPendingRequests
	} else if cfg.MaxPendingRequests > MaxPendingRequestsLimit {
		cfg.MaxPendingRequests = MaxPendingRequestsLimit
	}

	switch cfg.LogMinSeverity {
	case LogDebug, LogInfo, LogWarning, LogError:
	default:
		cfg.LogMinSeverity = def.LogMinSeverity
	}

	if cfg.LogHorizon <= 0 {
		cfg.LogHorizon = def.LogHorizon
	} else if cfg.LogHorizon < MinLogHorizon {
		cfg.LogHorizon = MinLogHorizon
	} else if cfg.LogHorizon > MaxLogHorizon {
		cfg.LogHorizon = MaxLogHorizon
	}

	return cfg
}

// NormalizeSyncResolution validates and canonicalizes a SyncResolution list:
// removes unknown/duplicate items, pins ResolutionMerge first if present, keeps
// at most MaxMiddleResolutions middle strategies, and always appends ResolutionManual.
func NormalizeSyncResolution(rules []string) []string {
	hasMerge := false
	middle := make([]string, 0, MaxMiddleResolutions)
	seen := make(map[string]bool)

	for _, r := range rules {
		switch r {
		case ResolutionMerge:
			hasMerge = true
		case ResolutionKeepNewest, ResolutionKeepOldest, ResolutionKeepLocal, ResolutionKeepDrive:
			if !seen[r] && len(middle) < MaxMiddleResolutions {
				seen[r] = true
				middle = append(middle, r)
			}
		case ResolutionManual:
			// Always pinned last below.
		}
	}

	if !hasMerge && len(middle) == 0 && len(rules) == 0 {
		return slices.Clone(DefaultSettings().SyncResolution)
	}

	out := make([]string, 0, 2+len(middle))
	if hasMerge {
		out = append(out, ResolutionMerge)
	}
	out = append(out, middle...)
	out = append(out, ResolutionManual)
	return out
}

// SanitizeDriveFolderName returns a Drive-safe folder name for syncRootFolder,
// falling back to "Vault" when empty.
func SanitizeDriveFolderName(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '-'
		default:
			if r < 32 {
				return -1
			}
			return r
		}
	}, strings.TrimSpace(name))
	cleaned = strings.Trim(cleaned, ". ")
	if cleaned == "" {
		return "Vault"
	}
	return cleaned
}

// ResolveSyncRootFolder returns the effective Drive sync root folder name,
// using the sanitized vault name when SyncRootFolder is empty.
func ResolveSyncRootFolder(vaultName, configured string) string {
	if trimmed := strings.TrimSpace(configured); trimmed != "" {
		return SanitizeDriveFolderName(trimmed)
	}
	return SanitizeDriveFolderName(vaultName)
}

// GlobalStatusEvent drives the compact status bar / mobile ribbon indicator.
type GlobalStatusEvent struct {
	Paused         bool `json:"paused"`
	NeedsLogin     bool `json:"needsLogin"`
	ConflictsTotal int  `json:"conflictsTotal"`
}

// FullScanStatus summarizes the progress and throughput of the Full Scan walker.
type FullScanStatus struct {
	Active      bool    `json:"active"`
	Scanned     int     `json:"scanned"`
	Total       int     `json:"total"`
	FilesPerSec float64 `json:"filesPerSec"`
}

// QueueCounts reports currently queued tasks per priority lane.
type QueueCounts struct {
	High int `json:"high"`
	Bulk int `json:"bulk"`
}

// LatencyStats reports High-priority task latency in milliseconds (enqueue to completion).
type LatencyStats struct {
	EWMA float64 `json:"ewma"`
	Last float64 `json:"last"`
	Max  float64 `json:"max"`
}

// ResolutionCounters tracks sync outcomes executed during the current plugin session.
type ResolutionCounters struct {
	Merged        int `json:"merged"`
	KeptNewer     int `json:"keptNewer"`
	KeptOlder     int `json:"keptOlder"`
	KeptLocal     int `json:"keptLocal"`
	KeptDrive     int `json:"keptDrive"`
	Pushed        int `json:"pushed"`
	Pulled        int `json:"pulled"`
	Trashed       int `json:"trashed"`
	Renamed       int `json:"renamed"`
	ManualFlagged int `json:"manualFlagged"`
}

// ConflictCounts breaks down pending manual interventions by kind.
type ConflictCounts struct {
	Markers  int `json:"markers"`
	Versions int `json:"versions"`
	Missing  int `json:"missing"`
	Total    int `json:"total"`
}

// SyncStatusSnapshot is the full telemetry payload polled by SyncStatusView while open.
type SyncStatusSnapshot struct {
	Paused             bool               `json:"paused"`
	NeedsLogin         bool               `json:"needsLogin"`
	FullScan           FullScanStatus     `json:"fullScan"`
	Queued             QueueCounts        `json:"queued"`
	InFlight           int                `json:"inFlight"`
	HighPriLatencyMs   LatencyStats       `json:"highPriLatencyMs"`
	SessionResolutions ResolutionCounters `json:"sessionResolutions"`
	Conflicts          ConflictCounts     `json:"conflicts"`
}

// LogEntry represents a single structured log record retained in Go's ring buffer.
type LogEntry struct {
	TimestampMs int64  `json:"timestampMs"`
	Severity    string `json:"severity"`
	Message     string `json:"message"`
	Detail      string `json:"detail,omitempty"`
}

// ConflictCandidate describes one competing version surfaced in ReconcileView.
type ConflictCandidate struct {
	Source       string `json:"source"` // "local" | "drive"
	FileID       string `json:"fileId,omitempty"`
	Name         string `json:"name"`
	MimeType     string `json:"mimeType"`
	ModifiedTime string `json:"modifiedTime"`
	Size         int64  `json:"size"`
	Trashed      bool   `json:"trashed,omitempty"`
	MD5          string `json:"md5,omitempty"`
}

// ConflictItem describes a file flagged for manual resolution in the Sync State DB.
type ConflictItem struct {
	Path         string              `json:"path"`
	Kind         string              `json:"kind"` // "markers" | "versions" | "missing"
	Summary      string              `json:"summary"`
	DetectedAtMs int64               `json:"detectedAtMs"`
	Candidates   []ConflictCandidate `json:"candidates"`
}

// VaultEventPayload carries local Obsidian Vault / Workspace events into Go.
type VaultEventPayload struct {
	Kind     string `json:"kind"` // "open" | "create" | "modify" | "delete" | "rename"
	Path     string `json:"path"`
	OldPath  string `json:"oldPath,omitempty"`
	IsFolder bool   `json:"isFolder,omitempty"`
	Size     int64  `json:"size,omitempty"`
	MTimeMs  int64  `json:"mtimeMs,omitempty"`
}

// SetRefreshTokenRequest passes an OAuth refresh token obtained via callback to Go.
type SetRefreshTokenRequest struct {
	Token string `json:"token"`
}

// FlushResult reports whether all High-priority sync tasks drained cleanly.
type FlushResult struct {
	Drained bool `json:"drained"`
}

// SetLogStreamRequest toggles live logEntry event emission when LogView opens/closes.
type SetLogStreamRequest struct {
	Enabled bool `json:"enabled"`
}

// GetLogsRequest optionally filters the returned ring-buffer entries by severity.
type GetLogsRequest struct {
	MinSeverity string `json:"minSeverity,omitempty"`
}

// GetConflictContentRequest identifies a conflict candidate to preview in ReconcileView.
type GetConflictContentRequest struct {
	Path   string `json:"path"`
	Source string `json:"source"` // "local" | "drive"
	FileID string `json:"fileId,omitempty"`
}

// GetConflictContentResult returns text or binary metadata for conflict preview.
type GetConflictContentResult struct {
	Text     string `json:"text,omitempty"`
	IsBinary bool   `json:"isBinary"`
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size"`
}

// ResolveVersionConflictRequest selects the winning version for a versions/missing conflict.
type ResolveVersionConflictRequest struct {
	Path        string `json:"path"`
	Winner      string `json:"winner"` // "local" | "drive" | "delete"
	DriveFileID string `json:"driveFileId,omitempty"`
}

// PathRequest addresses a single vault-relative file path.
type PathRequest struct {
	Path string `json:"path"`
}
