package gdrive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"vaultshare/go/syncstate"
)

const (
	defaultDriveAPIBase   = "https://www.googleapis.com/drive/v3"
	defaultDriveUploadAPI = "https://www.googleapis.com/upload/drive/v3"
	// FolderMimeType is Google Drive's MIME type for folder resources.
	FolderMimeType     = "application/vnd.google-apps.folder"
	googleAppsPrefix   = "application/vnd.google-apps."
	vaultShareRootName = "VaultShare"
	fileFields         = "id,name,mimeType,parents,trashed,modifiedTime,size,md5Checksum,sha256Checksum,headRevisionId"
)

// ErrAlreadyExists is returned by CreateFile when a file with the same name
// already exists inside the target parent folder.
var ErrAlreadyExists = errors.New("gdrive: file already exists in parent folder")

// RetryOptions configures exponential backoff retries for transient Drive API errors.
type RetryOptions struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Sleep       func(ctx context.Context, d time.Duration) error
	Jitter      func(base time.Duration) time.Duration
}

// DefaultRetryOptions returns production backoff defaults (~0.5s doubling up to 30s across 6 attempts).
func DefaultRetryOptions() RetryOptions {
	return RetryOptions{
		MaxAttempts: 6,
		BaseDelay:   500 * time.Millisecond,
		MaxDelay:    30 * time.Second,
		Sleep:       sleepContext,
		Jitter: func(base time.Duration) time.Duration {
			if base <= 0 {
				return 0
			}
			return time.Duration(rand.Int64N(int64(base)))
		},
	}
}

// APIError represents a non-2xx response returned by Google Drive v3.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return e.Message
}

// DriveFile represents a Google Drive v3 File resource projected for sync.
type DriveFile struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	MimeType       string   `json:"mimeType"`
	Parents        []string `json:"parents"`
	Trashed        bool     `json:"trashed"`
	ModifiedTime   string   `json:"modifiedTime"`
	Size           string   `json:"size"`
	MD5Checksum    string   `json:"md5Checksum"`
	SHA256Checksum string   `json:"sha256Checksum"`
	HeadRevisionID string   `json:"headRevisionId"`
}

// ByteSize parses Drive's string-encoded byte size into int64.
func (f DriveFile) ByteSize() int64 {
	sz, _ := strconv.ParseInt(strings.TrimSpace(f.Size), 10, 64)
	return sz
}

// FirstParent returns the primary parent folder ID, or empty string if absent.
func (f DriveFile) FirstParent() string {
	if len(f.Parents) == 0 {
		return ""
	}
	return f.Parents[0]
}

// IsGoogleNativeDoc reports whether mimeType is a non-folder Google Workspace native document.
func IsGoogleNativeDoc(mimeType string) bool {
	return strings.HasPrefix(mimeType, googleAppsPrefix) && mimeType != FolderMimeType
}

// SyncRootIDs identifies the resolved VaultShare container and vault sync root folders.
type SyncRootIDs struct {
	VaultShareFolderID string
	RootFolderID       string
}

// ChangeItem represents a single entry returned by Google Drive changes.list.
type ChangeItem struct {
	FileID  string     `json:"fileId"`
	Removed bool       `json:"removed"`
	File    *DriveFile `json:"file"`
}

// ChangePage holds all changes gathered across one changes.list poll cycle.
type ChangePage struct {
	Changes           []ChangeItem
	NewStartPageToken string
}

type createKey struct {
	parentID string
	name     string
}

// Client calls the Google Drive v3 REST API over a semaphore-capped *http.Client.
type Client struct {
	httpClient *http.Client
	auth       *TokenManager
	apiBase    string
	uploadBase string
	retry      RetryOptions

	chunkSize int // configurable for tests; defaults to 4 MiB

	createMu    sync.Mutex
	createLocks map[createKey]*sync.Mutex
}

