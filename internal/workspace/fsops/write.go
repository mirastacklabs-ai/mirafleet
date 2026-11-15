package fsops

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

const flagCreateExcl = os.O_WRONLY | os.O_CREATE | os.O_EXCL

// WriteResult describes a completed write.
type WriteResult struct {
	Path    string `json:"path"`
	Created bool   `json:"created"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
}

// Write creates or replaces a file atomically (temporary file in the same directory, then
// rename). Parent directories are created. The mode of an existing file is kept; a new file is
// 0644.
func (f *FS) Write(path, content string) (*WriteResult, error) {
	rel, err := f.rel(path)
	if err != nil {
		return nil, err
	}
	rel, err = f.target(rel)
	if err != nil {
		return nil, err
	}
	return f.put(rel, []byte(content))
}

// put writes data to rel (already symlink-resolved).
func (f *FS) put(rel string, data []byte) (*WriteResult, error) {
	if int64(len(data)) > f.opt.MaxFile {
		return nil, fail(ErrTooLarge, "content is %d bytes, larger than the %d byte limit", len(data), f.opt.MaxFile)
	}
	mode := fs.FileMode(defaultFileMode)
	created := true
	switch fi, err := f.root.Stat(rel); {
	case err == nil:
		if fi.IsDir() {
			return nil, fail(ErrInvalid, "EISDIR: illegal operation on a directory, open '%s'", rel)
		}
		if !fi.Mode().IsRegular() {
			return nil, fail(ErrRefused, "%s is not a regular file (FIFOs, devices and sockets are refused)", rel)
		}
		mode, created = fi.Mode().Perm(), false
	case !errors.Is(err, fs.ErrNotExist):
		return nil, mapErr(rel, err)
	}
	dir := filepath.Dir(rel)
	if dir != "." {
		if err := f.root.MkdirAll(dir, defaultDirMode); err != nil {
			return nil, mapErr(dir, err)
		}
	}
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, err
	}
	tmp := filepath.Join(dir, ".mirafleet-tmp-"+hex.EncodeToString(rnd[:]))
	tf, err := f.root.OpenFile(tmp, flagCreateExcl, 0o600)
	if err != nil {
		return nil, mapErr(rel, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = tf.Close()
			_ = f.root.Remove(tmp)
		}
	}()
	if _, err := tf.Write(data); err != nil {
		return nil, mapErr(rel, err)
	}
	if err := tf.Chmod(mode); err != nil {
		return nil, mapErr(rel, err)
	}
	if err := tf.Sync(); err != nil {
		return nil, mapErr(rel, err)
	}
	if err := tf.Close(); err != nil {
		return nil, mapErr(rel, err)
	}
	if err := f.root.Rename(tmp, rel); err != nil {
		return nil, mapErr(rel, err)
	}
	ok = true
	sum := sha256.Sum256(data)
	return &WriteResult{Path: rel, Created: created, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}, nil
}
