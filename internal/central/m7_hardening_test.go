//go:build unix

package central

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/secrets"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

type countSender struct{ n atomic.Int32 }

func (c *countSender) SyncCredentials(context.Context, int64, *transport.CredsSyncBody) error {
	c.n.Add(1)
	return nil
}

// H-06: a reconnect inside the edge gap does not decrypt and send at once; the next heartbeat's
// retry delivers the set.
func TestReconnectInsideEdgeGapDefersDelivery(t *testing.T) {
	key, _, err := secrets.GenerateKey()
	must(t, err)
	cs := &countSender{}
	e, _, _ := newM7EnvDB(t, time.Now, key, func(cc *service.CredentialsConfig) { cc.Sender = cs })
	r := e.addRunner("r1", store.StatusActive)
	if !e.srv.claimEdge(r.ID, time.Now()) { // a connect edge just happened: the next is throttled
		t.Fatal("first claim refused")
	}
	cl, err := e.dial(transport.UserRunner, "r1")
	must(t, err)
	defer cl.Close()
	hb, _ := json.Marshal(transport.Heartbeat{Hostname: "h", Proto: testProto, Features: []string{transport.FeatureCreds}})
	if ok, _, err := cl.SendRequest(transport.ReqHeartbeat, true, hb); err != nil || !ok {
		t.Fatalf("heartbeat: %v %v", ok, err)
	}
	e.waitOnline("r1")
	time.Sleep(200 * time.Millisecond)
	if n := cs.n.Load(); n != 0 {
		t.Fatalf("throttled reconnect delivered immediately (%d sends)", n)
	}
	if ok, _, err := cl.SendRequest(transport.ReqHeartbeat, true, hb); err != nil || !ok {
		t.Fatalf("second heartbeat: %v %v", ok, err)
	}
	waitFor(t, func() bool { return cs.n.Load() == 1 })
}
