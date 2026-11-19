// Package gdrive implements VaultShare V2's pure-Go Google Drive v3 client,
// OAuth 2.0 token manager (backed by Obsidian SecretStorage via SecretPort),
// and concurrency-capped http.RoundTripper transport.
//
// It contains zero syscall/js imports so retries, rate limiting, resumable
// uploads, idempotent file creation, and changes.list paging can be tested
// natively under go test ./go/gdrive/...
package gdrive
