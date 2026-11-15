//go:build unix

package fsops

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
)

func TestFIFORefused(t *testing.T) {
	f, dir := newFS(t)
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Skip(err)
	}
	if _, err := f.Read("pipe", ReadOptions{}); !errors.Is(err, ErrRefused) {
		t.Errorf("read: %v", err)
	}
	if _, err := f.Write("pipe", "x"); !errors.Is(err, ErrRefused) {
		t.Errorf("write: %v", err)
	}
	if _, err := f.Edit("pipe", EditOp{OldString: "a", NewString: "b"}); !errors.Is(err, ErrRefused) {
		t.Errorf("edit: %v", err)
	}
	if in, err := f.Stat("pipe"); err != nil || in.Type != "other" {
		t.Errorf("stat: %+v %v", in, err)
	}
	if _, err := f.Read("/dev/null", ReadOptions{}); err == nil {
		t.Error("device read accepted")
	}
}
