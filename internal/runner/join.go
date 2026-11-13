package runner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/apiclient"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
)

// Join enrols this host with central using a join token and records the result in dir: the
// runner key (generated if absent), known_hosts holding central's verified host key (pinned from
// the token hash, never trust-on-first-use) and, last, runner.yaml, whose presence marks the
// directory as joined. It refuses a directory that has already joined. The token is used only
// for the call and is never written anywhere.
func Join(ctx context.Context, dir, addr, name, token string) (*api.EnrollJoinResult, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("central address %q: %w", addr, err)
	}
	if !keys.RunnerNameRE.MatchString(name) {
		return nil, fmt.Errorf("invalid runner name %q (lowercase letters, digits, . _ -; max 63)", name)
	}
	if err := keys.EnsureDir(dir); err != nil {
		return nil, err
	}
	if _, err := os.Stat(ConfigPath(dir)); err == nil {
		return nil, ErrAlreadyJoined
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if _, err := os.Stat(KeyPath(dir)); errors.Is(err, os.ErrNotExist) {
		if _, err := keys.Generate(dir, KeyName, name); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	signer, err := keys.LoadSigner(KeyPath(dir))
	if err != nil {
		return nil, fmt.Errorf("load runner key: %w", err)
	}
	res, hk, err := apiclient.Join(ctx, addr, token, name, nil, signer)
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(dir, KnownHostsName, []byte(apiclient.KnownHostsLine(addr, hk)+"\n")); err != nil {
		return nil, fmt.Errorf("joined, but writing known_hosts failed: %w", err)
	}
	cfg := &FileConfig{Central: addr, Name: name, Fleet: res.Fleet}
	if err := SaveConfig(dir, cfg); err != nil {
		return nil, fmt.Errorf("joined, but writing runner.yaml failed: %w", err)
	}
	return res, nil
}
