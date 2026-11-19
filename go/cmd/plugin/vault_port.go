//go:build js && wasm

package main

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"syscall/js"

	"vaultshare/go/syncengine"
)

// jsVaultPort implements syncengine.VaultPort directly over Obsidian's app.vault API
// via opaque js.Value handles and awaitPromise.
type jsVaultPort struct{}

func (jsVaultPort) ConfigDir() string {
	h, err := getHostHandles()
	if err != nil {
		return ".obsidian"
	}
	cfg := strings.TrimSpace(h.vault.Get("configDir").String())
	if cfg == "" {
		return ".obsidian"
	}
	return cfg
}

func (jsVaultPort) Stat(_ context.Context, relPath string) (syncengine.LocalStat, bool, error) {
	h, err := getHostHandles()
	if err != nil {
		return syncengine.LocalStat{}, false, err
	}
	af := h.vault.Call("getAbstractFileByPath", relPath)
	if af.IsNull() || af.IsUndefined() {
		return syncengine.LocalStat{}, false, nil
	}
	stVal := af.Get("stat")
	if !stVal.IsNull() && !stVal.IsUndefined() {
		return syncengine.LocalStat{
			Path:     relPath,
			Size:     int64(stVal.Get("size").Float()),
			MTimeMs:  int64(stVal.Get("mtime").Float()),
			IsFolder: false,
		}, true, nil
	}
	return syncengine.LocalStat{
		Path:     relPath,
		IsFolder: true,
	}, true, nil
}

func (jsVaultPort) ListAllStats(_ context.Context) ([]syncengine.LocalStat, error) {
	h, err := getHostHandles()
	if err != nil {
		return nil, err
	}
	filesVal := h.vault.Call("getFiles")
	n := filesVal.Length()
	out := make([]syncengine.LocalStat, 0, n)
	for i := 0; i < n; i++ {
		tf := filesVal.Index(i)
		p := tf.Get("path").String()
		stVal := tf.Get("stat")
		var sizeVal, mtimeVal int64
		if !stVal.IsNull() && !stVal.IsUndefined() {
			sizeVal = int64(stVal.Get("size").Float())
			mtimeVal = int64(stVal.Get("mtime").Float())
		}
		out = append(out, syncengine.LocalStat{
			Path:     p,
			Size:     sizeVal,
			MTimeMs:  mtimeVal,
			IsFolder: false,
		})
	}
	return out, nil
}

func (jsVaultPort) ReadBytes(ctx context.Context, relPath string) ([]byte, syncengine.LocalStat, error) {
	h, err := getHostHandles()
	if err != nil {
		return nil, syncengine.LocalStat{}, err
	}
	tf := h.vault.Call("getFileByPath", relPath)
	if tf.IsNull() || tf.IsUndefined() {
		return nil, syncengine.LocalStat{}, fmt.Errorf("vault file not found: %s", relPath)
	}

	abVal, err := awaitPromise(ctx, h.vault.Call("readBinary", tf))
	if err != nil {
		return nil, syncengine.LocalStat{}, err
	}
	u8 := h.uint8ArrayCtor.New(abVal)
	buf := make([]byte, u8.Get("byteLength").Int())
	if len(buf) > 0 {
		js.CopyBytesToGo(buf, u8)
	}

	stVal := tf.Get("stat")
	st := syncengine.LocalStat{
		Path: relPath,
		Size: int64(len(buf)),
	}
	if !stVal.IsNull() && !stVal.IsUndefined() {
		st.Size = int64(stVal.Get("size").Float())
		st.MTimeMs = int64(stVal.Get("mtime").Float())
	}
	return buf, st, nil
}

func (jsVaultPort) WriteText(ctx context.Context, relPath string, text string) (syncengine.LocalStat, error) {
	h, err := getHostHandles()
	if err != nil {
		return syncengine.LocalStat{}, err
	}
	tf := h.vault.Call("getFileByPath", relPath)
	if !tf.IsNull() && !tf.IsUndefined() {
		if _, err := awaitPromise(ctx, h.vault.Call("modify", tf, text)); err != nil {
			return syncengine.LocalStat{}, err
		}
	} else {
		if err := ensureLocalParentDirs(ctx, h, relPath); err != nil {
			return syncengine.LocalStat{}, err
		}
		tf, err = awaitPromise(ctx, h.vault.Call("create", relPath, text))
		if err != nil {
			return syncengine.LocalStat{}, err
		}
	}
	return statFromTFile(tf, relPath, int64(len(text))), nil
}

func (jsVaultPort) WriteBytes(ctx context.Context, relPath string, data []byte) (syncengine.LocalStat, error) {
	h, err := getHostHandles()
	if err != nil {
		return syncengine.LocalStat{}, err
	}
	u8 := h.uint8ArrayCtor.New(len(data))
	if len(data) > 0 {
		js.CopyBytesToJS(u8, data)
	}
	ab := u8.Get("buffer")

	tf := h.vault.Call("getFileByPath", relPath)
	if !tf.IsNull() && !tf.IsUndefined() {
		if _, err := awaitPromise(ctx, h.vault.Call("modifyBinary", tf, ab)); err != nil {
			return syncengine.LocalStat{}, err
		}
	} else {
		if err := ensureLocalParentDirs(ctx, h, relPath); err != nil {
			return syncengine.LocalStat{}, err
		}
		tf, err = awaitPromise(ctx, h.vault.Call("createBinary", relPath, ab))
		if err != nil {
			return syncengine.LocalStat{}, err
		}
	}
	return statFromTFile(tf, relPath, int64(len(data))), nil
}

