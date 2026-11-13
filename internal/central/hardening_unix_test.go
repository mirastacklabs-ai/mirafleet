//go:build unix

package central

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// reqFrame builds one framed request payload.
func reqFrame(t *testing.T, method string, params any) []byte {
	t.Helper()
	req := api.Request{V: api.Version, ID: "t1", Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		must(t, err)
		req.Params = b
	}
	b, err := json.Marshal(req)
	must(t, err)
	return b
}

func (e *env) hasActionRow(outcome string) bool {
	for _, ev := range e.auditRows() {
		if ev.Action == api.MethodActionRun && ev.Outcome == outcome {
			return true
		}
	}
	return false
}

// D-05: a peer running as another UID is dropped before anything is read from it.
func TestAdminForeignUIDRejectedBeforeAnyRead(t *testing.T) {
	e := newEnv(t, "r1")
	sock := adminSocket(t, e)

	old := setPeerUIDFunc(func(*net.UnixConn) (uint32, error) { return uint32(os.Getuid() + 1), nil })
	t.Cleanup(func() { setPeerUIDFunc(old) })

	c, err := net.Dial("unix", sock)
	must(t, err)
	defer c.Close()
	// A header announcing an oversized request: had the server read it, it would answer
	// too_large. A rejected peer gets no byte at all.
	hdr := []byte{0x7f, 0xff, 0xff, 0xff}
	c.Write(hdr)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := c.Read(make([]byte, 64))
	if n != 0 {
		t.Fatalf("foreign peer received %d bytes", n)
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("foreign peer connection was left open")
	}
	waitFor(t, func() bool { return strings.Contains(e.logText(), "admin socket: rejected connection: peer uid") })
	// And a well-formed request from a foreign peer is not served either.
	c2, err := net.Dial("unix", sock)
	must(t, err)
	defer c2.Close()
	must(t, api.WriteFrame(c2, reqFrame(t, api.MethodFleetsList, nil)))
	c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := api.ReadFrame(c2, api.MaxResponseBytes); err == nil {
		t.Fatal("foreign peer got a response")
	}
}

// D-05/D-06: the client disconnecting cancels an admin action.run; a half-close does not.
func TestAdminClientDisconnectCancelsActionRun(t *testing.T) {
	e := newEnv(t, "r1")
	e.startRunner("r1")
	e.waitOnline("r1")
	sock := adminSocket(t, e)
	c, err := net.Dial("unix", sock)
	must(t, err)
	must(t, api.WriteFrame(c, reqFrame(t, api.MethodActionRun, api.ActionRunParams{Fleet: testFleet, Runner: "r1", Action: "long"})))
	waitFor(t, func() bool { return e.hasActionRow(service.OutcomeStart) })
	c.Close()
	waitFor(t, func() bool { return e.hasActionRow(service.OutcomeCancelled) })
}

func TestAdminHalfCloseDoesNotCancel(t *testing.T) {
	e := newEnv(t, "r1")
	e.startRunner("r1")
	e.waitOnline("r1")
	sock := adminSocket(t, e)
	c, err := net.Dial("unix", sock)
	must(t, err)
	defer c.Close()
	// 1 s action, then half-close right after the request.
	must(t, api.WriteFrame(c, reqFrame(t, api.MethodActionRun, api.ActionRunParams{Fleet: testFleet, Runner: "r1", Action: "long", TimeoutS: 1})))
	must(t, c.(*net.UnixConn).CloseWrite())
	c.SetReadDeadline(time.Now().Add(15 * time.Second))
	b, err := api.ReadFrame(c, api.MaxResponseBytes)
	if err != nil {
		t.Fatalf("half-closed request got no response: %v", err)
	}
	if e.hasActionRow(service.OutcomeCancelled) {
		t.Fatalf("half-close cancelled the action; response: %s", b)
	}
}

// D-06 on api@: CloseWrite after the request still gets the response; only a full close cancels.
func TestAPIHalfCloseDoesNotCancel(t *testing.T) {
	e := newEnv(t, "r1")
	e.addOperator("op")
	e.startRunner("r1")
	e.waitOnline("r1")
	cl, err := e.dial(transport.UserOperator, "op")
	must(t, err)
	defer cl.Close()
	ch, reqs, err := cl.OpenChannel(transport.ChanAPI, nil)
	must(t, err)
	go ssh.DiscardRequests(reqs)
	defer ch.Close()
	must(t, api.WriteFrame(ch, reqFrame(t, api.MethodActionRun, api.ActionRunParams{Fleet: testFleet, Runner: "r1", Action: "long", TimeoutS: 1})))
	must(t, ch.CloseWrite())
	done := make(chan []byte, 1)
	go func() {
		b, err := api.ReadFrame(ch, api.MaxResponseBytes)
		if err != nil {
			b = []byte("ERR " + err.Error())
		}
		done <- b
	}()
	select {
	case b := <-done:
		if strings.HasPrefix(string(b), "ERR") {
			t.Fatalf("half-closed request got no response: %s", b)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no response to a half-closed request")
	}
	if e.hasActionRow(service.OutcomeCancelled) {
		t.Fatal("half-close cancelled the action")
	}
}

func TestAPIFullChannelCloseCancelsAction(t *testing.T) {
	e := newEnv(t, "r1")
	e.addOperator("op")
	e.startRunner("r1")
	e.waitOnline("r1")
	cl, err := e.dial(transport.UserOperator, "op")
	must(t, err)
	defer cl.Close()
	ch, reqs, err := cl.OpenChannel(transport.ChanAPI, nil)
	must(t, err)
	go ssh.DiscardRequests(reqs)
	must(t, api.WriteFrame(ch, reqFrame(t, api.MethodActionRun, api.ActionRunParams{Fleet: testFleet, Runner: "r1", Action: "long"})))
	waitFor(t, func() bool { return e.hasActionRow(service.OutcomeStart) })
	ch.Close()
	waitFor(t, func() bool { return e.hasActionRow(service.OutcomeCancelled) })
}

// D-10: Close does not wait for the admin socket's context.
func TestCloseStopsAdminSocket(t *testing.T) {
	e := newEnv(t)
	sock := adminSocket(t, e) // its ctx is cancelled only at cleanup
	start := time.Now()
	e.srv.Close()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Close took %v", d)
	}
	if _, err := net.DialTimeout("unix", sock, time.Second); err == nil {
		t.Fatal("admin socket still accepting after Close")
	}
	if err := e.srv.ServeAdmin(context.Background(), sock+"2"); err == nil {
		t.Fatal("ServeAdmin on a closed server must fail")
	}
}
