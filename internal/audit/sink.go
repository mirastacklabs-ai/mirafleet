package audit

import "context"

// Sink receives audit events. The runner uses the JSONL Writer; central uses the SQLite store.
//
// Record reports failure; Writer.Log swallows it (best effort, M1 behaviour). Anything that must
// fail closed on an audit failure, such as the start-audit before running an action, must
// call Record and act on the error, never Log.
type Sink interface {
	Record(ctx context.Context, ev Event) error
}

var _ Sink = (*Writer)(nil)

// Record implements Sink by appending ev as one JSON line. A nil Writer is a no-op. If ctx is
// already done nothing is written and ctx.Err() is returned.
func (a *Writer) Record(ctx context.Context, ev Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a == nil {
		return nil
	}
	return a.write(ev)
}
