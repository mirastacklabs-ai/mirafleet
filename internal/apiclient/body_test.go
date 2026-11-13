package apiclient

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
)

func TestCallBodySendsRequestThenBody(t *testing.T) {
	f := startFake(t, func(_ string, ch ssh.Channel, req *api.Request) {
		switch req.Method {
		case api.MethodWorkspaceWriteFile:
			b, err := api.ReadBodyTimeout(ch, ch, api.MaxFileChunk)
			if err != nil {
				t.Errorf("server body: %v", err)
				return
			}
			reply(t, ch, req, api.WorkspaceWriteFileResult{Size: int64(len(b))})
		case api.MethodWorkspaceExec: // refused without reading the body
			api.WriteResponse(ch, api.NewError(req.ID, api.Errorf(api.CodeNotFound, "not found")))
		}
	})
	c := dialFake(t, f)
	ctx := context.Background()

	r, err := c.WorkspaceWriteFile(ctx, api.WorkspaceWriteFileParams{Workspace: "w", Path: "p", Length: 5}, []byte("hello"))
	if err != nil || r.Size != 5 {
		t.Fatalf("write: %+v %v", r, err)
	}
	big := bytes.Repeat([]byte{1}, api.MaxFileChunk)
	if r, err = c.WorkspaceWriteFile(ctx, api.WorkspaceWriteFileParams{Workspace: "w"}, big); err != nil || r.Size != int64(len(big)) {
		t.Fatalf("max body: %+v %v", r, err)
	}
	if r, err = c.WorkspaceWriteFile(ctx, api.WorkspaceWriteFileParams{Workspace: "w"}, nil); err != nil || r.Size != 0 {
		t.Fatalf("empty body: %+v %v", r, err)
	}
	// Over the limit: refused locally, nothing sent.
	n := f.channels.Load()
	if _, err = c.WorkspaceWriteFile(ctx, api.WorkspaceWriteFileParams{Workspace: "w"}, make([]byte, api.MaxFileChunk+1)); err == nil {
		t.Fatal("oversize body accepted")
	}
	if f.channels.Load() != n {
		t.Fatal("a channel was opened for an oversize body")
	}
	// The server refuses without reading the 1 MiB body: the call returns the API error.
	_, err = c.WorkspaceExec(ctx, api.WorkspaceExecParams{Workspace: "w", Argv: []string{"x"}, Stdin: true}, big)
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.CodeNotFound {
		t.Fatalf("refused call: %v", err)
	}
}

func TestCallReadBody(t *testing.T) {
	data := bytes.Repeat([]byte("ab"), 1000)
	f := startFake(t, func(_ string, ch ssh.Channel, req *api.Request) {
		var p api.WorkspaceReadFileParams
		api.DecodeParams(req.Params, &p)
		switch p.Path {
		case "missing":
			api.WriteResponse(ch, api.NewError(req.ID, api.Errorf(api.CodeNotFound, "nope")))
		case "empty":
			reply(t, ch, req, api.WorkspaceReadFileResult{EOF: true})
			api.WriteBody(ch, nil)
		case "huge": // announces more than a chunk
			reply(t, ch, req, api.WorkspaceReadFileResult{})
			var hdr [4]byte
			binary.BigEndian.PutUint32(hdr[:], api.MaxFileChunk+1)
			ch.Write(hdr[:])
		case "short": // result but no body at all
			reply(t, ch, req, api.WorkspaceReadFileResult{})
		default:
			reply(t, ch, req, api.WorkspaceReadFileResult{Size: int64(len(data)), Length: len(data), EOF: true})
			api.WriteBody(ch, data)
		}
	})
	c := dialFake(t, f)
	ctx := context.Background()

	r, b, err := c.WorkspaceReadFile(ctx, api.WorkspaceReadFileParams{Workspace: "w", Path: "a"})
	if err != nil || !bytes.Equal(b, data) || r.Length != len(data) || !r.EOF {
		t.Fatalf("read: %+v %d %v", r, len(b), err)
	}
	if r, b, err = c.WorkspaceReadFile(ctx, api.WorkspaceReadFileParams{Workspace: "w", Path: "empty"}); err != nil || len(b) != 0 || !r.EOF {
		t.Fatalf("empty: %+v %d %v", r, len(b), err)
	}
	_, b, err = c.WorkspaceReadFile(ctx, api.WorkspaceReadFileParams{Workspace: "w", Path: "missing"})
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.CodeNotFound || b != nil {
		t.Fatalf("missing: %v", err)
	}
	if _, _, err = c.WorkspaceReadFile(ctx, api.WorkspaceReadFileParams{Workspace: "w", Path: "huge"}); err == nil {
		t.Fatal("oversize response body accepted")
	}
	if _, _, err = c.WorkspaceReadFile(ctx, api.WorkspaceReadFileParams{Workspace: "w", Path: "short"}); err == nil {
		t.Fatal("missing response body accepted")
	}
}

func TestCallBodyCancelClosesChannel(t *testing.T) {
	gotBody := make(chan struct{})
	closed := make(chan struct{})
	f := startFake(t, func(_ string, ch ssh.Channel, req *api.Request) {
		api.ReadBodyTimeout(ch, ch, api.MaxFileChunk)
		close(gotBody)
		buf := make([]byte, 1)
		for { // blocks until the client closes the channel
			if _, err := ch.Read(buf); err != nil {
				close(closed)
				return
			}
		}
	})
	c := dialFake(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- c.CallBody(ctx, api.MethodWorkspaceWriteFile, api.WorkspaceWriteFileParams{Workspace: "w"}, []byte("x"), nil)
	}()
	<-gotBody
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CallBody did not return on cancel")
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("channel not closed on cancel")
	}
}
