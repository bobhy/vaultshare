package syncengine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"vaultshare/go/gdrive"
	"vaultshare/go/merge"
	"vaultshare/go/settings"
	"vaultshare/go/syncstate"
)

const (
	defaultWorkerCount    = 3
	statusHeartbeatPeriod = time.Minute
	pauseNoticeMessage    = "VaultShare conflict resolution is paused. Use Resume sync in Settings or Sync status to continue."
	loginNoticeMessage    = "VaultShare Google Drive authorization expired or is missing. Sign in again in Settings."
)

type vaultEventReq struct {
	ctx  context.Context
	ev   settings.VaultEventPayload
	done chan error
}

// EngineDeps bundles external ports and clocks required to construct an Engine.
type EngineDeps struct {
	Vault       VaultPort
	Drive       DrivePort
	Auth        AuthPort
	Store       syncstate.Store
	Transport   *gdrive.Transport
	Sink        EventSink
	Now         func() time.Time
	WorkerCount int
}

// Engine coordinates bidirectional local/Drive synchronization, three-way merging,
// prioritized background queues, Full Scan reconciliation, and conflict UI RPCs.
type Engine struct {
	vault     VaultPort
	drive     DrivePort
	auth      AuthPort
	store     syncstate.Store
	cache     *syncstate.Cache
	transport *gdrive.Transport
	sink      EventSink
	queue     *workQueue
	pathLocks *pathLocker
	logs      *logRing
	metrics   *metricsTracker
	now       func() time.Time

	workerCount int

	stateMu       sync.RWMutex
	cfg           settings.PluginSettings
	vaultName     string
	paused        bool
	needsLogin    bool
	loginNotified bool
	offline       bool
	lastEmittedEv settings.GlobalStatusEvent
	hasEmittedEv  bool

	echoMu              sync.Mutex
	inFlightLocalWrites map[string]int
	recentLocalWrites   map[string]echoEntry
	recentDriveWrites   map[string]echoEntry
	inSessionDeletes    map[string]time.Time
	warnedNativeDocs    map[string]bool

	eventIngestMu sync.Mutex
	eventCh       chan vaultEventReq

	debounceMu sync.Mutex
	debounces  map[string]*debounceEntry

	renameMu       sync.Mutex
	pendingDeletes map[string]*pendingDeleteEntry
	pendingCreates map[string]*pendingCreateEntry

	batchMu     sync.Mutex
	nextBatchID uint64
	activeBatch *changeBatch

	fullScanMu      sync.Mutex
	fullScanRunning bool

	pollWakeCh    chan struct{}
	scanTriggerCh chan struct{}

	epochMu     sync.Mutex
	runLoops    bool
	stopped     bool
	epochCancel context.CancelFunc
	epochWG     sync.WaitGroup
}

// NewEngine creates an Engine ready for Start.
func NewEngine(deps EngineDeps) *Engine {
	nowFn := deps.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	workers := deps.WorkerCount
	if workers <= 0 {
		workers = defaultWorkerCount
	}
	defCfg := settings.DefaultSettings()
	return &Engine{
		vault:               deps.Vault,
		drive:               deps.Drive,
		auth:                deps.Auth,
		store:               deps.Store,
		cache:               syncstate.NewCache(deps.Store),
		transport:           deps.Transport,
		sink:                deps.Sink,
		queue:               newWorkQueue(),
		pathLocks:           newPathLocker(),
		logs:                newLogRing(defCfg.LogMinSeverity, defCfg.LogHorizon, deps.Sink),
		metrics:             &metricsTracker{},
		now:                 nowFn,
		workerCount:         workers,
		cfg:                 defCfg,
		inFlightLocalWrites: make(map[string]int),
		recentLocalWrites:   make(map[string]echoEntry),
		recentDriveWrites:   make(map[string]echoEntry),
		inSessionDeletes:    make(map[string]time.Time),
		warnedNativeDocs:    make(map[string]bool),
		eventCh:             make(chan vaultEventReq, 256),
		debounces:           make(map[string]*debounceEntry),
		pendingDeletes:      make(map[string]*pendingDeleteEntry),
		pendingCreates:      make(map[string]*pendingCreateEntry),
		pollWakeCh:          make(chan struct{}, 1),
		scanTriggerCh:       make(chan struct{}, 1),
	}
}

