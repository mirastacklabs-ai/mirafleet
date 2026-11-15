//go:build unix

package central

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/secrets"
	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// The credential admin methods are served on the admin socket (with the secret as a body frame),
// are forbidden on api@, and the host-workspace operator methods are reachable on api@.
func TestM7CredentialAdminSocket(t *testing.T) {
	key, _, err := secrets.GenerateKey()
	must(t, err)
	e := newM7Env(t, time.Now, key, nil)
	sock := adminSocket(t, e)

	value := []byte("m7glue-decoy-token-5f0c2a")
	sum := sha256.Sum256(value)
	p := api.CredentialSetParams{CredMeta: transport.CredMeta{Name: "gl", Kind: transport.CredHTTPSToken,
		Hosts: []string{"gitlab.test"}, Auth: transport.AuthBasic, User: "oauth2"},
		Length: len(value), SHA256: hex.EncodeToString(sum[:])}
	var set api.CredentialSetResult
	must(t, m7AdminBody(sock, api.MethodCredentialSet, p, value, &set))
	if set.Credential.Name != "gl" || set.Replaced {
		t.Fatalf("set: %+v", set)
	}
	// A wrong digest is refused and stores nothing new.
	bad := p
	bad.SHA256 = strings.Repeat("0", 64)
	wantCode(t, m7AdminBody(sock, api.MethodCredentialSet, bad, value, nil), api.CodeBadRequest)
	var list api.CredentialListResult
	must(t, AdminCall(sock, api.MethodCredentialList, api.CredentialListParams{}, &list))
	raw, _ := json.Marshal(list)
	if len(list.Credentials) != 1 || strings.Contains(string(raw), string(value)) {
		t.Fatalf("list: %s", raw)
	}
	var as api.CredentialResult
	must(t, AdminCall(sock, api.MethodCredentialAssign, api.CredentialAssignParams{Name: "gl", Fleet: testFleet}, &as))
	if len(as.Credential.Assignments) != 1 {
		t.Fatalf("assign: %+v", as)
	}
	var un api.CredentialResult
	must(t, AdminCall(sock, api.MethodCredentialUnassign, api.CredentialUnassignParams{Name: "gl", Fleet: testFleet}, &un))
	if len(un.Credential.Assignments) != 0 {
		t.Fatalf("unassign: %+v", un)
	}
	must(t, AdminCall(sock, api.MethodCredentialDelete, api.CredentialDeleteParams{Name: "gl"}, nil))
	wantCode(t, AdminCall(sock, api.MethodCredentialDelete, api.CredentialDeleteParams{Name: "gl"}, nil), api.CodeNotFound)

	// An operator on api@ cannot reach any credential method; the host-workspace methods are routed
	// (the workspace does not exist, so the service answers, not the dispatcher).
	e.addOperator("ann")
	cl := e.opClient("ann")
	ctx := m7Ctx(t)
	for _, m := range []string{api.MethodCredentialSet, api.MethodCredentialList, api.MethodCredentialDelete,
		api.MethodCredentialAssign, api.MethodCredentialUnassign, api.MethodCredentialRotate} {
		wantCode(t, cl.Call(ctx, m, map[string]string{"name": "gl"}, nil), api.CodeForbidden)
	}
	ws := "0123456789abcdef0123456789abcdef"
	calls := map[string]any{
		api.MethodWorkspaceFS:         map[string]any{"workspace": ws, "op": "ls", "path": "."},
		api.MethodWorkspaceJobStart:   map[string]any{"workspace": ws, "command": "true"},
		api.MethodWorkspaceJobStatus:  map[string]any{"workspace": ws},
		api.MethodWorkspaceJobTail:    map[string]any{"workspace": ws, "job": "abcdef0123456789"},
		api.MethodWorkspaceJobCancel:  map[string]any{"workspace": ws, "job": "abcdef0123456789"},
		api.MethodWorkspaceOutputRead: map[string]any{"workspace": ws, "ref": "abcdef0123456789"},
	}
	for m, params := range calls {
		err := cl.Call(ctx, m, params, nil)
		if err == nil || strings.Contains(err.Error(), "unknown method") {
			t.Errorf("%s: %v (want a service answer)", m, err)
		}
	}
}
