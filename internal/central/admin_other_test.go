//go:build !unix

package central

import (
	"context"
	"path/filepath"
	"testing"
)

func TestAdminFailsClosedOnNonUnix(t *testing.T) {
	s := New(Config{Logf: t.Logf})
	if err := s.ServeAdmin(context.Background(), filepath.Join(t.TempDir(), "admin.sock")); err == nil {
		t.Fatal("ServeAdmin must fail on non-unix")
	}
	if err := checkPeer(nil); err == nil {
		t.Fatal("checkPeer must reject on non-unix")
	}
}