// Start loads persisted state and OAuth secrets, handles syncRootFolder changes,
// captures an initial Drive change cursor when authenticated, and launches workers.
func (e *Engine) Start(ctx context.Context, cfg settings.PluginSettings, vaultName string, runLoops bool) error {
	cfg = settings.NormalizeSettings(cfg)
	if e.transport != nil {
		e.transport.SetMaxPending(cfg.MaxPendingRequests)
	}
	e.logs.Configure(cfg.LogMinSeverity, cfg.LogHorizon)

	if err := e.cache.LoadAll(ctx); err != nil {
		return err
	}

	hasToken, err := e.auth.LoadFromSecrets(ctx)
	if err != nil {
		return err
	}

	resolvedRoot := settings.ResolveSyncRootFolder(vaultName, cfg.SyncRootFolder)
	prevRoot := e.cache.SyncRoot().RootFolderName
	if prevRoot != "" && prevRoot != resolvedRoot {
		e.log(settings.LogInfo, "Sync root folder changed; resetting sync baseline", fmt.Sprintf("%s -> %s", prevRoot, resolvedRoot))
		if err := e.cache.ResetSyncScope(ctx); err != nil {
			return err
		}
	}

	persistedPaused := e.cache.EngineState().Paused

	e.stateMu.Lock()
	e.cfg = cfg
	e.vaultName = vaultName
	e.paused = persistedPaused
	e.needsLogin = !hasToken
	e.loginNotified = false
	e.stateMu.Unlock()

	if persistedPaused && e.sink != nil {
		e.sink.EmitNotice(pauseNoticeMessage)
	}

	if err := e.StartupLocalDiff(ctx); err != nil {
		return err
	}

	needInitialScan := false
	if hasToken {
		if _, err := e.ensureSyncRootID(ctx); err != nil {
			e.handleIOError(err, "EnsureSyncRoot")
		} else {
			if e.cache.ChangeToken().StartPageToken == "" {
				tok, tokErr := e.drive.GetStartPageToken(ctx)
				if tokErr != nil {
					e.handleIOError(tokErr, "GetStartPageToken")
				} else if tok != "" {
					_ = e.cache.PutChangeToken(ctx, syncstate.ChangeTokenMeta{
						StartPageToken: tok,
						UpdatedAtMs:    e.now().UnixMilli(),
					})
				}
			}
			fsMeta := e.cache.FullScan()
			interval := time.Duration(cfg.FullScanIntervalMin) * time.Minute
			if !fsMeta.Initialized || e.now().Sub(time.UnixMilli(fsMeta.LastCompletedAtMs)) >= interval {
				needInitialScan = true
			}
		}
	}

	e.emitGlobalStatusIfChanged()

	e.epochMu.Lock()
	e.runLoops = runLoops
	e.stopped = false
	if runLoops {
		e.startEpochLoopsLocked(needInitialScan)
	}
	e.epochMu.Unlock()
	return nil
}

// Stop cancels all background loops, stops pending timers, and waits for all
// workers, coordinator, and event router goroutines to exit cleanly.
func (e *Engine) Stop() {
	e.epochMu.Lock()
	e.stopped = true
	e.stopEpochLoopsLocked()
	e.epochMu.Unlock()
}

func (e *Engine) startEpochLoopsLocked(runInitialFullScan bool) {
	if e.epochCancel != nil {
		return
	}
	epochCtx, cancel := context.WithCancel(context.Background())
	e.epochCancel = cancel

	for i := 0; i < e.workerCount; i++ {
		e.epochWG.Add(1)
		go func() {
			defer e.epochWG.Done()
			e.workerLoop(epochCtx)
		}()
	}

	e.epochWG.Add(1)
	go func() {
		defer e.epochWG.Done()
		e.coordinatorLoop(epochCtx, runInitialFullScan)
	}()

	e.epochWG.Add(1)
	go func() {
		defer e.epochWG.Done()
		e.eventRouterLoop(epochCtx)
	}()
}

func (e *Engine) stopEpochLoopsLocked() {
	cancel := e.epochCancel
	e.epochCancel = nil
	if cancel != nil {
		cancel()
	}
	e.queue.Wake()
	e.wakePoller()
	e.cancelAllTimers()
	e.epochWG.Wait()
}