// NewClient constructs a Drive v3 Client with the provided HTTP client and TokenManager.
func NewClient(httpClient *http.Client, auth *TokenManager, retry RetryOptions) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	def := DefaultRetryOptions()
	if retry.MaxAttempts <= 0 {
		retry.MaxAttempts = def.MaxAttempts
	}
	if retry.BaseDelay <= 0 {
		retry.BaseDelay = def.BaseDelay
	}
	if retry.MaxDelay <= 0 {
		retry.MaxDelay = def.MaxDelay
	}
	if retry.Sleep == nil {
		retry.Sleep = def.Sleep
	}
	if retry.Jitter == nil {
		retry.Jitter = def.Jitter
	}
	return &Client{
		httpClient:  httpClient,
		auth:        auth,
		apiBase:     defaultDriveAPIBase,
		uploadBase:  defaultDriveUploadAPI,
		retry:       retry,
		chunkSize:   defaultResumableChunkSize,
		createLocks: make(map[createKey]*sync.Mutex),
	}
}

// SetEndpoints overrides the Drive API and upload base URLs (used in tests).
func (c *Client) SetEndpoints(apiBase, uploadBase string) {
	c.apiBase = strings.TrimRight(apiBase, "/")
	c.uploadBase = strings.TrimRight(uploadBase, "/")
}

func (c *Client) lockCreate(parentID, name string) func() {
	key := createKey{parentID: parentID, name: name}
	c.createMu.Lock()
	mu, ok := c.createLocks[key]
	if !ok {
		mu = &sync.Mutex{}
		c.createLocks[key] = mu
	}
	c.createMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

type driveFileListResponse struct {
	Files         []DriveFile `json:"files"`
	NextPageToken string      `json:"nextPageToken"`
}

// EnsureSyncRoot resolves or creates `My Drive / VaultShare / <syncRootFolder>`.
func (c *Client) EnsureSyncRoot(ctx context.Context, syncRootFolder string) (SyncRootIDs, error) {
	vsID, err := c.ensureSingleFolder(ctx, "root", vaultShareRootName)
	if err != nil {
		return SyncRootIDs{}, fmt.Errorf("ensure VaultShare root: %w", err)
	}
	rootID, err := c.ensureSingleFolder(ctx, vsID.ID, syncRootFolder)
	if err != nil {
		return SyncRootIDs{}, fmt.Errorf("ensure sync root %q: %w", syncRootFolder, err)
	}
	return SyncRootIDs{
		VaultShareFolderID: vsID.ID,
		RootFolderID:       rootID.ID,
	}, nil
}

// EnsureFolderPath resolves or creates every segment of relDir recursively under rootID,
// returning the leaf folder ID and any newly discovered/created FolderRecords.
func (c *Client) EnsureFolderPath(ctx context.Context, rootID, relDir string) (string, []syncstate.FolderRecord, error) {
	segments := splitFolderPath(relDir)
	if len(segments) == 0 {
		return rootID, nil, nil
	}

	parentID := rootID
	var curPath string
	var records []syncstate.FolderRecord

	for _, seg := range segments {
		if curPath == "" {
			curPath = seg
		} else {
			curPath = curPath + "/" + seg
		}
		folder, err := c.ensureSingleFolder(ctx, parentID, seg)
		if err != nil {
			return "", records, err
		}
		records = append(records, syncstate.FolderRecord{
			DriveFolderID: folder.ID,
			ParentID:      parentID,
			Name:          seg,
			Path:          curPath,
			ModifiedTime:  folder.ModifiedTime,
		})
		parentID = folder.ID
	}
	return parentID, records, nil
}

func (c *Client) ensureSingleFolder(ctx context.Context, parentID, name string) (DriveFile, error) {
	unlock := c.lockCreate(parentID, name)
	defer unlock()

	matches, err := c.findChildren(ctx, parentID, name, true)
	if err != nil {
		return DriveFile{}, err
	}
	if len(matches) > 0 {
		return pickCanonicalFolder(matches), nil
	}
	return c.createFolder(ctx, parentID, name)
}

func pickCanonicalFolder(folders []DriveFile) DriveFile {
	sort.Slice(folders, func(i, j int) bool {
		if folders[i].ModifiedTime != folders[j].ModifiedTime {
			return folders[i].ModifiedTime < folders[j].ModifiedTime
		}
		return folders[i].ID < folders[j].ID
	})
	return folders[0]
}

// ListChildren returns all non-trashed subfolders and regular files directly inside folderID.
func (c *Client) ListChildren(ctx context.Context, folderID string) (folders []DriveFile, files []DriveFile, err error) {
	q := fmt.Sprintf("'%s' in parents and trashed=false", escapeQueryLiteral(folderID))
	fields := "nextPageToken,files(" + fileFields + ")"
	baseURL := fmt.Sprintf(
		"%s/files?q=%s&fields=%s&pageSize=1000",
		c.apiBase,
		url.QueryEscape(q),
		url.QueryEscape(fields),
	)

	pageToken := ""
	for {
		reqURL := baseURL
		if pageToken != "" {
			reqURL += "&pageToken=" + url.QueryEscape(pageToken)
		}
		status, body, reqErr := c.doAuthenticated(ctx, http.MethodGet, reqURL, nil, nil)
		if reqErr != nil {
			return nil, nil, reqErr
		}
		if err := checkStatus(status, body); err != nil {
			return nil, nil, err
		}
		var page driveFileListResponse
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, nil, fmt.Errorf("parse Drive child list: %w", err)
		}
		for _, item := range page.Files {
			if item.MimeType == FolderMimeType {
				folders = append(folders, item)
			} else if !IsGoogleNativeDoc(item.MimeType) {
				files = append(files, item)
			}
		}
		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}
	return folders, files, nil
}

