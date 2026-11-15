package central

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// F-16: an invalid Actions list is dropped (and logged), the connection stays open and the other
// heartbeat fields are kept. An invalid non-actions field still closes the connection.
func TestInvalidActionsHeartbeatKeepsConnection(t *testing.T) {
	e := newEnv(t, "r1")
	cl, err := e.dial(transport.UserRunner, "r1")
	must(t, err)
	defer cl.Close()
	send := func(hb transport.Heartbeat) bool {
		b, _ := json.Marshal(hb)
		ok, _, err := cl.SendRequest(transport.ReqHeartbeat, true, b)
		return err == nil && ok
	}
	bad := []transport.ActionInfo{{Name: "ok"}, {Name: ""}}
	for i := 0; i < 2; i++ { // as the first heartbeat, then as a later one
		if !send(transport.Heartbeat{Hostname: "h", Proto: testProto, Slots: 3, Actions: bad}) {
			t.Fatalf("heartbeat %d with an invalid actions list was refused", i)
		}
		e.waitOnline("r1")
	}
	if n := strings.Count(e.logText(), "invalid actions list"); n != 1 {
		t.Fatalf("invalid actions list logged %d times over 2 heartbeats, want once per connection", n)
	}
	for _, si := range e.reg.Snapshot() {
		if si.Name == "r1" && (si.Slots != 3 || len(si.Info.Actions) != 0) {
			t.Fatalf("snapshot: slots %d actions %v, want 3 and none", si.Slots, si.Info.Actions)
		}
	}
	closed := make(chan struct{})
	go func() { cl.Wait(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("central closed the connection over an invalid actions list")
	case <-time.After(300 * time.Millisecond):
	}
	// Another invalid field, even with a bad actions list, still closes it.
	if send(transport.Heartbeat{Hostname: "h", Proto: testProto, Slots: -1, Actions: bad}) {
		t.Fatal("heartbeat with invalid slots accepted")
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("central did not close the connection")
	}
}