// resetEpoch halts all active workers/coordinator/event router goroutines, clears
// transient queues and timers, executes fn with zero concurrent DB writers, and
// restarts a clean epoch when runLoops is active.
func (e *Engine) resetEpoch(fn func() error, runInitialScan bool) error {
	e.epochMu.Lock()
	defer e.epochMu.Unlock()

	e.stopEpochLoopsLocked()
	e.queue.Clear()
	e.clearTransientState()

	if err := fn(); err != nil {
		if e.runLoops && !e.stopped {
			e.startEpochLoopsLocked(false)
		}
		return err
	}

	if e.runLoops && !e.stopped {
		e.startEpochLoopsLocked(runInitialScan)
	}
	return nil
}

func (e *Engine) clearTransientState() {
	e.echoMu.Lock()
	e.inFlightLocalWrites = make(map[string]int)
	e.recentLocalWrites = make(map[string]echoEntry)
	e.recentDriveWrites = make(map[string]echoEntry)
	e.inSessionDeletes = make(map[string]time.Time)
	e.warnedNativeDocs = make(map[string]bool)
	e.echoMu.Unlock()

	e.batchMu.Lock()
	e.activeBatch = nil
	e.batchMu.Unlock()

	// Drain any stale queued Vault events from the previous epoch.
	for {
		select {
		case req := <-e.eventCh:
			if req.done != nil {
				req.done <- nil
			}
		default:
			return
		}
	}
}

func (e *Engine) eventRouterLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-e.eventCh:
			err := e.processVaultEventOrdered(ctx, req.ev)
			if req.done != nil {
				req.done <- err
			}
		}
	}
}

func (e *Engine) workerLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		blocked := e.IsPaused() || e.NeedsLogin()
		task, ok := e.queue.PopNext(blocked)
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-e.queue.WakeChan():
				continue
			}
		}

		err := e.executeTask(ctx, task)
		e.completeBatchTask(ctx, task.Path, task.BatchIDs, err)
		e.queue.FinishTask(task)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				e.log(settings.LogError, "Sync task failed: "+task.Path, err.Error())
			}
		} else if task.Priority == PriorityHigh && !task.EnqueuedAt.IsZero() {
			e.metrics.RecordHighPriLatency(e.now().Sub(task.EnqueuedAt))
		}
		e.emitGlobalStatusIfChanged()
	}
}

func (e *Engine) executeTask(ctx context.Context, task *SyncTask) error {
	switch task.Kind {
	case TaskLocalRename:
		return e.handleLocalFileRename(ctx, task.OldPath, task.Path)
	case TaskLocalFolderRename:
		return e.handleLocalFolderRename(ctx, task.OldPath, task.Path)
	case TaskTrashEmptyFolder:
		e.trashDriveFolderIfEmpty(ctx, task.Path)
		return nil
	default:
		return e.resolvePath(ctx, task.Path)
	}
}

// coordinatorLoop is the sole owner of Drive changes.list polling and Full Scan
// execution, ensuring they never run concurrently or race on FolderRecords.
func (e *Engine) coordinatorLoop(ctx context.Context, runInitialFullScan bool) {
	if runInitialFullScan && !e.IsPaused() && !e.NeedsLogin() {
		_ = e.runFullScan(ctx)
	}

	heartbeat := time.NewTicker(statusHeartbeatPeriod)
	defer heartbeat.Stop()

	for {
		if ctx.Err() != nil {
			return
		}
		cfg := e.getSettings()
		interval := time.Duration(cfg.DrivePollActiveMs) * time.Millisecond
		if e.isOffline() || e.IsPaused() || e.NeedsLogin() {
			interval = time.Duration(cfg.DrivePollIdleMs) * time.Millisecond
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-heartbeat.C:
			timer.Stop()
			e.emitGlobalStatusIfChanged()
		case <-e.scanTriggerCh:
			timer.Stop()
			if !e.IsPaused() && !e.NeedsLogin() {
				_ = e.runFullScan(ctx)
			}
		case <-e.pollWakeCh:
			timer.Stop()
			e.runPollCycle(ctx)
		case <-timer.C:
			e.runPollCycle(ctx)
		}
	}
}

