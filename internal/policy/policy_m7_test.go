package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

var allMethods = []string{
	api.MethodHello, api.MethodFleetsList, api.MethodRunnersList, api.MethodActionRun,
	api.MethodWorkspaceOpen, api.MethodWorkspaceExec, api.MethodWorkspaceReadFile,
	api.MethodWorkspaceWriteFile, api.MethodWorkspaceClose, api.MethodWorkspaceList,
	api.MethodWorkspaceFS, api.MethodWorkspaceJobStart, api.MethodWorkspaceJobStatus,
	api.MethodWorkspaceJobTail, api.MethodWorkspaceJobCancel, api.MethodWorkspaceOutputRead,
	api.MethodFleetWorkspaceSet, api.MethodWorkspaceAdminList, api.MethodWorkspaceKill,
	api.MethodFleetCreate, api.MethodTokenCreate, api.MethodTokenList, api.MethodTokenRevoke,
	api.MethodRunnerLifecycle, api.MethodOperatorCode, api.MethodOperatorRotateSecret,
	api.MethodOperatorList, api.MethodOperatorRevoke, api.MethodAuditList, api.MethodGUIResetAdmin,
	api.MethodFleetSetConfirm,
	api.MethodCredentialSet, api.MethodCredentialList, api.MethodCredentialDelete,
	api.MethodCredentialAssign, api.MethodCredentialUnassign, api.MethodCredentialRotate,
	api.MethodEnrollJoin, api.MethodEnrollRegister,
}

// Every method is classified exactly once, and the table agrees with it for every principal.
func TestAuthorizeMatchesClassification(t *testing.T) {
	ctx := context.Background()
	op := auth.Principal{Kind: auth.KindOperator, Name: "alice"}
	active := Resource{ActorStatus: string(store.OperatorActive)}
	for _, m := range allMethods {
		n := 0
		for _, b := range []bool{api.IsOperatorMethod(m), api.IsAdminMethod(m), api.IsEnrollMethod(m)} {
			if b {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s is classified %d times", m, n)
		}
		err := TablePolicy{}.Authorize(ctx, op, m, active)
		if want := api.IsOperatorMethod(m); (err == nil) != want {
			t.Errorf("operator on %s: err=%v want allowed=%v", m, err, want)
		}
		if err := (TablePolicy{}).Authorize(ctx, auth.LocalAdmin(1), m, Resource{}); err != nil {
			t.Errorf("admin on %s: %v", m, err)
		}
		err = TablePolicy{}.Authorize(ctx, auth.Principal{Kind: auth.KindEnroll}, m, Resource{})
		if (err == nil) != api.IsEnrollMethod(m) {
			t.Errorf("enroll on %s: %v", m, err)
		}
		if err := (TablePolicy{}).Authorize(ctx, auth.Principal{Kind: auth.KindRunner}, m, Resource{}); !errors.Is(err, ErrForbidden) {
			t.Errorf("runner on %s: %v", m, err)
		}
	}
}

func TestCredentialMethodsNeverForOperators(t *testing.T) {
	op := auth.Principal{Kind: auth.KindOperator, Name: "alice"}
	for _, m := range []string{api.MethodCredentialSet, api.MethodCredentialList, api.MethodCredentialDelete,
		api.MethodCredentialAssign, api.MethodCredentialUnassign, api.MethodCredentialRotate} {
		err := TablePolicy{}.Authorize(context.Background(), op, m, Resource{ActorStatus: string(store.OperatorActive)})
		if !errors.Is(err, ErrForbidden) {
			t.Errorf("%s allowed for an operator: %v", m, err)
		}
	}
}

func TestRequiresConfirm(t *testing.T) {
	yes := []struct{ m, fs string }{
		{api.MethodActionRun, ""}, {api.MethodWorkspaceOpen, ""}, {api.MethodWorkspaceExec, ""},
		{api.MethodWorkspaceWriteFile, ""}, {api.MethodWorkspaceJobStart, ""},
		{api.MethodWorkspaceFS, "write"}, {api.MethodWorkspaceFS, "edit"},
		{api.MethodWorkspaceFS, "multi_edit"}, {api.MethodWorkspaceFS, "notebook_edit"},
	}
	for _, c := range yes {
		if !RequiresConfirm(c.m, c.fs) {
			t.Errorf("%s %s must need confirm", c.m, c.fs)
		}
	}
	no := []struct{ m, fs string }{
		{api.MethodHello, ""}, {api.MethodFleetsList, ""}, {api.MethodRunnersList, ""},
		{api.MethodWorkspaceReadFile, ""}, {api.MethodWorkspaceClose, ""}, {api.MethodWorkspaceList, ""},
		{api.MethodWorkspaceJobStatus, ""}, {api.MethodWorkspaceJobTail, ""},
		{api.MethodWorkspaceJobCancel, ""}, {api.MethodWorkspaceOutputRead, ""},
		{api.MethodWorkspaceFS, "read"}, {api.MethodWorkspaceFS, "grep"}, {api.MethodWorkspaceFS, "glob"},
		{api.MethodWorkspaceFS, "ls"}, {api.MethodWorkspaceFS, "stat"}, {api.MethodWorkspaceFS, "bogus"},
		{api.MethodCredentialSet, ""},
	}
	for _, c := range no {
		if RequiresConfirm(c.m, c.fs) {
			t.Errorf("%s %s must not need confirm", c.m, c.fs)
		}
	}
	// fsOp is ignored for other methods.
	if RequiresConfirm(api.MethodWorkspaceFS, "") || !RequiresConfirm(api.MethodWorkspaceExec, "read") {
		t.Error("fsOp handling wrong")
	}
}