// FindInParents queries all supplied parent folder IDs for non-trashed files named name.
func (c *Client) FindInParents(ctx context.Context, parentIDs []string, name string) ([]DriveFile, error) {
	var all []DriveFile
	seen := make(map[string]bool)
	for _, pid := range parentIDs {
		matches, err := c.findChildren(ctx, pid, name, false)
		if err != nil {
			return nil, err
		}
		for _, f := range matches {
			if !seen[f.ID] && !IsGoogleNativeDoc(f.MimeType) {
				seen[f.ID] = true
				all = append(all, f)
			}
		}
	}
	return all, nil
}

func (c *Client) findChildren(ctx context.Context, parentID, name string, wantFolder bool) ([]DriveFile, error) {
	mimeClause := "mimeType!='" + FolderMimeType + "'"
	if wantFolder {
		mimeClause = "mimeType='" + FolderMimeType + "'"
	}
	q := fmt.Sprintf("'%s' in parents and name='%s' and %s and trashed=false", escapeQueryLiteral(parentID), escapeQueryLiteral(name), mimeClause)
	baseURL := fmt.Sprintf(
		"%s/files?q=%s&fields=%s&pageSize=1000",
		c.apiBase,
		url.QueryEscape(q),
		url.QueryEscape("nextPageToken,files("+fileFields+")"),
	)

	var out []DriveFile
	pageToken := ""
	for {
		reqURL := baseURL
		if pageToken != "" {
			reqURL += "&pageToken=" + url.QueryEscape(pageToken)
		}
		status, body, err := c.doAuthenticated(ctx, http.MethodGet, reqURL, nil, nil)
		if err != nil {
			return nil, err
		}
		if err := checkStatus(status, body); err != nil {
			return nil, err
		}
		var page driveFileListResponse
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("parse Drive child search: %w", err)
		}
		out = append(out, page.Files...)
		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}
	return out, nil
}

