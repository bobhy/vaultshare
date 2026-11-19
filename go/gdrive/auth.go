package gdrive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultRelayBaseURL is the shared Cloudflare OAuth relay operated by vault-share.
const DefaultRelayBaseURL = "https://vault-share-auth.bob-hyman.workers.dev"

const (
	proactiveRefreshWindow = 60 * time.Second
	authFailedCooldown     = 60 * time.Second
)

// AuthError indicates authentication is missing, expired, or rejected by the relay.
type AuthError struct {
	StatusCode int
	Message    string
}

func (e *AuthError) Error() string {
	return e.Message
}

// IsAuthError reports whether err (or any wrapped error) is an *AuthError.
func IsAuthError(err error) bool {
	var ae *AuthError
	return errors.As(err, &ae)
}

// SecretPort persists only the Google OAuth refresh token in Obsidian SecretStorage.
type SecretPort interface {
	LoadRefreshToken(ctx context.Context) (string, error)
	SaveRefreshToken(ctx context.Context, token string) error
}

type tokenRefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type tokenRefreshResponse struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

// TokenManager caches Google OAuth access tokens in memory and mints new ones
// via the Cloudflare relay using an un-throttled HTTP client.
type TokenManager struct {
	httpClient   *http.Client
	secrets      SecretPort
	relayBaseURL string
	now          func() time.Time

	mu           sync.Mutex
	refreshToken string
	accessToken  string
	expiresAt    time.Time
	authFailedAt time.Time
	refreshWait  chan struct{}
}

// NewTokenManager creates a TokenManager backed by an un-throttled HTTP client
// and optional SecretPort for refresh-token persistence.
func NewTokenManager(httpClient *http.Client, secrets SecretPort, relayBaseURL string) *TokenManager {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if strings.TrimSpace(relayBaseURL) == "" {
		relayBaseURL = DefaultRelayBaseURL
	}
	return &TokenManager{
		httpClient:   httpClient,
		secrets:      secrets,
		relayBaseURL: strings.TrimRight(relayBaseURL, "/"),
		now:          time.Now,
	}
}

// LoadFromSecrets reads the persisted refresh token into memory on startup.
func (tm *TokenManager) LoadFromSecrets(ctx context.Context) (bool, error) {
	if tm.secrets == nil {
		tm.mu.Lock()
		has := tm.refreshToken != ""
		tm.mu.Unlock()
		return has, nil
	}
	tok, err := tm.secrets.LoadRefreshToken(ctx)
	if err != nil {
		return false, err
	}
	tok = strings.TrimSpace(tok)
	tm.mu.Lock()
	tm.refreshToken = tok
	tm.accessToken = ""
	tm.expiresAt = time.Time{}
	tm.authFailedAt = time.Time{}
	tm.mu.Unlock()
	return tok != "", nil
}

// HasRefreshToken reports whether a refresh token is currently loaded.
func (tm *TokenManager) HasRefreshToken() bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.refreshToken != ""
}

// SetRefreshToken installs a new refresh token, persists it to SecretPort, and
// clears cached access tokens and error cooldowns.
func (tm *TokenManager) SetRefreshToken(ctx context.Context, refreshToken string) error {
	trimmed := strings.TrimSpace(refreshToken)
	if tm.secrets != nil {
		if err := tm.secrets.SaveRefreshToken(ctx, trimmed); err != nil {
			return fmt.Errorf("persist refresh token: %w", err)
		}
	}
	tm.mu.Lock()
	tm.refreshToken = trimmed
	tm.accessToken = ""
	tm.expiresAt = time.Time{}
	tm.authFailedAt = time.Time{}
	tm.mu.Unlock()
	return nil
}

// Logout clears local credentials and SecretStorage without calling Google /revoke
// (revoking at Google would invalidate all other connected devices).
func (tm *TokenManager) Logout(ctx context.Context) error {
	return tm.SetRefreshToken(ctx, "")
}

