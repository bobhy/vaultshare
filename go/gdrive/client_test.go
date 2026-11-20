package gdrive

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	tm := NewTokenManager(srv.Client(), &memorySecrets{token: "rt-1"}, srv.URL)
	tm.now = func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) }
	_, _ = tm.LoadFromSecrets(context.Background())
	// Pre-populate valid token on relay handler or let test handle /google/token/refresh.
	retry := RetryOptions{
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
		MaxDelay:    5 * time.Millisecond,
		Sleep:       func(_ context.Context, _ time.Duration) error { return nil },
		Jitter:      func(_ time.Duration) time.Duration { return 0 },
	}
	client := NewClient(srv.Client(), tm, retry)
	client.SetEndpoints(srv.URL+"/drive/v3", srv.URL+"/upload/drive/v3")
	return client, srv
}

func writeTokenRefresh(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"access_token":"tok-1","expires_in":3600}`))
}

func TestClientRetry401And429AndRevision404(t *testing.T) {
	ctx := context.Background()
	var getCalls atomic.Int32
	var refreshCalls atomic.Int32

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/google/token/refresh":
			refreshCalls.Add(1)
			writeTokenRefresh(w)
		case r.URL.Path == "/drive/v3/files/f-1":
			n := getCalls.Add(1)
			if n == 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if n == 2 {
				http.Error(w, "rate limit", http.StatusTooManyRequests)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"f-1","name":"Note.md","mimeType":"text/markdown","size":"5","headRevisionId":"r-9"}`))
		case r.URL.Path == "/drive/v3/files/f-1/revisions/rev-old":
			http.Error(w, "not found", http.StatusNotFound)
		default:
			http.NotFound(w, r)
		}
	})

	meta, found, err := client.GetFileMeta(ctx, "f-1")
	if err != nil || !found || meta.HeadRevisionID != "r-9" {
		t.Fatalf("GetFileMeta = (%+v, %v, %v)", meta, found, err)
	}
	if refreshCalls.Load() != 2 {
		t.Fatalf("expected 2 token refreshes (initial + after 401), got %d", refreshCalls.Load())
	}

	revBytes, revFound, err := client.DownloadRevision(ctx, "f-1", "rev-old")
	if err != nil || revFound || revBytes != nil {
		t.Fatalf("DownloadRevision 404 = (%v, %v, %v)", revBytes, revFound, err)
	}
}

func TestCreateFileIdempotent409(t *testing.T) {
	ctx := context.Background()
	var genCalls atomic.Int32

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/google/token/refresh":
			writeTokenRefresh(w)
		case r.URL.Path == "/drive/v3/files":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"files":[]}`))
		case r.URL.Path == "/drive/v3/files/generateIds":
			genCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ids":["gen-id-1"]}`))
		case r.URL.Path == "/upload/drive/v3/files":
			// Simulate retried create after earlier timed-out success.
			http.Error(w, "duplicate id", http.StatusConflict)
		case r.URL.Path == "/drive/v3/files/gen-id-1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"gen-id-1","name":"a.md","size":"5","headRevisionId":"r-1"}`))
		default:
			http.NotFound(w, r)
		}
	})

	created, err := client.CreateFile(ctx, "parent-1", "a.md", "text/markdown", 1700000000000, []byte("hello"))
	if err != nil {
		t.Fatalf("CreateFile 409 idempotent recovery failed: %v", err)
	}
	if created.ID != "gen-id-1" || genCalls.Load() != 1 {
		t.Fatalf("created = %+v, genCalls=%d", created, genCalls.Load())
	}
}

func TestResumableUploadAndChangesPaging(t *testing.T) {
	ctx := context.Background()
	payload := bytes.Repeat([]byte("A"), 1024)
	var received []byte
	var chunkCalls atomic.Int32

	var srvURL string
	client, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/google/token/refresh":
			writeTokenRefresh(w)
		case r.URL.Path == "/upload/drive/v3/files/f-big" && r.Method == http.MethodPatch:
			w.Header().Set("Location", srvURL+"/upload-session/s1")
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/upload-session/s1" && r.Method == http.MethodPut:
			if auth := r.Header.Get("Authorization"); auth != "" {
				t.Errorf("unexpected Authorization header on resumable chunk PUT: %q", auth)
			}
			n := chunkCalls.Add(1)
			if n == 2 {
				http.Error(w, "transient 503", http.StatusServiceUnavailable)
				return
			}
			cr := r.Header.Get("Content-Range")
			if strings.HasPrefix(cr, "bytes 0-") {
				received = append(received[:0], make([]byte, 512)...)
				_, _ = r.Body.Read(received[:512])
				w.Header().Set("Range", "bytes=0-511")
				w.WriteHeader(http.StatusPermanentRedirect)
				return
			}
			rest := make([]byte, 512)
			_, _ = r.Body.Read(rest)
			received = append(received, rest...)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"f-big","name":"big.bin","size":"1024","headRevisionId":"r-2"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/drive/v3/changes/startPageToken":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(startPageTokenResp{StartPageToken: "start-42"})
		case r.URL.Path == "/drive/v3/changes":
			pt := r.URL.Query().Get("pageToken")
			w.Header().Set("Content-Type", "application/json")
			if pt == "p1" {
				_ = json.NewEncoder(w).Encode(driveChangesResponse{
					NextPageToken: "p2",
					Changes:       []ChangeItem{{FileID: "f-1", File: &DriveFile{ID: "f-1", Name: "Note.md"}}},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(driveChangesResponse{
				NewStartPageToken: "p3",
				Changes:           []ChangeItem{{FileID: "f-gone", Removed: true}},
			})
		default:
			http.NotFound(w, r)
		}
	})
	srvURL = srv.URL
	client.chunkSize = 512

	// Force resumable path by lowering threshold via direct uploadResumable test call.
	status, body, err := client.uploadResumable(ctx, http.MethodPatch, srv.URL+"/upload/drive/v3/files/f-big?uploadType=resumable", map[string]any{}, "application/octet-stream", payload)
	if err != nil || status != http.StatusOK || !bytes.Equal(received, payload) {
		t.Fatalf("uploadResumable = (status=%d, err=%v, len=%d, body=%s)", status, err, len(received), string(body))
	}

	tok, err := client.GetStartPageToken(ctx)
	if err != nil || tok != "start-42" {
		t.Fatalf("GetStartPageToken = (%q, %v), want (start-42, nil)", tok, err)
	}

	changes, err := client.ListChanges(ctx, "p1")
	if err != nil {
		t.Fatalf("ListChanges: %v", err)
	}
	if changes.NewStartPageToken != "p3" || len(changes.Changes) != 2 || !changes.Changes[1].Removed {
		t.Fatalf("unexpected changes: %+v", changes)
	}
}

func TestLockCreateContextCancelAndEviction(t *testing.T) {
	client := NewClient(nil, nil, DefaultRetryOptions())

	unlock, err := client.lockCreate(context.Background(), "parent-1", "note.md")
	if err != nil {
		t.Fatalf("initial lockCreate: %v", err)
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.lockCreate(cancelCtx, "parent-1", "note.md"); err == nil {
		t.Fatalf("expected canceled context to abort waiting lockCreate")
	}

	unlock()
	client.createMu.Lock()
	remaining := len(client.createLocks)
	client.createMu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected createLocks entry evicted after unlock, got len=%d", remaining)
	}
}