// GetFileMeta fetches metadata for fileID, returning found=false on 404.
func (c *Client) GetFileMeta(ctx context.Context, fileID string) (DriveFile, bool, error) {
	reqURL := fmt.Sprintf("%s/files/%s?fields=%s", c.apiBase, url.PathEscape(fileID), url.QueryEscape(fileFields))
	status, body, err := c.doAuthenticated(ctx, http.MethodGet, reqURL, nil, nil)
	if err != nil {
		return DriveFile{}, false, err
	}
	if status == http.StatusNotFound {
		return DriveFile{}, false, nil
	}
	if err := checkStatus(status, body); err != nil {
		return DriveFile{}, false, err
	}
	var meta DriveFile
	if err := json.Unmarshal(body, &meta); err != nil {
		return DriveFile{}, false, fmt.Errorf("parse Drive file metadata: %w", err)
	}
	return meta, true, nil
}

// DownloadFile downloads the raw binary content of fileID.
func (c *Client) DownloadFile(ctx context.Context, fileID string) ([]byte, error) {
	reqURL := fmt.Sprintf("%s/files/%s?alt=media", c.apiBase, url.PathEscape(fileID))
	status, body, err := c.doAuthenticated(ctx, http.MethodGet, reqURL, nil, nil)
	if err != nil {
		return nil, err
	}
	if err := checkStatus(status, body); err != nil {
		return nil, err
	}
	return body, nil
}

// DownloadRevision downloads an earlier Drive file revision for 3-way merge base reconstruction.
// Returns (nil, false, nil) when Drive has pruned the revision (HTTP 404).
func (c *Client) DownloadRevision(ctx context.Context, fileID, revisionID string) ([]byte, bool, error) {
	if strings.TrimSpace(revisionID) == "" {
		return nil, false, nil
	}
	reqURL := fmt.Sprintf("%s/files/%s/revisions/%s?alt=media", c.apiBase, url.PathEscape(fileID), url.PathEscape(revisionID))
	status, body, err := c.doAuthenticated(ctx, http.MethodGet, reqURL, nil, nil)
	if err != nil {
		return nil, false, err
	}
	if status == http.StatusNotFound {
		return nil, false, nil
	}
	if err := checkStatus(status, body); err != nil {
		return nil, false, err
	}
	return body, true, nil
}

// MoveRenameFile moves fileID from oldParentID to newParentID (when distinct) and updates its name.
func (c *Client) MoveRenameFile(ctx context.Context, fileID, newParentID, oldParentID, newName string) (DriveFile, error) {
	q := url.Values{}
	q.Set("fields", fileFields)
	if newParentID != "" && newParentID != oldParentID {
		q.Set("addParents", newParentID)
		if oldParentID != "" {
			q.Set("removeParents", oldParentID)
		}
	}
	bodyMap := map[string]string{"name": newName}
	payload, err := json.Marshal(bodyMap)
	if err != nil {
		return DriveFile{}, err
	}
	reqURL := fmt.Sprintf("%s/files/%s?%s", c.apiBase, url.PathEscape(fileID), q.Encode())
	status, respBody, err := c.doAuthenticated(ctx, http.MethodPatch, reqURL, map[string]string{"Content-Type": "application/json"}, payload)
	if err != nil {
		return DriveFile{}, err
	}
	if err := checkStatus(status, respBody); err != nil {
		return DriveFile{}, err
	}
	var updated DriveFile
	if err := json.Unmarshal(respBody, &updated); err != nil {
		return DriveFile{}, fmt.Errorf("parse moved Drive file: %w", err)
	}
	return updated, nil
}

// TrashFile moves fileID to Google Drive Trash (`trashed: true`).
func (c *Client) TrashFile(ctx context.Context, fileID string) error {
	payload := []byte(`{"trashed":true}`)
	reqURL := fmt.Sprintf("%s/files/%s?fields=%s", c.apiBase, url.PathEscape(fileID), url.QueryEscape("id,trashed"))
	status, respBody, err := c.doAuthenticated(ctx, http.MethodPatch, reqURL, map[string]string{"Content-Type": "application/json"}, payload)
	if err != nil {
		return err
	}
	return checkStatus(status, respBody)
}

