//go:build js && wasm

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"vaultshare/go/gdrive"
	"vaultshare/go/settings"
	"vaultshare/go/syncengine"
)

type jsEventSink struct{}

func (jsEventSink) EmitGlobalStatus(ev settings.GlobalStatusEvent) {
	emitHostJSON("globalStatus", ev)
}

func (jsEventSink) EmitLog(entry settings.LogEntry) {
	emitHostJSON("logEntry", entry)
}

func (jsEventSink) EmitNotice(message string) {
	emitHostJSON("notice", struct {
		Message string `json:"message"`
	}{Message: message})
}

func emitHostJSON(kind string, payload any) {
	h, err := getHostHandles()
	if err != nil {
		return
	}
	fn := h.host.Get("emitEvent")
	if fn.IsNull() || fn.IsUndefined() {
		return
	}
	b, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return
	}
	fn.Invoke(kind, string(b))
}

// syncService owns the initialized Store, SecretPort, Google Drive client, and
// syncengine.Engine exposed across the window.__vaultshare RPC boundary.
type syncService struct {
	mu     sync.Mutex
	store  *idbStore
	secret *jsSecretPort
	engine *syncengine.Engine
	ctx    context.Context
	cancel context.CancelFunc
}

func newSyncService() *syncService {
	ctx, cancel := context.WithCancel(context.Background())
	return &syncService{
		ctx:    ctx,
		cancel: cancel,
	}
}

func (s *syncService) shutdown() {
	s.cancel()
	s.mu.Lock()
	eng := s.engine
	st := s.store
	s.engine = nil
	s.store = nil
	s.mu.Unlock()

	if eng != nil {
		eng.Stop()
	}
	if st != nil {
		_ = st.Close()
	}
}

func (s *syncService) getEngine() (*syncengine.Engine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engine == nil {
		return nil, errors.New("sync engine has not started")
	}
	return s.engine, nil
}

func (s *syncService) handleStart(payload string) (settings.GlobalStatusEvent, error) {
	var cfg settings.PluginSettings
	if err := json.Unmarshal([]byte(payload), &cfg); err != nil {
		return settings.GlobalStatusEvent{}, err
	}
	cfg = settings.NormalizeSettings(cfg)

	h, err := getHostHandles()
	if err != nil {
		return settings.GlobalStatusEvent{}, err
	}
	vaultName := h.vault.Call("getName").String()

	s.mu.Lock()
	if s.engine != nil {
		eng := s.engine
		s.mu.Unlock()
		if err := eng.UpdateSettings(s.ctx, cfg); err != nil {
			return settings.GlobalStatusEvent{}, err
		}
		return globalStatusFromEngine(eng), nil
	}

	store := newIDBStore(vaultName)
	secret := newJSSecretPort(vaultName)
	authTransport := gdrive.NewTransport(jsRequestUrlDo, 0)
	driveTransport := gdrive.NewTransport(jsRequestUrlDo, cfg.MaxPendingRequests)
	tokenMgr := gdrive.NewTokenManager(&http.Client{Transport: authTransport}, secret, gdrive.DefaultRelayBaseURL)
	driveClient := gdrive.NewClient(&http.Client{Transport: driveTransport}, tokenMgr, gdrive.DefaultRetryOptions())

	eng := syncengine.NewEngine(syncengine.EngineDeps{
		Vault:     jsVaultPort{},
		Drive:     driveClient,
		Auth:      tokenMgr,
		Store:     store,
		Transport: driveTransport,
		Sink:      jsEventSink{},
	})
	s.store = store
	s.secret = secret
	s.engine = eng
	s.mu.Unlock()

	if err := eng.Start(s.ctx, cfg, vaultName, true); err != nil {
		return settings.GlobalStatusEvent{}, err
	}
	return globalStatusFromEngine(eng), nil
}

func (s *syncService) invoke(method, payload string) (any, error) {
	if method == "start" {
		return s.handleStart(payload)
	}

	eng, err := s.getEngine()
	if err != nil {
		return nil, err
	}

	switch method {
	case "updateSettings":
		var cfg settings.PluginSettings
		if err := json.Unmarshal([]byte(payload), &cfg); err != nil {
			return nil, err
		}
		if err := eng.UpdateSettings(s.ctx, cfg); err != nil {
			return nil, err
		}
		return globalStatusFromEngine(eng), nil

	case "setRefreshToken":
		var req settings.SetRefreshTokenRequest
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			return nil, err
		}
		if err := eng.SetRefreshToken(s.ctx, req.Token); err != nil {
			return nil, err
		}
		return globalStatusFromEngine(eng), nil

	case "logout":
		if err := eng.Logout(s.ctx); err != nil {
			return nil, err
		}
		return globalStatusFromEngine(eng), nil

	case "onVaultEvent":
		var req settings.VaultEventPayload
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			return nil, err
		}
		if err := eng.OnVaultEvent(s.ctx, req); err != nil {
			return nil, err
		}
		return struct{}{}, nil

	case "flushHighPriority":
		flushCtx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
		defer cancel()
		drained, err := eng.FlushHighPriority(flushCtx)
		if err != nil {
			return nil, err
		}
		return settings.FlushResult{Drained: drained}, nil

	case "pause":
		if err := eng.Pause(s.ctx); err != nil {
			return nil, err
		}
		return globalStatusFromEngine(eng), nil

	case "resume":
		if err := eng.Resume(s.ctx); err != nil {
			return nil, err
		}
		return globalStatusFromEngine(eng), nil

	case "startFullScan":
		eng.RequestFullScan()
		return struct{}{}, nil

	case "getStatusSnapshot":
		return eng.GetStatusSnapshot(), nil

	case "getLogs":
		var req settings.GetLogsRequest
		if payload != "" && payload != "{}" {
			if err := json.Unmarshal([]byte(payload), &req); err != nil {
				return nil, err
			}
		}
		return eng.GetLogs(req.MinSeverity), nil

	case "setLogStream":
		var req settings.SetLogStreamRequest
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			return nil, err
		}
		eng.SetLogStream(req.Enabled)
		return struct{}{}, nil

	case "listConflicts":
		return eng.ListConflicts(), nil

	case "getConflictContent":
		var req settings.GetConflictContentRequest
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			return nil, err
		}
		return eng.GetConflictContent(s.ctx, req)

	case "resolveVersionConflict":
		var req settings.ResolveVersionConflictRequest
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			return nil, err
		}
		if err := eng.ResolveVersionConflict(s.ctx, req); err != nil {
			return nil, err
		}
		return struct{}{}, nil

	case "clearMarkerConflict":
		var req settings.PathRequest
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			return nil, err
		}
		if err := eng.ClearMarkerConflict(s.ctx, req.Path); err != nil {
			return nil, err
		}
		return struct{}{}, nil

	case "clearAllState":
		if err := eng.ClearAllState(s.ctx); err != nil {
			return nil, err
		}
		return globalStatusFromEngine(eng), nil

	default:
		return nil, errors.New("unknown bridge method: " + method)
	}
}

func globalStatusFromEngine(eng *syncengine.Engine) settings.GlobalStatusEvent {
	snap := eng.GetStatusSnapshot()
	return settings.GlobalStatusEvent{
		Paused:         snap.Paused,
		NeedsLogin:     snap.NeedsLogin,
		ConflictsTotal: snap.Conflicts.Total,
	}
}
