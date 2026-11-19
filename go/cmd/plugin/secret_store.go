//go:build js && wasm

package main

import (
	"context"
	"sync"
)

// jsSecretPort reads and writes the vault-scoped OAuth refresh token directly
// in Obsidian's app.secretStorage using the injected app handle.
type jsSecretPort struct {
	mu        sync.Mutex
	vaultName string
}

func newJSSecretPort(vaultName string) *jsSecretPort {
	return &jsSecretPort{vaultName: vaultName}
}

func (s *jsSecretPort) SetVaultName(name string) {
	s.mu.Lock()
	s.vaultName = name
	s.mu.Unlock()
}

func (s *jsSecretPort) secretKey() string {
	s.mu.Lock()
	name := s.vaultName
	s.mu.Unlock()
	return "vaultshare-" + slugifyVaultName(name) + "-refresh-token"
}

func (s *jsSecretPort) LoadRefreshToken(_ context.Context) (string, error) {
	h, err := getHostHandles()
	if err != nil {
		return "", err
	}
	if h.secretStorage.IsUndefined() || h.secretStorage.IsNull() {
		return "", nil
	}
	val := h.secretStorage.Call("getSecret", s.secretKey())
	if val.IsUndefined() || val.IsNull() {
		return "", nil
	}
	return val.String(), nil
}

func (s *jsSecretPort) SaveRefreshToken(_ context.Context, token string) error {
	h, err := getHostHandles()
	if err != nil {
		return err
	}
	if h.secretStorage.IsUndefined() || h.secretStorage.IsNull() {
		return nil
	}
	h.secretStorage.Call("setSecret", s.secretKey(), token)
	return nil
}