type startPageTokenResp struct {
	StartPageToken string `json:"startPageToken"`
}

// GetStartPageToken fetches the current Drive changes cursor.
func (c *Client) GetStartPageToken(ctx context.Context) (string, error) {
	reqURL := fmt.Sprintf("%s/changes/getStartPageToken", c.apiBase)
	status, body, err := c.doAuthenticated(ctx, http.MethodGet, reqURL, nil, nil)
	if err != nil {
		return "", err
	}
	if err := checkStatus(status, body); err != nil {
		return "", err
	}
	var parsed startPageTokenResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse startPageToken: %w", err)
	}
	return parsed.StartPageToken, nil
}

type driveChangesResponse struct {
	NextPageToken     string       `json:"nextPageToken"`
	NewStartPageToken string       `json:"newStartPageToken"`
	Changes           []ChangeItem `json:"changes"`
}

// ListChanges fetches all pages of Drive changes starting from pageToken.
func (c *Client) ListChanges(ctx context.Context, pageToken string) (ChangePage, error) {
	fields := "nextPageToken,newStartPageToken,changes(fileId,removed,file(" + fileFields + "))"
	var out ChangePage
	curToken := pageToken

	for {
		q := url.Values{}
		q.Set("pageToken", curToken)
		q.Set("includeRemoved", "true")
		q.Set("spaces", "drive")
		q.Set("pageSize", "1000")
		q.Set("fields", fields)

		reqURL := fmt.Sprintf("%s/changes?%s", c.apiBase, q.Encode())
		status, body, err := c.doAuthenticated(ctx, http.MethodGet, reqURL, nil, nil)
		if err != nil {
			return ChangePage{}, err
		}
		if err := checkStatus(status, body); err != nil {
			return ChangePage{}, err
		}
		var page driveChangesResponse
		if err := json.Unmarshal(body, &page); err != nil {
			return ChangePage{}, fmt.Errorf("parse changes.list: %w", err)
		}
		out.Changes = append(out.Changes, page.Changes...)
		if page.NextPageToken == "" {
			out.NewStartPageToken = page.NewStartPageToken
			break
		}
		curToken = page.NextPageToken
	}
	return out, nil
}

func (c *Client) createFolder(ctx context.Context, parentID, name string) (DriveFile, error) {
	payload, err := json.Marshal(map[string]any{
		"name":     name,
		"mimeType": FolderMimeType,
		"parents":  []string{parentID},
	})
	if err != nil {
		return DriveFile{}, err
	}
	reqURL := fmt.Sprintf("%s/files?fields=%s", c.apiBase, url.QueryEscape(fileFields))
	status, body, err := c.doAuthenticated(ctx, http.MethodPost, reqURL, map[string]string{"Content-Type": "application/json"}, payload)
	if err != nil {
		return DriveFile{}, err
	}
	if err := checkStatus(status, body); err != nil {
		return DriveFile{}, err
	}
	var folder DriveFile
	if err := json.Unmarshal(body, &folder); err != nil {
		return DriveFile{}, fmt.Errorf("parse created Drive folder: %w", err)
	}
	return folder, nil
}

func (c *Client) doAuthenticated(
	ctx context.Context,
	method string,
	reqURL string,
	extraHeaders map[string]string,
	body []byte,
) (int, []byte, error) {
	status, respBody, err := c.requestWithRetry(ctx, method, reqURL, extraHeaders, body, true)
	if err == nil && status == http.StatusUnauthorized {
		c.auth.Invalidate()
		return c.requestWithRetry(ctx, method, reqURL, extraHeaders, body, true)
	}
	return status, respBody, err
}

