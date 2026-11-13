package central

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// S10: a heartbeat failing Validate is refused and the connection is closed, whether it is the
// first one or a later one; a valid one carries its workspace fields into the snapshot.
func TestInvalidHeartbeatClosesConnection(t *testing.T) {
	for _, first := range []bool{true, false} {
		e := newEnv(t, "r1")
		cl, err := e.dial(transport.UserRunner, "r1")
		must(t, err)
		defer cl.Close()
		send := func(hb transport.Heartbeat) bool {
			b, _ := json.Marshal(hb)
			ok, _, err := cl.SendRequest(transport.ReqHeartbeat, true, b)
			return err == nil && ok
		}
		if !first {
			if !send(transport.Heartbeat{Hostname: "h", Slots: 3, Executor: "containerd", SecureRuntime: "gvisor", Images: []string{"a:*"}}) {
				t.Fatal("valid heartbeat refused")
			}
			e.waitOnline("r1")
			var found bool
			for _, si := range e.reg.Snapshot() {
				if si.Slots == 3 && si.Executor == "containerd" && si.SecureRuntime == "gvisor" && len(si.Images) == 1 {
					found = true
				}
			}
			if !found {
				t.Fatalf("snapshot lacks heartbeat fields: %+v", e.reg.Snapshot())
			}
		}
		if send(transport.Heartbeat{Hostname: "h", Slots: -1}) {
			t.Fatal("invalid heartbeat accepted")
		}
		closed := make(chan struct{})
		go func() { cl.Wait(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("central did not close the connection")
		}
	}
}
