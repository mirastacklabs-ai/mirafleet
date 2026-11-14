package orch

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/apiclient"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// clientConn adapts apiclient.Client to Conn. The keepalive is a hello round trip: apiclient does
// not expose the SSH connection, so a keepalive@mirafleet global request cannot be sent from here.
type clientConn struct{ *apiclient.Client }

func (c clientConn) Ping(ctx context.Context) error {
	_, err := c.Client.Hello(ctx, "mcp")
	return err
}

// defaultDial dials with apiclient (host key from the caller, no fallback) and says hello.
func defaultDial(ctx context.Context, addr string, signer ssh.Signer, hostKey ssh.HostKeyCallback) (Conn, error) {
	cl, err := apiclient.Dial(ctx, addr, transport.UserOperator, signer, hostKey)
	if err != nil {
		return nil, err
	}
	if err := checkHello(ctx, cl); err != nil {
		cl.Close()
		return nil, err
	}
	return clientConn{cl}, nil
}

// helloer is the part of apiclient.Client that checkHello needs.
type helloer interface {
	Hello(ctx context.Context, client string) (*api.HelloResult, error)
}

// checkHello says hello within apiclient.HandshakeTimeout (a central that completes the SSH
// handshake but never answers must not hang the dial) and checks the protocol version.
func checkHello(ctx context.Context, h helloer) error {
	hctx, cancel := context.WithTimeout(ctx, apiclient.HandshakeTimeout)
	defer cancel()
	res, err := h.Hello(hctx, "mcp")
	if err != nil {
		return err
	}
	for _, v := range res.Versions {
		if v == api.Version {
			return nil
		}
	}
	return errors.New("unsupported protocol version")
}

// live is one established connection and its keepalive.
type live struct {
	conn Conn
	stop chan struct{}
	once sync.Once
}

type dialState struct {
	done chan struct{}
	lv   *live
	err  error
}

// pool holds the connection of one context.
type pool struct {
	o    *Orch
	name string
	addr string
	sem  chan struct{} // api.MaxChannelsPerConn slots

	mu      sync.Mutex
	cur     *live
	dialing *dialState
}

func (o *Orch) poolFor(name, addr string) (*pool, error) {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return nil, errClosed
	}
	p := o.pools[name]
	var old *pool
	if p != nil && p.addr != addr { // re-login to another address
		old, p = p, nil
	}
	if p == nil {
		p = &pool{o: o, name: name, addr: addr, sem: make(chan struct{}, api.MaxChannelsPerConn)}
		o.pools[name] = p
	}
	o.mu.Unlock()
	if old != nil {
		old.closeAll()
	}
	return p, nil
}

func (p *pool) closeAll() {
	p.mu.Lock()
	lv := p.cur
	p.cur = nil
	p.mu.Unlock()
	if lv != nil {
		lv.close()
	}
}

func (lv *live) close() {
	lv.once.Do(func() {
		close(lv.stop)
		lv.conn.Close()
	})
}

// drop discards lv (closing it) and forgets it if it is still current.
func (p *pool) drop(lv *live) {
	p.mu.Lock()
	if p.cur == lv {
		p.cur = nil
	}
	p.mu.Unlock()
	lv.close()
}

// get returns the live connection, dialling it if needed. Concurrent callers share one dial,
// which runs under the pool's context so one caller cancelling does not abort the others.
func (p *pool) get(ctx context.Context) (*live, error) {
	p.mu.Lock()
	if lv := p.cur; lv != nil {
		p.mu.Unlock()
		return lv, nil
	}
	d := p.dialing
	if d == nil {
		if !p.o.goStart() {
			p.mu.Unlock()
			return nil, errClosed
		}
		d = &dialState{done: make(chan struct{})}
		p.dialing = d
		go p.dial(d)
	}
	p.mu.Unlock()
	select {
	case <-d.done:
		return d.lv, d.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *pool) dial(d *dialState) {
	defer p.o.wg.Done()
	lv, err := p.connect()
	p.mu.Lock()
	p.dialing = nil
	if err == nil {
		if p.o.goStart() {
			p.cur = lv
			go p.keepalive(lv)
		} else {
			lv.close()
			lv, err = nil, errClosed
		}
	}
	p.mu.Unlock()
	d.lv, d.err = lv, err
	close(d.done)
}

func (p *pool) connect() (*live, error) {
	o := p.o
	if o.cfgErr != nil {
		return nil, ErrNotSetUp
	}
	kp := opconf.KnownHostsPath(o.cfg.ConfigDir)
	if _, err := os.Stat(kp); err != nil {
		return nil, ErrNotSetUp // no pinned host key: never trust on first use
	}
	kf := opconf.KeyPath(o.cfg.ConfigDir)
	if _, err := os.Stat(kf); err != nil {
		return nil, ErrNotSetUp
	}
	signer, err := keys.LoadSigner(kf)
	if err != nil {
		o.cfg.Logf("operator key: %v", err)
		return nil, errf(api.CodeUnauthenticated, nil, "the operator key cannot be used: run `mirafleet mcp login …`")
	}
	conn, err := o.cfg.Dial(o.ctx, p.addr, signer, apiclient.KnownHosts(kp))
	if err != nil {
		return nil, err
	}
	return &live{conn: conn, stop: make(chan struct{})}, nil
}

// keepalive pings lv every KeepaliveInterval and drops it when a ping fails.
func (p *pool) keepalive(lv *live) {
	defer p.o.wg.Done()
	iv := p.o.cfg.KeepaliveInterval
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-lv.stop:
			return
		case <-p.o.ctx.Done():
			return
		case <-t.C:
			// Ping only an idle connection, holding a channel slot while it runs: a ping
			// must never be the ninth concurrent channel, and a busy connection is alive.
			if len(p.sem) > 0 {
				continue
			}
			select {
			case p.sem <- struct{}{}:
			default:
				continue
			}
			pctx, cancel := context.WithTimeout(p.o.ctx, iv)
			err := lv.conn.Ping(pctx)
			cancel()
			<-p.sem
			if err != nil && p.o.ctx.Err() == nil {
				var oce *ssh.OpenChannelError
				if errors.As(err, &oce) || !(isTransport(err) || errors.Is(err, context.DeadlineExceeded)) {
					// Refused or garbled, but the connection itself answered: keep it.
					p.o.cfg.Logf("keepalive to context %s: %v (connection kept)", p.name, err)
					continue
				}
				p.o.cfg.Logf("keepalive to context %s failed: connection dropped", p.name)
				p.drop(lv)
				return
			}
		}
	}
}