func (jsVaultPort) Rename(ctx context.Context, oldPath, newPath string) error {
	h, err := getHostHandles()
	if err != nil {
		return err
	}
	af := h.vault.Call("getAbstractFileByPath", oldPath)
	if af.IsNull() || af.IsUndefined() {
		return fmt.Errorf("vault file not found for rename: %s", oldPath)
	}
	if err := ensureLocalParentDirs(ctx, h, newPath); err != nil {
		return err
	}
	_, err = awaitPromise(ctx, h.vault.Call("rename", af, newPath))
	return err
}

func (jsVaultPort) Trash(ctx context.Context, relPath string) error {
	h, err := getHostHandles()
	if err != nil {
		return err
	}
	af := h.vault.Call("getAbstractFileByPath", relPath)
	if af.IsNull() || af.IsUndefined() {
		return nil
	}
	_, err = awaitPromise(ctx, h.vault.Call("trash", af, false))
	return err
}

func (jsVaultPort) ExistsInTrash(ctx context.Context, relPath string, size int64) (bool, error) {
	h, err := getHostHandles()
	if err != nil {
		return false, err
	}
	adapter := h.vault.Get("adapter")
	if adapter.IsNull() || adapter.IsUndefined() {
		return false, nil
	}
	trashPath := ".trash/" + strings.TrimPrefix(relPath, "/")
	stVal, err := awaitPromise(ctx, adapter.Call("stat", trashPath))
	if err == nil && !stVal.IsNull() && !stVal.IsUndefined() {
		if size < 0 || int64(stVal.Get("size").Float()) == size {
			return true, nil
		}
	}

	// Obsidian appends " 1", " 2" on repeated trash collisions inside .trash/.
	trashDir := path.Dir(trashPath)
	base := path.Base(trashPath)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	listed, listErr := awaitPromise(ctx, adapter.Call("list", trashDir))
	if listErr != nil || listed.IsNull() || listed.IsUndefined() {
		return false, nil
	}
	filesVal := listed.Get("files")
	if filesVal.IsNull() || filesVal.IsUndefined() {
		return false, nil
	}
	for i := 0; i < filesVal.Length(); i++ {
		cand := filesVal.Index(i).String()
		candBase := path.Base(cand)
		if !strings.HasPrefix(candBase, stem+" ") || path.Ext(candBase) != ext {
			continue
		}
		if size < 0 {
			return true, nil
		}
		cst, cErr := awaitPromise(ctx, adapter.Call("stat", cand))
		if cErr == nil && !cst.IsNull() && !cst.IsUndefined() && int64(cst.Get("size").Float()) == size {
			return true, nil
		}
	}
	return false, nil
}

func (jsVaultPort) WithOpenFileLock(ctx context.Context, relPath string, fn func(context.Context) error) error {
	h, err := getHostHandles()
	if err != nil {
		return err
	}
	var cb js.Func
	var releaseOnce sync.Once
	cb = js.FuncOf(func(_ js.Value, _ []js.Value) any {
		releaseOnce.Do(func() { cb.Release() })
		return newJSPromise(func() (any, error) {
			return nil, fn(ctx)
		})
	})
	promise := h.host.Call("withOpenFileLock", relPath, cb)
	_, awaitErr := awaitPromise(ctx, promise)
	releaseOnce.Do(func() { cb.Release() })
	return awaitErr
}

func ensureLocalParentDirs(ctx context.Context, h *hostHandles, relPath string) error {
	dir := path.Dir(strings.TrimPrefix(relPath, "/"))
	if dir == "" || dir == "." {
		return nil
	}
	segs := strings.Split(dir, "/")
	cur := ""
	for _, seg := range segs {
		if seg == "" || seg == "." {
			continue
		}
		if cur == "" {
			cur = seg
		} else {
			cur += "/" + seg
		}
		af := h.vault.Call("getAbstractFileByPath", cur)
		if !af.IsNull() && !af.IsUndefined() {
			continue
		}
		if _, err := awaitPromise(ctx, h.vault.Call("createFolder", cur)); err != nil {
			if af2 := h.vault.Call("getAbstractFileByPath", cur); af2.IsNull() || af2.IsUndefined() {
				return errors.New("failed creating folder " + cur + ": " + err.Error())
			}
		}
	}
	return nil
}

func statFromTFile(tf js.Value, relPath string, fallbackSize int64) syncengine.LocalStat {
	st := syncengine.LocalStat{
		Path: relPath,
		Size: fallbackSize,
	}
	if tf.IsNull() || tf.IsUndefined() {
		return st
	}
	stVal := tf.Get("stat")
	if !stVal.IsNull() && !stVal.IsUndefined() {
		st.Size = int64(stVal.Get("size").Float())
		st.MTimeMs = int64(stVal.Get("mtime").Float())
	}
	return st
}
