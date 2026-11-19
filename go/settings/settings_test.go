package settings

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestDefaultSettingsJSONRoundTrip(t *testing.T) {
	want := DefaultSettings()
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var got PluginSettings
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, want)
	}
}

func TestNormalizeSettings(t *testing.T) {
	tests := []struct {
		name  string
		in    PluginSettings
		check func(t *testing.T, got PluginSettings)
	}{
		{
			name: "zero values receive defaults",
			in:   PluginSettings{},
			check: func(t *testing.T, got PluginSettings) {
				def := DefaultSettings()
				if !reflect.DeepEqual(got, def) {
					t.Fatalf("got %+v, want %+v", got, def)
				}
			},
		},
		{
			name: "clamps concurrency, log horizon, and maxWait >= debounce",
			in: PluginSettings{
				LocalDebounceMs:    5000,
				LocalMaxWaitMs:     1000,
				DrivePollActiveMs:  60000,
				DrivePollIdleMs:    10000,
				MaxPendingRequests: 99,
				LogMinSeverity:     "BOGUS",
				LogHorizon:         5,
			},
			check: func(t *testing.T, got PluginSettings) {
				if got.LocalMaxWaitMs != 5000 {
					t.Errorf("LocalMaxWaitMs = %d, want 5000", got.LocalMaxWaitMs)
				}
				if got.DrivePollIdleMs != 60000 {
					t.Errorf("DrivePollIdleMs = %d, want 60000", got.DrivePollIdleMs)
				}
				if got.MaxPendingRequests != MaxPendingRequestsLimit {
					t.Errorf("MaxPendingRequests = %d, want %d", got.MaxPendingRequests, MaxPendingRequestsLimit)
				}
				if got.LogMinSeverity != LogInfo {
					t.Errorf("LogMinSeverity = %q, want %q", got.LogMinSeverity, LogInfo)
				}
				if got.LogHorizon != MinLogHorizon {
					t.Errorf("LogHorizon = %d, want %d", got.LogHorizon, MinLogHorizon)
				}
			},
		},
		{
			name: "sync resolution enforces Merge first, dedup, max middle, Manual last",
			in: PluginSettings{
				SyncResolution: []string{
					ResolutionManual,
					ResolutionKeepDrive,
					ResolutionMerge,
					ResolutionKeepDrive,
					ResolutionKeepNewest,
					ResolutionKeepOldest,
					"invalid",
				},
			},
			check: func(t *testing.T, got PluginSettings) {
				want := []string{ResolutionMerge, ResolutionKeepDrive, ResolutionKeepNewest, ResolutionManual}
				if !reflect.DeepEqual(got.SyncResolution, want) {
					t.Errorf("SyncResolution = %v, want %v", got.SyncResolution, want)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, NormalizeSettings(tc.in))
		})
	}
}

func TestResolveSyncRootFolder(t *testing.T) {
	if got := ResolveSyncRootFolder("My: Vault/1?", ""); got != "My- Vault-1-" {
		t.Errorf("ResolveSyncRootFolder sanitized = %q", got)
	}
	if got := ResolveSyncRootFolder("Ignored", "CustomRoot"); got != "CustomRoot" {
		t.Errorf("ResolveSyncRootFolder custom = %q", got)
	}
	if got := ResolveSyncRootFolder("...", ""); got != "Vault" {
		t.Errorf("ResolveSyncRootFolder fallback = %q", got)
	}
}
