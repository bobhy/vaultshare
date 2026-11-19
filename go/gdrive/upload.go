package gdrive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	multipartUploadLimit      = 5 * 1024 * 1024 // 5 MiB
	defaultResumableChunkSize = 4 * 1024 * 1024 // 4 MiB (multiple of 256 KiB)
	resumableSessionRestarts  = 2
)

var errSessionExpired = errors.New("gdrive: resumable upload session expired")

type generateIDsResponse struct {
	IDs []string `json:"ids"`
}

func (c *Client) generateFileID(ctx context.Context) (string, error) {
	reqURL := fmt.Sprintf("%s/files/generateIds?count=1&space=drive&type=files", c.apiBase)
	status, body, err := c.doAuthenticated(ctx, http.MethodGet, reqURL, nil, nil)
	if err != nil {
		return "", err
	}
	if err := checkStatus(status, body); err != nil {
		return "", err
	}
	var resp generateIDsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parse generateIds: %w", err)
	}
	if len(resp.IDs) == 0 || resp.IDs[0] == "" {
		return "", errors.New("gdrive: generateIds returned empty list")
	}
	return resp.IDs[0], nil
}

// CreateFile uploads a new file inside parentID using a pre-generated ID and
// per-(parentID, name) single-flight locking to avoid duplicate creation.
func (c *Client) CreateFile(
	ctx context.Context,
	parentID, name, mimeType string,
	mtimeMs int64,
	data []byte,
) (DriveFile, error) {
	if mimeType == "" {
		mimeType = ContentMimeType(name)
	}

	unlock := c.lockCreate(parentID, name)
	defer unlock()

	existing, err := c.findChildren(ctx, parentID, name, false)
	if err != nil {
		return DriveFile{}, err
	}
	if len(existing) > 0 {
		return DriveFile{}, ErrAlreadyExists
	}

	// Generate ID once before any upload/retry so 409 Conflict can prove idempotency.
	generatedID, err := c.generateFileID(ctx)
	if err != nil {
		return DriveFile{}, err
	}

	meta := map[string]any{
		"id":       generatedID,
		"name":     name,
		"parents":  []string{parentID},
		"mimeType": mimeType,
	}
	if mtimeMs > 0 {
		meta["modifiedTime"] = formatDriveTime(mtimeMs)
	}

	var status int
	var respBody []byte
	var upErr error
	if len(data) <= multipartUploadLimit {
		reqURL := fmt.Sprintf("%s/files?uploadType=multipart&fields=%s", c.uploadBase, url.QueryEscape(fileFields))
		status, respBody, upErr = c.uploadMultipart(ctx, http.MethodPost, reqURL, meta, mimeType, data)
	} else {
		reqURL := fmt.Sprintf("%s/files?uploadType=resumable&fields=%s", c.uploadBase, url.QueryEscape(fileFields))
		status, respBody, upErr = c.uploadResumable(ctx, http.MethodPost, reqURL, meta, mimeType, data)
	}

	if upErr != nil {
		return DriveFile{}, upErr
	}
	if status == http.StatusConflict {
		got, found, getErr := c.GetFileMeta(ctx, generatedID)
		if getErr == nil && found && !got.Trashed && got.ByteSize() == int64(len(data)) {
			return got, nil
		}
	}
	if err := checkStatus(status, respBody); err != nil {
		return DriveFile{}, err
	}
	var created DriveFile
	if err := json.Unmarshal(respBody, &created); err != nil {
		return DriveFile{}, fmt.Errorf("parse created Drive file: %w", err)
	}
	return created, nil
}

