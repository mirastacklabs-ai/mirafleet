//go:build unix

package central

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/secrets"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// S11: a runner may open events@ and only events@; its batches become audit rows attributed to
// the runner from the store, and a heartbeat that advertises credentials is accepted.
func TestRunnerEventsChannel(t *testing.T) {
	key, _, err := secrets.GenerateKey()
	must(t, err)
	e := newM7Env(t, time.Now, key, nil)
	e.addRunner("r1", store.StatusActive)
	// H-03: only credentials the runner holds are recorded, so give it "gl".
	value := []byte("events-decoy-token")
	sum := sha256.Sum256(value)
	_, err = e.svc.SetCredential(context.Background(), e.admin, service.SourceCLI, api.CredentialSetParams{
		CredMeta: transport.CredMeta{Name: "gl", Kind: transport.CredHTTPSToken, Hosts: []string{"gitlab.example.com"}, Auth: transport.AuthBasic, User: "u"},
		Length:   len(value), SHA256: hex.EncodeToString(sum[:])}, value)
	must(t, err)
	_, err = e.svc.AssignCredential(context.Background(), e.admin, service.SourceCLI, api.CredentialAssignParams{Name: "gl", Fleet: testFleet})
	must(t, err)
	cl, err := e.dial(transport.UserRunner, "r1")
	must(t, err)
	defer cl.Close()
	hb, _ := json.Marshal(transport.Heartbeat{Hostname: "h", Features: []string{transport.FeatureCreds, transport.FeatureEvents},
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