func (e *Engine) runPollCycle(ctx context.Context) {
	if e.IsPaused() || e.NeedsLogin() {
		return
	}
	if err := e.pollDriveChangesOnce(ctx); err == nil {
		e.setOffline(false)
	}
	cfg := e.getSettings()
	fsMeta := e.cache.FullScan()
	interval := time.Duration(cfg.FullScanIntervalMin) * time.Minute
	if !fsMeta.Initialized || e.now().Sub(time.UnixMilli(fsMeta.LastCompletedAtMs)) >= interval {
		_ = e.runFullScan(ctx)
	}
}

func (e *Engine) wakePoller() {
	select {
	case e.pollWakeCh <- struct{}{}:
	default:
	}
}

// RequestFullScan schedules a Full Scan on the coordinatorLoop (never spawning an
// untracked goroutine or running concurrently with pollDriveChangesOnce).
func (e *Engine) RequestFullScan() {
	select {
	case e.scanTriggerCh <- struct{}{}:
	default:
	}
}

// UpdateSettings applies normalized settings dynamically and handles syncRootFolder
// changes with a clean epoch barrier so no old-scope worker can write stale state.
func (e *Engine) UpdateSettings(ctx context.Context, cfg settings.PluginSettings) error {
	cfg = settings.NormalizeSettings(cfg)

	e.stateMu.Lock()
	vaultName := e.vaultName
	prevRoot := settings.ResolveSyncRootFolder(vaultName, e.cfg.SyncRootFolder)
	nextRoot := settings.ResolveSyncRootFolder(vaultName, cfg.SyncRootFolder)
	e.cfg = cfg
	e.stateMu.Unlock()

	if e.transport != nil {
		e.transport.SetMaxPending(cfg.MaxPendingRequests)
	}
	e.logs.Configure(cfg.LogMinSeverity, cfg.LogHorizon)

	if prevRoot != nextRoot && nextRoot != e.cache.SyncRoot().RootFolderName {
		e.log(settings.LogInfo, "Sync root folder updated; resetting sync baseline", fmt.Sprintf("%s -> %s", prevRoot, nextRoot))
		err := e.resetEpoch(func() error {
			if err := e.cache.ResetSyncScope(ctx); err != nil {
				return err
			}
			if !e.NeedsLogin() {
				if _, err := e.ensureSyncRootID(ctx); err != nil {
					e.handleIOError(err, "EnsureSyncRoot")
				} else if tok, err := e.drive.GetStartPageToken(ctx); err == nil && tok != "" {
					_ = e.cache.PutChangeToken(ctx, syncstate.ChangeTokenMeta{
						StartPageToken: tok,
						UpdatedAtMs:    e.now().UnixMilli(),
					})
				}
				_ = e.StartupLocalDiff(ctx)
			}
			return nil
		}, !e.NeedsLogin())
		if err != nil {
			return err
		}
	}

	e.wakePoller()
	e.emitGlobalStatusIfChanged()
	return nil
}

// SetRefreshToken stores an OAuth refresh token, clears NeedsLogin, initializes
// the Drive sync root and cursor if missing, and triggers catch-up sync.
func (e *Engine) SetRefreshToken(ctx context.Context, token string) error {
	if err := e.auth.SetRefreshToken(ctx, token); err != nil {
		return err
	}
	e.setNeedsLogin(false)
	e.setOffline(false)

	if _, err := e.ensureSyncRootID(ctx); err != nil {
		e.handleIOError(err, "EnsureSyncRoot")
		return err
	}
	if e.cache.ChangeToken().StartPageToken == "" {
		tok, err := e.drive.GetStartPageToken(ctx)
		if err != nil {
			e.handleIOError(err, "GetStartPageToken")
			return err
		}
		if err := e.cache.PutChangeToken(ctx, syncstate.ChangeTokenMeta{
			StartPageToken: tok,
			UpdatedAtMs:    e.now().UnixMilli(),
		}); err != nil {
			return err
		}
	}

	e.log(settings.LogInfo, "Google Drive authorization updated", "")
	e.queue.Wake()
	e.wakePoller()
	e.emitGlobalStatusIfChanged()
	return nil
}

// Logout clears locally stored OAuth credentials and enters NeedsLogin without
// altering the persisted user Pause state.
func (e *Engine) Logout(ctx context.Context) error {
	if err := e.auth.Logout(ctx); err != nil {
		return err
	}
	e.setNeedsLogin(true)
	e.log(settings.LogInfo, "Logged out from Google Drive", "")
	e.emitGlobalStatusIfChanged()
	return nil
}

