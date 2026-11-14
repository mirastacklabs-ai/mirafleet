package central

import (
	"context"
	"sync"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// handler runs one method. params is the raw request params; the handler decodes them strictly.
// The returned value is marshalled as the result; the error goes through service.ToAPIError.
type handler func(ctx context.Context, c *callCtx, req *api.Request) (any, error)

// callCtx is what a handler may use: the verified principal, the service and per-connection
// state. It never exposes store rows to the wire; handlers map to api DTOs.
type callCtx struct {
	p     auth.Principal
	src   service.Source
	svc   *service.Service
	st    store.Store
	logf  func(string, ...any)
	state *connState
	// body is the channel body I/O of this call (api@ only; nil on the admin socket). It is set on a
	// per-call copy of the connection's callCtx, never on the shared one.
	body *callBody
}

// connState is per-connection mutable state shared by the channels of one connection.
type connState struct {
	mu   sync.Mutex // guards hint (D-13: channels of one connection run concurrently)
	hint string     // client hint from hello; recorded, never trusted
}

// methodTable is one front end's method set: the allow predicate (an independent gate in front
// of the policy) and the handlers. The operator table serves api@mirafleet; S9 builds an admin
// table (api.IsAdminMethod) on the same type for the admin socket.
type methodTable struct {
	src      service.Source
	allowed  func(method string) bool
	handlers map[string]handler
	// timeout bounds a call; action.run sets its own deadline in the service.
	timeout func(method string) time.Duration
}

// serve dispatches req and returns the response to write. Methods outside the allow predicate
// are forbidden and never reach a handler, whatever the principal.
func (t *methodTable) serve(ctx context.Context, c *callCtx, req *api.Request) *api.Response {
	if !t.allowed(req.Method) {
		if api.IsAdminMethod(req.Method) || api.IsEnrollMethod(req.Method) || api.IsOperatorMethod(req.Method) {
			// Principal and method only, never params. Attempts at admin/enroll methods through
			// a channel that must not carry them are worth a log line (D-12).
			if api.IsAdminMethod(req.Method) || api.IsEnrollMethod(req.Method) {
				c.logf("api: %s %q attempted forbidden method %q", c.p.Kind, c.p.Name, req.Method)
			}
			return api.NewError(req.ID, api.Errorf(api.CodeForbidden, "forbidden"))
		}
		return api.NewError(req.ID, api.Errorf(api.CodeBadRequest, "unknown method"))
	}
	h, ok := t.handlers[req.Method]
	if !ok {
		return api.NewError(req.ID, api.Errorf(api.CodeBadRequest, "unknown method"))
	}
	if d := t.timeout(req.Method); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	out, err := h(ctx, c, req)
	if err != nil {
		return api.NewError(req.ID, service.ToAPIError(err))
	}
	resp, rerr := api.NewResult(req.ID, out)
	if rerr != nil {
		c.logf("api: encode %s result: %v", req.Method, rerr)
		return api.NewError(req.ID, api.Internal(rerr))
	}
	return resp
}

// operatorTable is the api@mirafleet table: hello, fleets.list, runners.list, action.run and the
// workspace.* operator methods.
func operatorTable() *methodTable {
	return &methodTable{
		src:     service.SourceAPI,
		allowed: api.IsOperatorMethod,
		handlers: map[string]handler{
			api.MethodHello:       handleHello,
			api.MethodFleetsList:  handleFleetsList,
			api.MethodRunnersList: handleRunnersList,
			api.MethodActionRun:   handleActionRun,

			api.MethodWorkspaceOpen:      handleWorkspaceOpen,
			api.MethodWorkspaceExec:      handleWorkspaceExec,
			api.MethodWorkspaceReadFile:  handleWorkspaceReadFile,
			api.MethodWorkspaceWriteFile: handleWorkspaceWriteFile,
			api.MethodWorkspaceClose:     handleWorkspaceClose,
			api.MethodWorkspaceList:      handleWorkspaceList,
		},
		timeout: operatorTimeout,
	}
}

// operatorTimeout is the per-method outer deadline of the api@ table. action.run and the
// workspace calls that carry their own (tighter) deadline in the service get a generous safety
// net instead (see the ws*OuterTimeout constants).
func operatorTimeout(m string) time.Duration {
	switch m {
	case api.MethodActionRun:
		return 0
	case api.MethodWorkspaceOpen:
		return wsOpenOuterTimeout
	case api.MethodWorkspaceExec:
		return wsExecOuterTimeout
	case api.MethodWorkspaceReadFile, api.MethodWorkspaceWriteFile:
		return wsFileOuterTimeout
	case api.MethodWorkspaceClose:
		return wsCloseOuterTimeout
	}
	return api.ListTimeout
}

// versions returns a copy of the supported protocol versions.
func versions() []int { return append([]int(nil), api.SupportedVersions...) }

func handleHello(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.HelloParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	// hello also tells the caller whether their operator is still active.
	op, err := c.st.GetOperatorByID(ctx, c.p.ID)
	if err != nil || op.Status != store.OperatorActive {
		return nil, api.Errorf(api.CodeForbidden, "forbidden")
	}
	hint := sanitizeHint(p.Client)
	c.state.mu.Lock()
	c.state.hint = hint
	c.state.mu.Unlock()
	if hint != "" {
		c.logf("operator %q hello (client hint %q, unverified)", c.p.Name, hint)
	}
	return api.HelloResult{Operator: c.p.Name, Versions: versions(), Server: "mirafleet"}, nil
}

// sanitizeHint keeps a short printable-ASCII client hint; anything else is dropped.
func sanitizeHint(s string) string {
	if len(s) > 32 {
		return ""
	}
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return ""
		}
	}
	return s
}

func handleFleetsList(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var none struct{}
	if e := api.DecodeParams(req.Params, &none); e != nil {
		return nil, e
	}
	fs, err := c.svc.ListFleets(ctx, c.p, c.src)
	if err != nil {
		return nil, err
	}
	out := api.FleetsListResult{Fleets: make([]api.FleetInfo, len(fs))}
	for i, f := range fs {
		out.Fleets[i] = fleetInfo(f, c.exposeConfig())
	}
	return out, nil
}

func handleRunnersList(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.RunnersListParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	vs, err := c.svc.ListRunners(ctx, c.p, c.src, p.Fleet)
	if err != nil {
		return nil, err
	}
	out := api.RunnersListResult{Runners: make([]api.RunnerInfo, len(vs))}
	for i, v := range vs {
		ri := api.RunnerInfo{Name: v.Name, Status: string(v.Status), Online: v.Online, Labels: v.Labels,
			Slots: v.Slots, FreeSlots: v.FreeSlots, Executor: v.Executor}
		if !v.LastSeenAt.IsZero() {
			t := v.LastSeenAt
			ri.LastSeen = &t
		}
		out.Runners[i] = ri
	}
	return out, nil
}

func handleActionRun(ctx context.Context, c *callCtx, req *api.Request) (any, error) {
	var p api.ActionRunParams
	if e := api.DecodeParams(req.Params, &p); e != nil {
		return nil, e
	}
	return c.svc.RunAction(ctx, c.p, c.src, p.Fleet, p.Runner, p.Action, p.Params, p.TimeoutS, p.Confirm)
}
