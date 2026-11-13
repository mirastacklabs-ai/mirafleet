package central

import (
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/service"
)

// OperatorConns tracks live operator SSH connections by operator id so that revoking an
// operator drops them (and, via their contexts, their in-flight execs). It implements
// service.OperatorConns. Build it first, hand it to the service and to central.Config.
type OperatorConns struct {
	mu sync.Mutex
	m  map[int64]map[*ssh.ServerConn]struct{}
}

var _ service.OperatorConns = (*OperatorConns)(nil)

// NewOperatorConns returns an empty tracker.
func NewOperatorConns() *OperatorConns {
	return &OperatorConns{m: map[int64]map[*ssh.ServerConn]struct{}{}}
}

// add tracks c for operator id and returns the function that stops tracking it.
func (o *OperatorConns) add(id int64, c *ssh.ServerConn) (remove func()) {
	o.mu.Lock()
	if o.m[id] == nil {
		o.m[id] = map[*ssh.ServerConn]struct{}{}
	}
	o.m[id][c] = struct{}{}
	o.mu.Unlock()
	return func() {
		o.mu.Lock()
		delete(o.m[id], c)
		if len(o.m[id]) == 0 {
			delete(o.m, id)
		}
		o.mu.Unlock()
	}
}

// KickOperator closes every tracked connection of operator id. The lock is not held while
// closing.
func (o *OperatorConns) KickOperator(id int64, _ string) {
	o.mu.Lock()
	conns := make([]*ssh.ServerConn, 0, len(o.m[id]))
	for c := range o.m[id] {
		conns = append(conns, c)
	}
	o.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}