// UpdateFile overwrites an existing Drive file's content and modifiedTime.
func (c *Client) UpdateFile(
	ctx context.Context,
	fileID, mimeType string,
	mtimeMs int64,
	data []byte,
) (DriveFile, error) {
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	meta := map[string]any{}
	if mtimeMs > 0 {
		meta["modifiedTime"] = formatDriveTime(mtimeMs)
	}

	var status int
	var respBody []byte
	var err error
	if len(data) <= multipartUploadLimit {
		reqURL := fmt.Sprintf("%s/files/%s?uploadType=multipart&fields=%s", c.uploadBase, url.PathEscape(fileID), url.QueryEscape(fileFields))
		status, respBody, err = c.uploadMultipart(ctx, http.MethodPatch, reqURL, meta, mimeType, data)
	} else {
		reqURL := fmt.Sprintf("%s/files/%s?uploadType=resumable&fields=%s", c.uploadBase, url.PathEscape(fileID), url.QueryEscape(fileFields))
		status, respBody, err = c.uploadResumable(ctx, http.MethodPatch, reqURL, meta, mimeType, data)
	}
	if err != nil {
		return DriveFile{}, err
	}
	if err := checkStatus(status, respBody); err != nil {
		return DriveFile{}, err
	}
	var updated DriveFile
	if err := json.Unmarshal(respBody, &updated); err != nil {
		return DriveFile{}, fmt.Errorf("parse updated Drive file: %w", err)
	}
	return updated, nil
}

func (c *Client) uploadMultipart(
	ctx context.Context,
	method, reqURL string,
	meta map[string]any,
	mimeType string,
	data []byte,
) (int, []byte, error) {
	body, contentType, err := buildMultipartRelated(meta, data, mimeType)
	if err != nil {
		return 0, nil, err
	}
	return c.doAuthenticated(ctx, method, reqURL, map[string]string{"Content-Type": contentType}, body)
}

func (c *Client) uploadResumable(
	ctx context.Context,
	method, initURL string,
	meta map[string]any,
	mimeType string,
	data []byte,
) (int, []byte, error) {
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return 0, nil, err
	}
	chunkSize := c.chunkSize
	if chunkSize <= 0 {
		chunkSize = defaultResumableChunkSize
	}

	for range resumableSessionRestarts {
		sessionURI, status, body, err := c.initResumableSession(ctx, method, initURL, metaBytes, mimeType, len(data))
		if err != nil {
			return 0, nil, err
		}
		if status == http.StatusConflict || status >= 400 {
			return status, body, nil
		}
		status, body, err = c.streamResumableChunks(ctx, sessionURI, data, chunkSize)
		if errors.Is(err, errSessionExpired) {
			continue
		}
		return status, body, err
	}
	return 0, nil, errSessionExpired
}

func (c *Client) initResumableSession(
	ctx context.Context,
	method, initURL string,
	metaBytes []byte,
	mimeType string,
	totalLen int,
) (string, int, []byte, error) {
	for attempt := 1; attempt <= c.retry.MaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return "", 0, nil, ctx.Err()
		}
		token, err := c.auth.AccessToken(ctx)
		if err != nil {
			return "", 0, nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, initURL, bytes.NewReader(metaBytes))
		if err != nil {
			return "", 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json; charset=UTF-8")
		req.Header.Set("X-Upload-Content-Type", mimeType)
		req.Header.Set("X-Upload-Content-Length", strconv.Itoa(totalLen))

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil || attempt >= c.retry.MaxAttempts {
				return "", 0, nil, err
			}
			if sleepErr := c.retry.Sleep(ctx, c.backoffDelay(attempt, nil)); sleepErr != nil {
				return "", 0, nil, sleepErr
			}
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusUnauthorized && attempt == 1 {
			c.auth.Invalidate()
			continue
		}
		if isRetryableResponse(resp.StatusCode, body) && attempt < c.retry.MaxAttempts {
			if sleepErr := c.retry.Sleep(ctx, c.backoffDelay(attempt, resp.Header)); sleepErr != nil {
				return "", 0, nil, sleepErr
			}
			continue
		}
		if resp.StatusCode >= 400 {
			return "", resp.StatusCode, body, nil
		}
		loc := resp.Header.Get("Location")
		if loc == "" {
			return "", resp.StatusCode, body, errors.New("gdrive: resumable init missing Location header")
		}
		return loc, resp.StatusCode, body, nil
	}
	return "", 0, nil, errors.New("gdrive: resumable init failed after retries")
}