func (c *Client) requestWithRetry(
	ctx context.Context,
	method string,
	reqURL string,
	extraHeaders map[string]string,
	body []byte,
	withAuth bool,
) (int, []byte, error) {
	var lastErr error
	for attempt := 1; attempt <= c.retry.MaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}

		var token string
		if withAuth {
			var err error
			token, err = c.auth.AccessToken(ctx)
			if err != nil {
				return 0, nil, err
			}
		}

		var bodyReader io.Reader
		if len(body) > 0 {
			bodyReader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
		if err != nil {
			return 0, nil, err
		}
		if withAuth {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for k, v := range extraHeaders {
			req.Header.Set(k, v)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil || attempt >= c.retry.MaxAttempts {
				return 0, nil, lastErr
			}
			delay := c.backoffDelay(attempt, nil)
			if sleepErr := c.retry.Sleep(ctx, delay); sleepErr != nil {
				return 0, nil, sleepErr
			}
			continue
		}

		respBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			if attempt >= c.retry.MaxAttempts {
				return 0, nil, lastErr
			}
			delay := c.backoffDelay(attempt, resp.Header)
			if sleepErr := c.retry.Sleep(ctx, delay); sleepErr != nil {
				return 0, nil, sleepErr
			}
			continue
		}

		if !isRetryableResponse(resp.StatusCode, respBody) || attempt >= c.retry.MaxAttempts {
			return resp.StatusCode, respBody, nil
		}

		lastErr = &APIError{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("Drive API transient error (%d)", resp.StatusCode),
		}
		delay := c.backoffDelay(attempt, resp.Header)
		if sleepErr := c.retry.Sleep(ctx, delay); sleepErr != nil {
			return 0, nil, sleepErr
		}
	}

	if lastErr != nil {
		return 0, nil, lastErr
	}
	return 0, nil, errors.New("Drive request failed after retries")
}

func (c *Client) backoffDelay(attempt int, headers http.Header) time.Duration {
	if headers != nil {
		if d, ok := parseRetryAfter(headers.Get("Retry-After"), time.Now()); ok {
			if d > c.retry.MaxDelay {
				return c.retry.MaxDelay
			}
			return d
		}
	}
	exp := c.retry.BaseDelay
	for i := 1; i < attempt; i++ {
		if exp >= c.retry.MaxDelay/2 {
			exp = c.retry.MaxDelay
			break
		}
		exp *= 2
	}
	jitter := c.retry.Jitter(c.retry.BaseDelay)
	delay := exp + jitter
	if delay > c.retry.MaxDelay {
		return c.retry.MaxDelay
	}
	return delay
}

func parseRetryAfter(raw string, now time.Time) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(raw, 64); err == nil {
		if secs <= 0 {
			return 0, true
		}
		return time.Duration(secs * float64(time.Second)), true
	}
	if when, err := http.ParseTime(raw); err == nil {
		d := when.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

type driveErrorEnvelope struct {
	Error struct {
		Errors []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	} `json:"error"`
}

func isRetryableResponse(status int, body []byte) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	case http.StatusForbidden:
		var env driveErrorEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			return false
		}
		for _, item := range env.Error.Errors {
			if item.Reason == "rateLimitExceeded" || item.Reason == "userRateLimitExceeded" {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func checkStatus(status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}
	return &APIError{
		StatusCode: status,
		Message:    fmt.Sprintf("Drive API error (%d): %s", status, strings.TrimSpace(string(body))),
	}
}

func splitFolderPath(folderPath string) []string {
	parts := strings.FieldsFunc(folderPath, func(r rune) bool {
		return r == '/' || r == '\\'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func escapeQueryLiteral(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `'`, `\'`)
}

// ContentMimeType infers an upload MIME type from filename when none is provided.
func ContentMimeType(name string) string {
	switch strings.ToLower(path.Ext(strings.TrimSpace(name))) {
	case ".md":
		return "text/markdown"
	case ".txt":
		return "text/plain"
	case ".json":
		return "application/json"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".pdf":
		return "application/pdf"
	default:
		return "application/octet-stream"
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