// Pause halts all conflict resolution (High and Bulk workers plus Drive polling)
// while keeping debounce event capture running, and persists paused state.
func (e *Engine) Pause(ctx context.Context) error {
	e.stateMu.Lock()
	e.paused = true
	e.stateMu.Unlock()
	if err := e.cache.PutEngineState(ctx, syncstate.EngineStateMeta{Paused: true}); err != nil {
		return err
	}
	e.log(settings.LogInfo, "Conflict resolution paused", "")
	e.emitGlobalStatusIfChanged()
	return nil
}

// Resume clears persisted pause state and wakes sync workers and Drive polling.
func (e *Engine) Resume(ctx context.Context) error {
	e.stateMu.Lock()
	e.paused = false
	e.stateMu.Unlock()
	if err := e.cache.PutEngineState(ctx, syncstate.EngineStateMeta{Paused: false}); err != nil {
		return err
	}
	e.log(settings.LogInfo, "Conflict resolution resumed", "")
	e.queue.Wake()
	e.wakePoller()
	e.emitGlobalStatusIfChanged()
	return nil
}

// TriggerFullScan initiates a Full Scan synchronously (when background loops are
// off, e.g. in tests) or signals coordinatorLoop when running live.
func (e *Engine) TriggerFullScan(ctx context.Context) error {
	e.epochMu.Lock()
	loopsActive := e.epochCancel != nil
	e.epochMu.Unlock()
	if loopsActive {
		e.RequestFullScan()
		return nil
	}
	return e.runFullScan(ctx)
}

// FlushHighPriority flushes pending local debounces and held deletes, then waits
// up to ctx deadline for all High-priority tasks to drain before sleep/suspend.
func (e *Engine) FlushHighPriority(ctx context.Context) (bool, error) {
	e.FlushDebounces()
	e.FlushPendingDeletes()
	if e.IsPaused() || e.NeedsLogin() {
		return false, nil
	}
	for {
		drained, ch := e.queue.HighDrained()
		if drained {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, nil
		case <-ch:
		}
	}
}

// GetStatusSnapshot returns complete telemetry for SyncStatusView.
func (e *Engine) GetStatusSnapshot() settings.SyncStatusSnapshot {
	fs, lat, res := e.metrics.Snapshot()
	high, bulk, inFlight := e.queue.Counts()
	return settings.SyncStatusSnapshot{
		Paused:     e.IsPaused(),
		NeedsLogin: e.NeedsLogin(),
		FullScan:   fs,
		Queued: settings.QueueCounts{
			High: high,
			Bulk: bulk,
		},
		InFlight:           inFlight,
		HighPriLatencyMs:   lat,
		SessionResolutions: res,
		Conflicts:          e.cache.ConflictCounts(),
	}
}

// GetLogs returns buffered log entries filtered by minSeverity.
func (e *Engine) GetLogs(minSeverity string) []settings.LogEntry {
	return e.logs.Snapshot(minSeverity)
}

// SetLogStream enables or disables live EmitLog push updates for LogView.
func (e *Engine) SetLogStream(enabled bool) {
	e.logs.SetStream(enabled)
}

