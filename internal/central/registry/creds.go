package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// SyncCredentials implements service.CredentialSender: it sends the runner's complete effective
// credential set as a creds_sync request over its live connection (docs/M7.md 4.2). The runner
// keeps the values in memory only. The body is never logged; errors name no value.
func (r *Registry) SyncCredentials(ctx context.Context, runnerID int64, body *transport.CredsSyncBody) error {
	data, err := transport.EncodeCredsSync(body)
	if err != nil {
		return errors.New("encode creds_sync")
	}
	defer clear(data) // the encoded copy holds the secrets
	res, err := r.Workspace(ctx, runnerID, transport.WorkspaceRequest{Op: transport.OpCredsSync,
		StdinLen: int64(len(data))}, bytes.NewReader(data), nil)
	if err != nil {
		return err
	}
	if res == nil || res.Code != transport.CodeOK {
		code := "no result"
		if res != nil {
			code = res.Code
		}
		return fmt.Errorf("runner refused creds_sync: %s", code)
	}
	return nil
}
