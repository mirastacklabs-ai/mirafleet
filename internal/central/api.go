package central

import (
	"context"
	"errors"
	"io"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
)

// serveOperator is the operator role: api@mirafleet channels only, at most
// api.MaxChannelsPerConn at a time. The connection is registered in OperatorConns so that
// service.RevokeOperator kicks it; its context dies with the connection, which cancels every
// in-flight request (and so action.run) of that connection.
func (s *Server) serveOperator(sconn *ssh.ServerConn, p auth.Principal, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { sconn.Wait(); cancel() }()
	defer s.cfg.Operators.add(p.ID, sconn)()
	go ssh.DiscardRequests(reqs) // operators send no global requests we honour

	tctx, tcancel := context.WithTimeout(ctx, storeTimeout)
	if err := s.cfg.Store.TouchOperator(tctx, p.ID, s.cfg.Now()); err != nil {
		s.cfg.Logf("touch operator %d: %v", p.ID, err)
	}
	tcancel()

	table := operatorTable()
	cc := &callCtx{p: p, src: table.src, svc: s.cfg.Service, st: s.cfg.Store, logf: s.cfg.Logf, state: &connState{}}
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
		go ssh.DiscardRequests(creqs)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s.serveAPIChannel(ctx, ch, table, cc)
		}()
	}
	cancel()
}

// serveAPIChannel handles one request on one channel: read, dispatch, respond, close. The
// request context ends when the connection ctx ends or the peer closes the channel.
func (s *Server) serveAPIChannel(connCtx context.Context, ch ssh.Channel, table *methodTable, cc *callCtx) {
	defer ch.Close()
	req, id, err := api.ReadRequestTimeout(ch, ch)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			api.WriteResponse(ch, api.NewError(id, api.AsError(err)))
		}
		return
	}
	ctx, cancel := context.WithCancel(connCtx)
	defer cancel()
	// Closing the channel unblocks a stalled write; the peer closing it cancels the request.
	stop := context.AfterFunc(ctx, func() { ch.Close() })
	defer stop()
	go func() { io.Copy(io.Discard, ch); cancel() }()

	resp := table.serve(ctx, cc, req)
	if err := api.WriteResponse(ch, resp); err != nil {
		s.cfg.Logf("api: write response to %q: %v", cc.p.Name, err)
	}
}
