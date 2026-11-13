//go:build unix

package central

import (
	"strings"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

// Admin methods on the admin socket: fleet.workspace_set (full replace), workspace.admin_list and
// workspace.kill; the operator workspace methods are refused there.
func TestWorkspaceAdminMethodsOnAdminSocket(t *testing.T) {
	w := newWSEnv(t)
	sock := adminSocket(t, w.env)
	ctx := tctx(t)
	alice := w.opClient("alice")
	bob := w.opClient("bob")
	a, err := alice.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: wsDevFleet, Session: "a"})
	must(t, err)
	b, err := bob.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: wsGdFleet, Session: "b", Confirm: wsGdFleet})
	must(t, err)

	// admin_list: every operator's workspaces, with the owner's name; filters by fleet.
	var all api.WorkspaceAdminListResult
	must(t, AdminCall(sock, api.MethodWorkspaceAdminList, api.WorkspaceAdminListParams{}, &all))
	if len(all.Workspaces) != 2 {
		t.Fatalf("admin_list: %+v", all)
	}
	owners := map[string]string{}
	for _, x := range all.Workspaces {
		owners[x.ID] = x.Operator
	}
	if owners[a.Workspace.ID] != "alice" || owners[b.Workspace.ID] != "bob" {
		t.Fatalf("owners: %v", owners)
	}
	var gd api.WorkspaceAdminListResult
	must(t, AdminCall(sock, api.MethodWorkspaceAdminList, api.WorkspaceAdminListParams{Fleet: wsGdFleet}, &gd))
	if len(gd.Workspaces) != 1 || gd.Workspaces[0].ID != b.Workspace.ID {
		t.Fatalf("filtered admin_list: %+v", gd)
	}
	wantCode(t, AdminCall(sock, api.MethodWorkspaceAdminList, api.WorkspaceAdminListParams{Fleet: "Bad Fleet"}, nil), api.CodeBadRequest)

	// kill: regardless of owner; destroyed on the runner and closed in the store; audited.
	must(t, AdminCall(sock, api.MethodWorkspaceKill, api.WorkspaceKillParams{ID: a.Workspace.ID}, nil))
	row, err := w.db.GetWorkspace(ctx, a.Workspace.ID)
	must(t, err)
	if row.State != store.WorkspaceClosed || w.live.opCalls("destroy") != 1 {
		t.Fatalf("after kill: %s destroys=%d", row.State, w.live.opCalls("destroy"))
	}
	var killRows int
	for _, ev := range w.auditRows() {
		if ev.Action == api.MethodWorkspaceKill && ev.Workspace == a.Workspace.ID && ev.Actor != "" {
			killRows++
		}
	}
	if killRows != 2 { // start + outcome
		t.Fatalf("kill audit rows: %d", killRows)
	}
	// Killing again is a no-op; unknown and malformed ids are not_found.
	must(t, AdminCall(sock, api.MethodWorkspaceKill, api.WorkspaceKillParams{ID: a.Workspace.ID}, nil))
	wantCode(t, AdminCall(sock, api.MethodWorkspaceKill, api.WorkspaceKillParams{ID: strings.Repeat("0", 32)}, nil), api.CodeNotFound)
	wantCode(t, AdminCall(sock, api.MethodWorkspaceKill, api.WorkspaceKillParams{ID: "nope"}, nil), api.CodeNotFound)
	// The owner's next call sees it closed.
	_, err = alice.WorkspaceExec(ctx, api.WorkspaceExecParams{Workspace: a.Workspace.ID, Argv: []string{"id"}}, nil)
	wantCode(t, err, api.CodeConflict)

	// workspace_set is a full replace: omitted fields are not merged and not defaulted.
	partial := api.WorkspaceConfig{DefaultImage: wsImg, Images: []string{wsImg}, Network: "none", MemoryMB: 1024, ExecMaxS: 60}
	wantCode(t, AdminCall(sock, api.MethodFleetWorkspaceSet, api.FleetWorkspaceSetParams{Fleet: wsDevFleet, Workspace: partial}, nil), api.CodeBadRequest)
	if cfg, err := w.db.GetFleetWorkspace(ctx, wsDevFleet); err != nil || len(cfg.Images) != 2 {
		t.Fatalf("a refused set changed the config: %+v %v", cfg, err)
	}
	full := api.WorkspaceConfig{Executor: "containerd", DefaultImage: wsImg, Images: []string{wsImg}, Pull: "never", Network: "none",
		CPUMilli: 500, MemoryMB: 1024, Pids: 64, TmpfsMB: 16, DiskMB: 2048, IdleTTLS: 60, MaxTTLS: 120, ExecDefaultS: 5, ExecMaxS: 60}
	var set api.FleetWorkspaceSetResult
	must(t, AdminCall(sock, api.MethodFleetWorkspaceSet, api.FleetWorkspaceSetParams{Fleet: wsDevFleet, Workspace: full}, &set))
	if set.Fleet.Name != wsDevFleet || set.Fleet.Mode != "workspace" {
		t.Fatalf("set result: %+v", set)
	}
	cfg, err := w.db.GetFleetWorkspace(ctx, wsDevFleet)
	must(t, err)
	if len(cfg.Images) != 1 || cfg.Pull != store.PullNever || cfg.MemoryMB != 1024 || cfg.ExecMax.Seconds() != 60 || cfg.CPUMilli != 500 ||
		cfg.IdleTTL.Seconds() != 60 {
		t.Fatalf("not replaced: %+v", cfg)
	}
	// Invalid configs and unknown fleets are refused; strict params.
	wantCode(t, AdminCall(sock, api.MethodFleetWorkspaceSet, api.FleetWorkspaceSetParams{Fleet: wsDevFleet,
		Workspace: api.WorkspaceConfig{DefaultImage: "x:1", Images: []string{wsImg}}}, nil), api.CodeBadRequest)
	wantCode(t, AdminCall(sock, api.MethodFleetWorkspaceSet, api.FleetWorkspaceSetParams{Fleet: "nofleet",
		Workspace: full}, nil), api.CodeNotFound)
	wantCode(t, AdminCall(sock, api.MethodFleetWorkspaceSet, map[string]any{"fleet": wsDevFleet, "extra": 1}, nil), api.CodeBadRequest)
	// An actions-mode fleet takes no workspace config.
	if err := AdminCall(sock, api.MethodFleetWorkspaceSet, api.FleetWorkspaceSetParams{Fleet: testFleet,
		Workspace: full}, nil); err == nil {
		t.Fatal("workspace config accepted on an actions fleet")
	}
}

// The operator workspace methods are never served on the admin socket.
func TestWorkspaceOperatorMethodsRefusedOnAdminSocket(t *testing.T) {
	w := newWSEnv(t)
	sock := adminSocket(t, w.env)
	before := len(w.auditRows())
	for _, m := range []string{api.MethodWorkspaceOpen, api.MethodWorkspaceExec, api.MethodWorkspaceReadFile,
		api.MethodWorkspaceWriteFile, api.MethodWorkspaceClose, api.MethodWorkspaceList} {
		wantCode(t, AdminCall(sock, m, nil, nil), api.CodeForbidden)
		wantCode(t, AdminCall(sock, m, map[string]any{"workspace": strings.Repeat("0", 32)}, nil), api.CodeForbidden)
	}
	if n := len(w.auditRows()); n != before {
		t.Fatalf("audit grew by %d on refused methods", n-before)
	}
}
