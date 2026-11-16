package central

import (
	"context"
	"errors"
	"io"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// serveOperator is the operator role: api@mirafleet channels only, at most
// api.MaxChannelsPerConn at a time. The connection is registered in OperatorConns so that
// service.RevokeOperator kicks it; its context dies with the connection, which cancels every
// in-flight request (and so action.run) of that connection.
func (s *Server) serveOperator(sconn *ssh.ServerConn, p auth.Principal, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { sconn.Wait(); cancel() }()
	go ssh.DiscardRequests(reqs) // operators send no global requests we honour

	if s.beforeOperatorAdd != nil {
		s.beforeOperatorAdd(p)
	}
	defer s.cfg.Operators.add(p.ID, sconn)()

	// An operator revoked after authentication but before the add above was not kicked (the kick
	// found no connection to close): reload by id and drop the connection unless still active.
	// Revoke commits before it kicks, so from here on either the kick or this check catches it.
	tctx, tcancel := context.WithTimeout(ctx, storeTimeout)
	op, err := s.cfg.Store.GetOperatorByID(tctx, p.ID)
	if err != nil || op.Status != store.OperatorActive {
		tcancel()
		s.cfg.Logf("operator %q (id %d) no longer active after authentication; closing connection", p.Name, p.ID)
		return
	}
	if err := s.cfg.Store.TouchOperator(tctx, p.ID, s.cfg.Now()); err != nil {
		s.cfg.Logf("touch operator %d: %v", p.ID, err)
		if errors.Is(err, store.ErrNotFound) {
			tcancel()
			return
		}
	}
	tcancel()

	table := operatorTable()
	cc := &callCtx{p: p, src: table.src, svc: s.cfg.Service, st: s.cfg.Store, logf: s.cfg.Logf, state: &connState{}, proto: s.cfg.Proto}
	sem := make(chan struct{}, api.MaxChannelsPerConn)
	var wg sync.WaitGroup
	defer wg.Wait()
	for nch := range chans {
		if !channelAllowed(auth.KindOperator, nch.ChannelType()) {
			nch.Reject(ssh.UnknownChannelType, "channel type not allowed")
			continue
		}
		select {
		case sem <- struct{}{}:
		default:
			nch.Reject(ssh.ResourceShortage, "too many concurrent channels")
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			<-sem
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s.serveAPIChannel(ctx, sconn, ch, creqs, table, cc)
		}()
	}
	cancel()
}

// serveAPIChannel handles one request on one channel: read, dispatch, respond, close. The
// request context ends when the connection ctx ends or the peer FULLY closes the channel. A
// half-close (CloseWrite after the request, EOF on our read side) only means "no more input":
// the request still runs and its response is still written. Full closure is observed as
// the channel's request stream ending (the mux closes it when the peer's close arrives).
func (s *Server) serveAPIChannel(connCtx context.Context, sconn *ssh.ServerConn, ch ssh.Channel, creqs <-chan *ssh.Request, table *methodTable, cc *callCtx) {
	defer ch.Close()
	ctx, cancel := context.WithCancel(connCtx)
	defer cancel()
	go func() { ssh.DiscardRequests(creqs); cancel() }()

	req, id, err := api.ReadRequestTimeout(ch, ch)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			writeResponseTimeout(ch, ch, api.NewError(id, api.AsError(err)), sconn)
		}
		return
	}
	// Closing the channel unblocks a stalled write; the peer closing it cancels the request.
	stop := context.AfterFunc(ctx, func() { ch.Close() })
	defer stop()
	// Per-call copy: the body I/O must not be shared between the channels of one connection.
	call := *cc
	call.body = &callBody{r: ch, closer: ch}
	if !takesBody(req.Method) {
		go io.Copy(io.Discard, ch) // keep the window open; stray input after the request is ignored
	}

	// Methods that may carry a body frame read it from ch themselves, and only after the
	// service has authorised the call, so nothing drains the channel until the handler is done.
	// The client's write of at most MaxFileChunk+4 bytes fits the SSH window either way.
	resp := table.serve(ctx, &call, req)
	if takesBody(req.Method) {
		go io.Copy(io.Discard, ch)
	}
	var out []byte
	if resp.Error == nil && call.body.hasOut {
		out = call.body.out
	}
	if err := writeResponseBodyTimeout(ch, ch, resp, out, call.body.hasOut && resp.Error == nil, sconn); err != nil {
		s.cfg.Logf("api: write response to %q: %v", cc.p.Name, err)
	}
}
