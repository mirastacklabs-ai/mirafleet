//go:build unix

package central

import (
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
)

// Additive list fields. runners.list carries slots, free_slots and executor; fleets.list
// carries workspace{default_image, images, network} for everyone and the complete config on the
// admin socket only.
func TestListAdditiveWorkspaceFields(t *testing.T) {
	w := newWSEnv(t)
	sock := adminSocket(t, w.env)
	ctx := tctx(t)
	alice := w.opClient("alice")

	var rl api.RunnersListResult
	must(t, AdminCall(sock, api.MethodRunnersList, api.RunnersListParams{Fleet: wsDevFleet}, &rl))
	if len(rl.Runners) != 1 || rl.Runners[0].Slots != 4 || rl.Runners[0].FreeSlots != 4 || rl.Runners[0].Executor != "containerd" {
		t.Fatalf("runners.list before open: %+v", rl.Runners)
	}
	_, err := alice.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: wsDevFleet, Session: "s"})
	must(t, err)
	orl, err := alice.RunnersList(ctx, wsDevFleet)
	must(t, err)
	if len(orl.Runners) != 1 || orl.Runners[0].Slots != 4 || orl.Runners[0].FreeSlots != 3 {
		t.Fatalf("runners.list after open: %+v", orl.Runners)
	}

	ofl, err := alice.FleetsList(ctx)
	must(t, err)
	var dev *api.FleetInfo
	for i := range ofl.Fleets {
		if ofl.Fleets[i].Name == wsDevFleet {
			dev = &ofl.Fleets[i]
		}
	}
	if dev == nil || dev.Workspace == nil || dev.Workspace.DefaultImage != wsImg || len(dev.Workspace.Images) != 2 ||
		dev.Workspace.Network != "none" {
		t.Fatalf("operator fleets.list: %+v", dev)
	}
	if dev.Workspace.Config != nil {
		t.Fatalf("operator saw the complete config: %+v", dev.Workspace.Config)
	}
	var afl api.FleetsListResult
	must(t, AdminCall(sock, api.MethodFleetsList, nil, &afl))
	var adev *api.FleetInfo
	for i := range afl.Fleets {
		if afl.Fleets[i].Name == wsDevFleet {
			adev = &afl.Fleets[i]
		}
	}
	if adev == nil || adev.Workspace == nil || adev.Workspace.Config == nil || adev.Workspace.Config.DefaultImage != wsImg ||
		adev.Workspace.Config.MemoryMB == 0 || adev.Workspace.Config.IdleTTLS == 0 || adev.Workspace.Config.ExecMaxS == 0 {
		t.Fatalf("admin fleets.list: %+v", adev)
	}
}
