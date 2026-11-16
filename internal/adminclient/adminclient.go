// Package adminclient is the client side of central's admin socket. It lives apart from
// internal/central so the darwin and windows binaries (which carry the admin CLI) never link the
// server-side stack.
package adminclient

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
)

// ActionTimeoutS is the action.run timeout the CLI asks for (the service caps it).
const ActionTimeoutS = 600

// Call is the client side of the admin socket (used by `mirafleet central ...`): it sends
// one framed request and reads one framed response. params and result may be nil. A response
// error is returned as *api.Error. There is no overall deadline: action.run may run for minutes.
func Call(socketPath, method string, params, result any) error {
	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		rawParams = b
	}
	var idb [8]byte
	if _, err := rand.Read(idb[:]); err != nil {
		return err
	}
	id := hex.EncodeToString(idb[:])
	payload, err := json.Marshal(api.Request{V: api.Version, ID: id, Method: method, Params: rawParams})
	if err != nil {
		return err
	}
	if len(payload) > api.MaxRequestBytes {
		return api.Errorf(api.CodeTooLarge, "request exceeds %d bytes", api.MaxRequestBytes)
	}
	c, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := api.WriteFrame(c, payload); err != nil {
		return err
	}
	b, err := api.ReadFrame(c, api.MaxResponseBytes)
	if err != nil {
		return err
	}
	var resp api.Response
	if err := json.Unmarshal(b, &resp); err != nil {
		return err
	}
	if resp.Error != nil {
		return resp.Error
	}
	if resp.ID != id {
		return errors.New("admin: response id mismatch")
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(resp.Result, result)
}