func (c *Client) streamResumableChunks(ctx context.Context, sessionURI string, data []byte, chunkSize int) (int, []byte, error) {
	total := len(data)
	offset := 0
	for {
		end := offset + chunkSize
		if end > total {
			end = total
		}
		chunk := data[offset:end]
		hdrs := map[string]string{
			"Content-Range": fmt.Sprintf("bytes %d-%d/%d", offset, end-1, total),
		}
		if total == 0 {
			hdrs["Content-Range"] = "bytes */0"
		}

		// Pre-authorized session URI: omit Authorization header on chunk PUTs.
		status, body, respHdrs, err := c.doPutWithHeaders(ctx, sessionURI, hdrs, chunk)
		if err != nil {
			nextOffset, ok, qErr := c.queryResumableOffset(ctx, sessionURI, total)
			if errors.Is(qErr, errSessionExpired) {
				return 0, nil, errSessionExpired
			}
			if ok && nextOffset > offset {
				offset = nextOffset
				continue
			}
			return 0, nil, err
		}
		if status == http.StatusNotFound || status == http.StatusGone {
			return 0, nil, errSessionExpired
		}
		if status == http.StatusPermanentRedirect {
			if nextOffset, ok := parseRangeNextOffset(respHdrs.Get("Range")); ok {
				offset = nextOffset
			} else {
				offset = end
			}
			continue
		}
		if status >= 200 && status < 300 {
			return status, body, nil
		}
		return status, body, nil
	}
}

func (c *Client) doPutWithHeaders(
	ctx context.Context,
	reqURL string,
	headers map[string]string,
	body []byte,
) (int, []byte, http.Header, error) {
	var lastErr error
	for attempt := 1; attempt <= c.retry.MaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return 0, nil, nil, ctx.Err()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, reqURL, bytes.NewReader(body))
		if err != nil {
			return 0, nil, nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil || attempt >= c.retry.MaxAttempts {
				return 0, nil, nil, lastErr
			}
			if sleepErr := c.retry.Sleep(ctx, c.backoffDelay(attempt, nil)); sleepErr != nil {
				return 0, nil, nil, sleepErr
			}
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if isRetryableResponse(resp.StatusCode, respBody) && attempt < c.retry.MaxAttempts {
			lastErr = &APIError{StatusCode: resp.StatusCode, Message: fmt.Sprintf("Drive upload transient error (%d)", resp.StatusCode)}
			if sleepErr := c.retry.Sleep(ctx, c.backoffDelay(attempt, resp.Header)); sleepErr != nil {
				return 0, nil, nil, sleepErr
			}
			continue
		}
		return resp.StatusCode, respBody, resp.Header, nil
	}
	return 0, nil, nil, lastErr
}

func (c *Client) queryResumableOffset(ctx context.Context, sessionURI string, total int) (int, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, sessionURI, nil)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("Content-Range", fmt.Sprintf("bytes */%d", total))
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, false, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return 0, false, errSessionExpired
	}
	if resp.StatusCode == http.StatusPermanentRedirect {
		next, ok := parseRangeNextOffset(resp.Header.Get("Range"))
		return next, ok, nil
	}
	return 0, false, nil
}

func parseRangeNextOffset(rangeHeader string) (int, bool) {
	// Drive returns e.g. "bytes=0-1023".
	idx := strings.LastIndexByte(rangeHeader, '-')
	if idx < 0 {
		return 0, false
	}
	end, err := strconv.Atoi(strings.TrimSpace(rangeHeader[idx+1:]))
	if err != nil || end < 0 {
		return 0, false
	}
	return end + 1, true
}

func buildMultipartRelated(meta map[string]any, data []byte, mimeType string) ([]byte, string, error) {
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return nil, "", err
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	metaHeader := textproto.MIMEHeader{}
	metaHeader.Set("Content-Type", "application/json; charset=UTF-8")
	metaPart, err := mw.CreatePart(metaHeader)
	if err != nil {
		return nil, "", err
	}
	if _, err := metaPart.Write(metaBytes); err != nil {
		return nil, "", err
	}

	mediaHeader := textproto.MIMEHeader{}
	mediaHeader.Set("Content-Type", mimeType)
	mediaPart, err := mw.CreatePart(mediaHeader)
	if err != nil {
		return nil, "", err
	}
	if _, err := mediaPart.Write(data); err != nil {
		return nil, "", err
	}

	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), "multipart/related; boundary=" + mw.Boundary(), nil
}

func formatDriveTime(mtimeMs int64) string {
	return time.UnixMilli(mtimeMs).UTC().Format(time.RFC3339Nano)
}
