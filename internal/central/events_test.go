package central

import (
	"encoding/json"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// S11: a runner may open events@ and only events@; its batches become audit rows attributed to
// the runner from the store, and a heartbeat that advertises credentials is accepted.
func TestRunnerEventsChannel(t *testing.T) {
	e := newEnv(t, "r1")
	cl, err := e.dial(transport.UserRunner, "r1")
	must(t, err)
	defer cl.Close()
	hb, _ := json.Marshal(transport.Heartbeat{Hostname: "h", Proto: testProto, Features: []string{transport.FeatureCreds, transport.FeatureEvents},
		Credentials: []transport.HeartbeatCredential{{Name: "gl", Kind: "https-token", Hosts: []string{"gitlab.example.com"}, Status: "ok"}}})
	if ok, _, err := cl.SendRequest(transport.ReqHeartbeat, true, hb); err != nil || !ok {
		t.Fatalf("heartbeat: %v %v", ok, err)
	}
	e.waitOnline("r1")

	if _, _, err := cl.OpenChannel("exec@mirafleet", nil); err == nil {
		t.Fatal("runner opened a channel other than events@")
	} else if _, ok := err.(*ssh.OpenChannelError); !ok {
		t.Fatal(err)
	}

	ch, reqs, err := cl.OpenChannel(transport.ChanEvents, nil)
	must(t, err)
	go ssh.DiscardRequests(reqs)
	defer ch.Close()
	must(t, transport.WriteEventBatch(ch, &transport.EventBatch{Events: []transport.Event{{
		Type: transport.EventCredentialUse, Session: "0123456789abcdef0123456789abcdef", Credential: "gl",
		Host: "gitlab.example.com", Status: transport.EventOK, Count: 3, WindowMs: 1000, WindowS: 60}}}))
	waitFor(t, func() bool {
		for _, a := range e.auditRows() {
			if a.Action == service.ActionCredentialUse && a.Runner == "r1" && a.Params["count"] == "3" {
				return true
			}
		}
		return false
	})
}
