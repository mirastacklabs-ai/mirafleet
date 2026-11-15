//go:build unix

package central

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/secrets"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
)

// Helpers of TestM7Done (m7_done_test.go) and the M7 glue tests.

// m7Ring adapts a secrets.Key to service.KeyRing (the same adapter cmd/mirafleet uses).
type m7Ring struct{ k secrets.Key }

func (r m7Ring) Seal(pt, aad []byte) ([]byte, error) { return secrets.Seal(r.k, pt, aad) }
func (r m7Ring) Open(ct, aad []byte) ([]byte, error) { return secrets.Open(r.k, ct, aad) }
func (r m7Ring) Check() []byte                       { return r.k.Check() }
func (r m7Ring) VerifyCheck(stored []byte) bool      { return r.k.VerifyCheck(stored) }

// newM7Env is newEnvOpts with a service that holds credentials under key, delivers them through
// the registry, and runs on the injected clock. tune may adjust the credentials config.
func newM7Env(t *testing.T, clock func() time.Time, key secrets.Key, tune func(*service.CredentialsConfig)) *env {
	t.Helper()
	var reg *registry.Registry
	var svc *service.Service
	e := newEnvOpts(t, 15*time.Second, func(c *Config) {
		r, err := registry.New(registry.Options{StaleAfter: 45 * time.Second, Logf: c.Logf})
		must(t, err)
		cc := service.CredentialsConfig{Key: m7Ring{key}, Sender: r}
		if tune != nil {
			tune(&cc)
		}
		s, err := service.New(service.Config{Store: c.Store, Policy: policy.TablePolicy{}, Live: r,
			HostKey: c.HostKey.PublicKey(), Now: clock, OperatorConns: c.Operators, Logf: c.Logf,
			ReaperInterval: time.Hour, Credentials: cc})
		must(t, err)
		c.Registry, c.Service = r, s
		reg, svc = r, s
	})
	e.reg, e.svc = reg, svc
	return e
}

// m7AdminBody is one admin-socket call with a body frame behind the request (credential.set),
// the same wire as cmd/mirafleet's adminCallBody.
func m7AdminBody(sock, method string, params any, body []byte, result any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	id := fmt.Sprintf("%x", time.Now().UnixNano())
	payload, err := json.Marshal(api.Request{V: api.Version, ID: id, Method: method, Params: raw})
	if err != nil {
		return err
	}
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if err := api.WriteFrame(c, payload); err != nil {
		return err
	}
	werr := api.WriteBody(c, body)
	b, err := api.ReadFrame(c, api.MaxResponseBytes)
	if err != nil {
		if werr != nil {
			err = werr
		}
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

// m7Ctx is a context that ends after 60 s (or with the test).
func m7Ctx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}