// Invalidate drops the cached access token so the next AccessToken call refreshes.
func (tm *TokenManager) Invalidate() {
	tm.mu.Lock()
	tm.accessToken = ""
	tm.expiresAt = time.Time{}
	tm.mu.Unlock()
}

// AccessToken returns a valid Bearer access token, refreshing proactively via the
// relay when needed. Concurrent callers single-flight without holding tm.mu across I/O.
func (tm *TokenManager) AccessToken(ctx context.Context) (string, error) {
	for {
		tm.mu.Lock()
		if tm.refreshToken == "" {
			tm.mu.Unlock()
			return "", &AuthError{Message: "not authenticated: log in to Google Drive in settings"}
		}

		now := tm.now()
		if !tm.authFailedAt.IsZero() && now.Sub(tm.authFailedAt) < authFailedCooldown {
			tm.mu.Unlock()
			return "", &AuthError{Message: "authentication failed recently: log in again in settings"}
		}

		if tm.accessToken != "" && now.Before(tm.expiresAt.Add(-proactiveRefreshWindow)) {
			tok := tm.accessToken
			tm.mu.Unlock()
			return tok, nil
		}

		if tm.refreshWait != nil {
			ch := tm.refreshWait
			tm.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-ch:
				continue
			}
		}

		ch := make(chan struct{})
		tm.refreshWait = ch
		rt := tm.refreshToken
		tm.mu.Unlock()

		parsed, err := tm.requestRefresh(ctx, rt)
		var rotated string
		if err == nil {
			rotated = strings.TrimSpace(parsed.RefreshToken)
			if rotated != "" && rotated != rt && tm.secrets != nil {
				if saveErr := tm.secrets.SaveRefreshToken(ctx, rotated); saveErr != nil {
					err = fmt.Errorf("persist rotated refresh token: %w", saveErr)
				}
			}
		}

		tm.mu.Lock()
		if err != nil {
			if IsAuthError(err) {
				tm.authFailedAt = tm.now()
			}
		} else if tm.refreshToken == rt {
			tm.accessToken = parsed.AccessToken
			tm.expiresAt = tm.now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
			tm.authFailedAt = time.Time{}
			if rotated != "" && rotated != rt {
				tm.refreshToken = rotated
			}
		}
		tok := tm.accessToken
		close(ch)
		tm.refreshWait = nil
		tm.mu.Unlock()

		if err != nil {
			return "", err
		}
		if tok == "" {
			return "", &AuthError{Message: "refresh token cleared during refresh"}
		}
		return tok, nil
	}
}

func (tm *TokenManager) requestRefresh(ctx context.Context, refreshToken string) (tokenRefreshResponse, error) {
	payload, err := json.Marshal(tokenRefreshRequest{RefreshToken: refreshToken})
	if err != nil {
		return tokenRefreshResponse{}, fmt.Errorf("encode refresh request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		tm.relayBaseURL+"/google/token/refresh",
		bytes.NewReader(payload),
	)
	if err != nil {
		return tokenRefreshResponse{}, fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := tm.httpClient.Do(req)
	if err != nil {
		return tokenRefreshResponse{}, fmt.Errorf("token refresh network error: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return tokenRefreshResponse{}, fmt.Errorf("read token refresh response: %w", err)
	}

	if resp.StatusCode >= 400 {
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
			return tokenRefreshResponse{}, &AuthError{
				StatusCode: resp.StatusCode,
				Message:    fmt.Sprintf("token refresh rejected (%d)", resp.StatusCode),
			}
		}
		return tokenRefreshResponse{}, fmt.Errorf("token refresh failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed tokenRefreshResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return tokenRefreshResponse{}, fmt.Errorf("parse token refresh response: %w", err)
	}
	if parsed.AccessToken == "" || parsed.ExpiresIn <= 0 {
		return tokenRefreshResponse{}, &AuthError{Message: "unexpected token refresh response shape"}
	}
	return parsed, nil
}