// ListConflicts returns all active conflict items formatted for ReconcileView.
func (e *Engine) ListConflicts() []settings.ConflictItem {
	recs := e.cache.ListConflicts()
	items := make([]settings.ConflictItem, 0, len(recs))
	for _, rec := range recs {
		if rec.Conflict == nil {
			continue
		}
		candidates := make([]settings.ConflictCandidate, 0, 1+len(rec.DriveVersions))
		if rec.Local != nil && rec.Local.DeletedAtMs == 0 {
			candidates = append(candidates, settings.ConflictCandidate{
				Source:       "local",
				Name:         path.Base(rec.Path),
				MimeType:     firstNonEmpty(rec.MimeType, gdrive.ContentMimeType(rec.Path)),
				ModifiedTime: time.UnixMilli(rec.Local.MtimeMs).UTC().Format(time.RFC3339),
				Size:         rec.Local.Size,
				MD5:          rec.Local.MD5,
			})
		}
		for _, dv := range rec.DriveVersions {
			candidates = append(candidates, settings.ConflictCandidate{
				Source:       "drive",
				FileID:       dv.FileID,
				Name:         firstNonEmpty(dv.Name, path.Base(rec.Path)),
				MimeType:     firstNonEmpty(dv.MimeType, rec.MimeType),
				ModifiedTime: dv.ModifiedTime,
				Size:         dv.Size,
				Trashed:      dv.Trashed,
				MD5:          dv.MD5,
			})
		}
		items = append(items, settings.ConflictItem{
			Path:         rec.Path,
			Kind:         rec.Conflict.Kind,
			Summary:      rec.Conflict.Summary,
			DetectedAtMs: rec.Conflict.DetectedAtMs,
			Candidates:   candidates,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Path < items[j].Path
	})
	return items
}

// GetConflictContent loads local or Drive candidate content for previewing in ReconcileView.
func (e *Engine) GetConflictContent(ctx context.Context, req settings.GetConflictContentRequest) (settings.GetConflictContentResult, error) {
	relPath := normalizeRelPath(req.Path)
	rec, _ := e.cache.GetFile(relPath)

	var (
		data     []byte
		mimeType string
	)
	switch req.Source {
	case "local":
		b, _, err := e.vault.ReadBytes(ctx, relPath)
		if err != nil {
			return settings.GetConflictContentResult{}, err
		}
		data = b
		mimeType = firstNonEmpty(rec.MimeType, gdrive.ContentMimeType(relPath))
	case "drive":
		fileID := req.FileID
		if fileID == "" && len(rec.DriveVersions) > 0 {
			fileID = rec.DriveVersions[0].FileID
		}
		if fileID == "" {
			return settings.GetConflictContentResult{}, errors.New("missing Drive fileId for conflict preview")
		}
		b, err := e.drive.DownloadFile(ctx, fileID)
		if err != nil {
			e.handleIOError(err, "DownloadFile "+fileID)
			return settings.GetConflictContentResult{}, err
		}
		data = b
		mimeType = firstNonEmpty(rec.MimeType, gdrive.ContentMimeType(relPath))
		for _, dv := range rec.DriveVersions {
			if dv.FileID == fileID && dv.MimeType != "" {
				mimeType = dv.MimeType
				break
			}
		}
	default:
		return settings.GetConflictContentResult{}, fmt.Errorf("unknown conflict source %q", req.Source)
	}

	isText := merge.IsTextMergePath(relPath) || (strings.HasPrefix(mimeType, "text/") && utf8.Valid(data)) ||
		(utf8.Valid(data) && !bytes.Contains(data, []byte{0}))
	if !isText {
		return settings.GetConflictContentResult{
			IsBinary: true,
			MimeType: mimeType,
			Size:     int64(len(data)),
		}, nil
	}
	return settings.GetConflictContentResult{
		Text:     string(data),
		IsBinary: false,
		MimeType: mimeType,
		Size:     int64(len(data)),
	}, nil
}

// ResolveVersionConflict applies the user's explicit winner choice ("local", "drive",
// or "delete") for ConflictVersions or ConflictMissing records.
func (e *Engine) ResolveVersionConflict(ctx context.Context, req settings.ResolveVersionConflictRequest) error {
	relPath := normalizeRelPath(req.Path)
	if relPath == "" {
		return errors.New("path is required")
	}
	return e.withPathLock(ctx, relPath, func() error {
		return e.resolveVersionConflictLocked(ctx, relPath, req)
	})
}

func (e *Engine) resolveVersionConflictLocked(ctx context.Context, relPath string, req settings.ResolveVersionConflictRequest) error {
	rec, hasRec := e.cache.GetFile(relPath)

	switch req.Winner {
	case "local":
		rootID, err := e.ensureSyncRootID(ctx)
		if err != nil {
			e.handleIOError(err, "EnsureSyncRoot")
			return err
		}
		data, st, err := e.vault.ReadBytes(ctx, relPath)
		if err != nil {
			return err
		}
		shaHex, md5Hex := computeHashes(data)
		targetID := req.DriveFileID
		if targetID == "" && hasRec && rec.LastSynced != nil {
			targetID = rec.LastSynced.DriveFileID
		}
		if targetID == "" && hasRec && len(rec.DriveVersions) > 0 {
			for _, dv := range rec.DriveVersions {
				if !dv.Trashed {
					targetID = dv.FileID
					break
				}
			}
		}
		if targetID != "" {
			if err := e.pushExistingLocalFile(ctx, relPath, targetID, "", st, data, shaHex, md5Hex); err != nil {
				return err
			}
			e.trashExtraDriveIDs(ctx, rec, targetID)
		} else {
			if err := e.pushNewLocalFile(ctx, rootID, relPath, st, data, shaHex, md5Hex); err != nil {
				return err
			}
		}
		e.metrics.IncResolution(func(c *settings.ResolutionCounters) { c.KeptLocal++ })

	case "drive":
		fileID := req.DriveFileID
		if fileID == "" && hasRec && len(rec.DriveVersions) > 0 {
			fileID = rec.DriveVersions[0].FileID
		}
		if fileID == "" {
			return errors.New("missing Drive fileId for Drive winner")
		}
		meta, found, err := e.drive.GetFileMeta(ctx, fileID)
		if err != nil {
			e.handleIOError(err, "GetFileMeta "+fileID)
			return err
		}
		if !found || meta.Trashed {
			e.markInSessionDelete(relPath)
			if err := e.vault.Trash(ctx, relPath); err != nil {
				return err
			}
			if err := e.cache.DeleteFile(ctx, relPath); err != nil {
				return err
			}
			e.metrics.IncResolution(func(c *settings.ResolutionCounters) { c.Trashed++ })
		} else {
			if err := e.pullDriveFile(ctx, relPath, meta, pullOptions{
				isNew:          !hasRec || rec.LastSynced == nil,
				forceOverwrite: true,
			}); err != nil {
				return err
			}
			e.trashExtraDriveIDs(ctx, rec, meta.ID)
			e.metrics.IncResolution(func(c *settings.ResolutionCounters) { c.KeptDrive++ })
		}

	case "delete":
		e.markInSessionDelete(relPath)
		_ = e.vault.Trash(ctx, relPath)
		e.trashExtraDriveIDs(ctx, rec, "")
		if err := e.cache.DeleteFile(ctx, relPath); err != nil {
			return err
		}
		e.metrics.IncResolution(func(c *settings.ResolutionCounters) { c.Trashed++ })

	default:
		return fmt.Errorf("unknown conflict winner %q", req.Winner)
	}

	e.log(settings.LogInfo, "Resolved conflict manually ("+req.Winner+")", relPath)
	e.emitGlobalStatusIfChanged()
	return nil
}

// ClearMarkerConflict verifies conflict markers have been removed from the local
// note, clears its ConflictMarkers flag, and enqueues the note at PriorityHigh.
func (e *Engine) ClearMarkerConflict(ctx context.Context, rawPath string) error {
	relPath := normalizeRelPath(rawPath)
	if relPath == "" {
		return errors.New("path is required")
	}
	return e.withPathLock(ctx, relPath, func() error {
		data, _, err := e.vault.ReadBytes(ctx, relPath)
		if err != nil {
			return err
		}
		if merge.HasConflictMarkersWith(string(data), merge.StyleForPath(relPath)) {
			return errors.New("conflict markers are still present in " + relPath)
		}
		rec, ok := e.cache.GetFile(relPath)
		if !ok {
			return errors.New("no sync record for " + relPath)
		}
		rec.Conflict = nil
		rec.UpdatedAtMs = e.now().UnixMilli()
		if err := e.cache.PutFile(ctx, rec); err != nil {
			return err
		}
		e.queue.Enqueue(relPath, PriorityHigh, "manual-clear", e.now(), 0)
		e.log(settings.LogInfo, "Cleared marker conflict", relPath)
		e.emitGlobalStatusIfChanged()
		return nil
	})
}

func (e *Engine) isPathBusy(path string) bool {
	return e.queue.IsInFlight(path) || e.pathLocks.IsBusy(path)
}

func (e *Engine) withPathLock(ctx context.Context, path string, fn func() error) error {
	return e.withPathLocks(ctx, []string{path}, fn)
}

func (e *Engine) withPathLocks(ctx context.Context, paths []string, fn func() error) error {
	release, err := e.pathLocks.Acquire(ctx, paths...)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

// ClearAllState halts active background work, clears queues/timers and the Sync
// State DB under an epoch barrier, and restarts cleanly while keeping auth intact.
func (e *Engine) ClearAllState(ctx context.Context) error {
	err := e.resetEpoch(func() error {
		if err := e.cache.ClearAll(ctx); err != nil {
			return err
		}
		e.stateMu.Lock()
		e.paused = false
		e.stateMu.Unlock()
		return nil
	}, false)
	if err != nil {
		return err
	}

	e.log(settings.LogInfo, "Cleared all local sync state", "")
	e.emitGlobalStatusIfChanged()
	return nil
}

// IsPaused reports whether conflict resolution is currently paused by the user.
func (e *Engine) IsPaused() bool {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return e.paused
}

// NeedsLogin reports whether OAuth authorization is missing or expired.
func (e *Engine) NeedsLogin() bool {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return e.needsLogin
}

func (e *Engine) setNeedsLogin(v bool) {
	e.stateMu.Lock()
	was := e.needsLogin
	e.needsLogin = v
	shouldNotify := v && !e.loginNotified
	if shouldNotify {
		e.loginNotified = true
	} else if !v {
		e.loginNotified = false
	}
	e.stateMu.Unlock()

	if shouldNotify && !was && e.sink != nil {
		e.sink.EmitNotice(loginNoticeMessage)
	}
}

func (e *Engine) isOffline() bool {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return e.offline
}

func (e *Engine) setOffline(v bool) {
	e.stateMu.Lock()
	e.offline = v
	e.stateMu.Unlock()
}

func (e *Engine) getSettings() settings.PluginSettings {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return e.cfg
}

func (e *Engine) ensureSyncRootID(ctx context.Context) (string, error) {
	meta := e.cache.SyncRoot()
	e.stateMu.RLock()
	desiredName := settings.ResolveSyncRootFolder(e.vaultName, e.cfg.SyncRootFolder)
	e.stateMu.RUnlock()

	if meta.RootFolderID != "" && meta.RootFolderName == desiredName {
		return meta.RootFolderID, nil
	}
	ids, err := e.drive.EnsureSyncRoot(ctx, desiredName)
	if err != nil {
		return "", err
	}
	if err := e.cache.PutSyncRoot(ctx, syncstate.SyncRootMeta{
		VaultShareFolderID: ids.VaultShareFolderID,
		RootFolderID:       ids.RootFolderID,
		RootFolderName:     desiredName,
	}); err != nil {
		return "", err
	}
	return ids.RootFolderID, nil
}

func (e *Engine) trashExtraDriveIDs(ctx context.Context, rec syncstate.FileRecord, keepID string) {
	for _, id := range rec.DriveFileIDs {
		if id == "" || id == keepID {
			continue
		}
		e.recordDriveEcho(id, "")
		_ = e.drive.TrashFile(ctx, id)
	}
}

func (e *Engine) handleIOError(err error, op string) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	if gdrive.IsAuthError(err) {
		e.setNeedsLogin(true)
		e.log(settings.LogError, "Google Drive authorization error ("+op+")", err.Error())
		e.emitGlobalStatusIfChanged()
		return
	}
	if errors.Is(err, gdrive.ErrOffline) {
		e.setOffline(true)
		e.log(settings.LogWarning, "Network offline during "+op, err.Error())
		return
	}
	e.log(settings.LogWarning, "Drive I/O error during "+op, err.Error())
}

func (e *Engine) emitGlobalStatusIfChanged() {
	if e.sink == nil {
		return
	}
	ev := settings.GlobalStatusEvent{
		Paused:         e.IsPaused(),
		NeedsLogin:     e.NeedsLogin(),
		ConflictsTotal: e.cache.ConflictCounts().Total,
	}
	e.stateMu.Lock()
	changed := !e.hasEmittedEv || e.lastEmittedEv != ev
	if changed {
		e.lastEmittedEv = ev
		e.hasEmittedEv = true
	}
	e.stateMu.Unlock()
	if changed {
		e.sink.EmitGlobalStatus(ev)
	}
}

func (e *Engine) log(sev, msg, detail string) {
	e.logs.Log(sev, msg, detail, e.now())
}

func (e *Engine) cancelAllTimers() {
	e.debounceMu.Lock()
	for k, ent := range e.debounces {
		if ent.timer != nil {
			ent.timer.Stop()
		}
		delete(e.debounces, k)
	}
	e.debounceMu.Unlock()

	e.renameMu.Lock()
	for k, del := range e.pendingDeletes {
		if del.timer != nil {
			del.timer.Stop()
		}
		delete(e.pendingDeletes, k)
	}
	clear(e.pendingCreates)
	e.renameMu.Unlock()
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
