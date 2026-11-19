package gdrive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type memorySecrets struct {
	token string
	saves int
}

func (m *memorySecrets) LoadRefreshToken(_ context.Context) (string, error) {
	return m.token, nil
}

func (m *memorySecrets) SaveRefreshToken(_ context.Context, tok string) error {
	m.token = tok
	m.saves++
	return nil
}

func TestTokenManagerRefreshRotationAndLogout(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var req tokenRefreshRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resp := tokenRefreshResponse{
			AccessToken: "acc-" + req.RefreshToken,
			ExpiresIn:   3600,
		}
		if n == 1 {
			resp.RefreshToken = "rt-rotated"
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	secrets := &memorySecrets{token: "rt-initial"}
	tm := NewTokenManager(srv.Client(), secrets, srv.URL)
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	tm.now = func() time.Time { return now }

	ok, err := tm.LoadFromSecrets(ctx)
	if err != nil || !ok {
		t.Fatalf("LoadFromSecrets = (%v, %v)", ok, err)
	}

	tok, err := tm.AccessToken(ctx)
	if err != nil || tok != "acc-rt-initial" {
		t.Fatalf("AccessToken = (%q, %v)", tok, err)
	}
	if secrets.token != "rt-rotated" {
		t.Fatalf("rotated secret not saved: got %q", secrets.token)
	}

	// Subsequent call within window uses cached token without hitting relay.
	tok2, err := tm.AccessToken(ctx)
	if err != nil || tok2 != tok || calls.Load() != 1 {
		t.Fatalf("cached AccessToken = (%q, %v, calls=%d)", tok2, err, calls.Load())
	}

	// Logout clears local secret only without network calls.
	if err := tm.Logout(ctx); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if secrets.token != "" || tm.HasRefreshToken() || calls.Load() != 1 {
		t.Fatalf("Logout state mismatch: secret=%q has=%v calls=%d", secrets.token, tm.HasRefreshToken(), calls.Load())
	}
}

func TestTokenManagerCooldownOnAuthRejection(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	tm := NewTokenManager(srv.Client(), &memorySecrets{}, srv.URL)
	_ = tm.SetRefreshToken(ctx, "bad-rt")

	if _, err := tm.AccessToken(ctx); !IsAuthError(err) {
		t.Fatalf("expected AuthError, got %v", err)
	}
	if _, err := tm.AccessToken(ctx); !IsAuthError(err) || calls.Load() != 1 {
		t.Fatalf("expected cooldown to suppress second relay call; calls=%d err=%v", calls.Load(), err)
	}
}
