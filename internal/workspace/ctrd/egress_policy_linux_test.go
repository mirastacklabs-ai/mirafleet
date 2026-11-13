//go:build linux

package ctrd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/wsconfig"
)

func TestCreateRefusesEgressWithSecureRuntime(t *testing.T) {
	e := &Executor{
		cfg: &wsconfig.Config{Network: []string{"none", "egress"}, Images: []string{"docker.io/library/alpine:*"}},
		cc:  &wsconfig.Containerd{SecureRuntime: "io.containerd.runsc.v1"},
	}
	s := workspace.Spec{
		ID: workspace.NewID(), Image: "docker.io/library/alpine:3", Pull: workspace.PullIfNotPresent,
		RequireSecureRuntime: true,
		Network:              workspace.Network{Mode: workspace.NetEgress, Allow: []workspace.EgressRule{{Host: "pypi.org", Port: 443}}},
	}
	err := e.Create(context.Background(), s)
	if !errors.Is(err, workspace.ErrRefused) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "egress is not supported with secure runtimes yet") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckReattachPolicy(t *testing.T) {
	off := &Executor{cfg: &wsconfig.Config{Network: []string{"none"}}}
	if err := off.checkReattach([]string{"pypi.org:443"}); err == nil {
		t.Fatal("egress no longer allowed: want error")
	}
	on := &Executor{cfg: &wsconfig.Config{Network: []string{"none", "egress"}}}
	if err := on.checkReattach([]string{"pypi.org:443"}); err != nil {
		t.Fatal(err)
	}
	if err := on.checkReattach([]string{"10.0.0.0/8:443"}); err == nil {
		t.Fatal("cidr rule with egress_cidr_rules off: want error")
	}
}
