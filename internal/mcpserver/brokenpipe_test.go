package mcpserver

import (
	"context"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

type epipeWriter struct{}

func (epipeWriter) Write([]byte) (int, error) {
	return 0, &os.PathError{Op: "write", Path: "/dev/stdout", Err: syscall.EPIPE}
}

// A closed stdout (EPIPE) is a clean end, not an error.
func TestServeBrokenStdoutIsClean(t *testing.T) {
	inR, inW := io.Pipe()
	defer inW.Close()
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), inR, epipeWriter{}, Config{Name: "m", Tools: []Tool{pingTool()}})
	}()
	if _, err := io.WriteString(inW, initLine+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop after a broken stdout")
	}
}
