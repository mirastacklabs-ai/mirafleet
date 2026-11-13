package service

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/tokens"
)

var enrollP = auth.Principal{Kind: auth.KindEnroll, Remote: "203.0.113.9:5555"}

func genKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

type fakeConns struct{ kicked []string }

func (f *fakeConns) KickOperator(id int64, reason string) {
	f.kicked = append(f.kicked, fmt.Sprintf("%d %s", id, reason))
}

func (e *env) initSecret() {
	e.t.Helper()
	if err := e.svc.InitRegistrationSecret(e.ctx, e.admin, SourceCLI); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) currentCode() string {
	e.t.Helper()
	r, err := e.svc.OperatorCode(e.ctx, e.admin, SourceCLI)
	if err != nil {
		e.t.Fatal(err)
	}
	return r.Code
}

func (e *env) token(fleet string, p TokenParams) string {
	e.t.Helper()
	p.Fleet = fleet
	ct, err := e.svc.CreateToken(e.ctx, e.admin, SourceCLI, p)
	if err != nil {
		e.t.Fatal(err)
	}
	return ct.Token
}

func TestJoinSuccess(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	tok := e.token("prod", TokenParams{Labels: map[string]string{"env": "p"}})
	res, err := e.svc.Join(e.ctx, enrollP, SourceEnroll, tok, "web1", genKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Fleet != "prod" || res.Runner != "web1" || res.Status != "pending" {
		t.Fatalf("%+v", res)
	}
	r, err := e.db.GetRunner(e.ctx, store.Ref{Fleet: "prod", Runner: "web1"})
	if err != nil || r.Labels["env"] != "p" {
		t.Fatalf("labels from token: %+v %v", r, err)
	}
	var found bool
	for _, a := range e.audit() {
		if a.Action == api.MethodEnrollJoin && a.Outcome == OutcomeOK && a.Source == "enroll" && a.Fleet == "prod" && a.Runner == "web1" {
			found = true
			if strings.Contains(a.Subject+a.Message, tok) {
				t.Error("token in audit")
			}
		}
	}
	if !found {
		t.Error("successful join not audited")
	}
	// Auto-approve token joins active.
	res, err = e.svc.Join(e.ctx, enrollP, SourceEnroll, e.token("prod", TokenParams{Approve: true}), "web2", genKey(t))
	if err != nil || res.Status != "active" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestJoinFailuresAreIdentical(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	key := genKey(t)

	good := e.token("prod", TokenParams{})
	id, secret, pin, _ := tokens.ParseJoinToken(good)
	other := genKey(t)
	wrongPin := tokens.JoinPrefix + id + "." + strings.Split(good, ".")[2] + "." + tokens.HostKeyHash(other)

	// wrong secret: flip to another valid 32-byte secret
	wrongSecret := tokens.JoinPrefix + id + "." + strings.Split(e.token("prod", TokenParams{}), ".")[2] + "." + pin
	_ = secret

	// unknown id
	unknown := tokens.JoinPrefix + "0123456789abcdef." + strings.Split(good, ".")[2] + "." + pin

	// used up
	used := e.token("prod", TokenParams{})
	if _, err := e.svc.Join(e.ctx, enrollP, SourceEnroll, used, "u1", genKey(t)); err != nil {
		t.Fatal(err)
	}
	// revoked
	revoked := e.token("prod", TokenParams{})
	rid, _, _, _ := tokens.ParseJoinToken(revoked)
	if err := e.svc.RevokeToken(e.ctx, e.admin, SourceCLI, rid); err != nil {
		t.Fatal(err)
	}
	// expired
	expired := e.token("prod", TokenParams{TTL: time.Minute})
	savedNow := e.svc.now
	e.svc.now = func() time.Time { return t0.Add(time.Hour) }
	defer func() { e.svc.now = savedNow }()

	cases := map[string]string{
		"malformed": "mfj1.zzz", "empty": "", "wrong host pin": wrongPin, "wrong secret": wrongSecret,
		"unknown id": unknown, "used up": used, "revoked": revoked, "expired": expired,
	}
	var first *api.Error
	var firstName string
	for name, tok := range cases {
		_, err := e.svc.Join(e.ctx, enrollP, SourceEnroll, tok, "newrunner", key)
		ae, ok := err.(*api.Error)
		if !ok {
			t.Fatalf("%s: %T %v", name, err, err)
		}
		if first == nil {
			first, firstName = ae, name
			continue
		}
		if !reflect.DeepEqual(ae, first) {
			t.Errorf("%s: %+v differs from %s: %+v", name, ae, firstName, first)
		}
	}
	if first.Code == api.CodeConflict {
		t.Error("refusal must not be a conflict")
	}
	if !strings.Contains(e.logs.String(), "expired, revoked or used up") {
		t.Error("real reason not logged")
	}
	for _, a := range e.audit() {
		if a.Action == api.MethodEnrollJoin && a.Runner != "u1" {
			t.Errorf("failed join audited: %+v", a)
		}
	}
}

func TestJoinConflictsAndKeyDisjointness(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	e.initSecret()
	e.token("prod", TokenParams{Uses: 5})
	tok := e.token("prod", TokenParams{Uses: 5})
	k1 := genKey(t)
	if _, err := e.svc.Join(e.ctx, enrollP, SourceEnroll, tok, "web1", k1); err != nil {
		t.Fatal(err)
	}
	// name taken
	if _, err := e.svc.Join(e.ctx, enrollP, SourceEnroll, tok, "web1", genKey(t)); code(t, err) != api.CodeConflict {
		t.Errorf("name taken: %v", err)
	}
	// key known
	if _, err := e.svc.Join(e.ctx, enrollP, SourceEnroll, tok, "web2", k1); code(t, err) != api.CodeConflict {
		t.Errorf("key known: %v", err)
	}
	// operator's key
	opKey := genKey(t)
	if _, err := e.svc.Register(e.ctx, enrollP, SourceEnroll, e.currentCode(), "alice", opKey); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Join(e.ctx, enrollP, SourceEnroll, tok, "web3", opKey)
	if code(t, err) != api.CodeConflict {
		t.Errorf("operator key joined: %v", err)
	}
	if _, err := e.db.GetRunner(e.ctx, store.Ref{Fleet: "prod", Runner: "web3"}); err == nil {
		t.Error("runner created with an operator's key")
	}
	// only enroll principals
	if _, err := e.svc.Join(e.ctx, e.admin, SourceCLI, tok, "web4", genKey(t)); code(t, err) != api.CodeForbidden {
		t.Errorf("admin join: %v", err)
	}
	if _, err := e.svc.Join(e.ctx, e.operator("bob"), SourceAPI, tok, "web4", genKey(t)); code(t, err) != api.CodeForbidden {
		t.Errorf("operator join: %v", err)
	}
	if _, err := e.svc.Join(e.ctx, enrollP, SourceEnroll, tok, "Bad Name", genKey(t)); code(t, err) != api.CodeBadRequest {
		t.Errorf("bad name: %v", err)
	}
}

func TestRegisterSameCodeTwoOperators(t *testing.T) {
	e := newEnv(t)
	e.initSecret()
	c := e.currentCode()
	for _, n := range []string{"alice", "bob"} {
		res, err := e.svc.Register(e.ctx, enrollP, SourceEnroll, c, n, genKey(t))
		if err != nil || res.Operator != n || res.Status != "active" {
			t.Fatalf("%s: %+v %v", n, res, err)
		}
	}
	// Audited with the operator name and fingerprint.
	n := 0
	for _, a := range e.audit() {
		if a.Action == api.MethodEnrollRegister && a.Outcome == OutcomeOK && a.Params["fingerprint"] != "" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("register audit rows %d", n)
	}
}

func TestRegisterRefusals(t *testing.T) {
	e := newEnv(t)
	e.fleet("prod")
	key := genKey(t)

	// Missing secret: refused (fail closed) with the identical error.
	fake := tokens.CodePrefix + strings.Repeat("A", tokens.CodeLen) + "." + tokens.HostKeyHash(e.host)
	_, errMissing := e.svc.Register(e.ctx, enrollP, SourceEnroll, fake, "alice", key)
	if code(t, errMissing) != api.CodeUnauthenticated {
		t.Fatalf("missing secret: %v", errMissing)
	}
	if !strings.Contains(e.logs.String(), "registration secret unavailable") {
		t.Error("reason not logged")
	}

	e.initSecret()
	cur := e.currentCode()
	body, _, _ := tokens.ParseCode(cur)

	// previous-step code
	prev := t0
	e.svc.now = func() time.Time { return prev.Add(61 * time.Second) } // now in next step
	_, errPrev := e.svc.Register(e.ctx, enrollP, SourceEnroll, cur, "alice", key)
	e.svc.now = func() time.Time { return t0 }

	// wrong host-key hash
	wrongHost := tokens.CodePrefix + body + "." + tokens.HostKeyHash(genKey(t))
	_, errHost := e.svc.Register(e.ctx, enrollP, SourceEnroll, wrongHost, "alice", key)
	_, errMal := e.svc.Register(e.ctx, enrollP, SourceEnroll, "garbage", "alice", key)
	wrongBody := tokens.CodePrefix + strings.Repeat("B", tokens.CodeLen) + "." + tokens.HostKeyHash(e.host)
	_, errBody := e.svc.Register(e.ctx, enrollP, SourceEnroll, wrongBody, "alice", key)

	for name, err := range map[string]error{"prev": errPrev, "host": errHost, "malformed": errMal, "body": errBody} {
		if !reflect.DeepEqual(err, errMissing) {
			t.Errorf("%s: %v differs from %v", name, err, errMissing)
		}
	}
	if ops, _ := e.db.ListOperators(e.ctx, true); len(ops) != 0 {
		t.Errorf("operators created by refused registrations: %d", len(ops))
	}

	// runner key refused
	rk := genKey(t)
	if _, err := e.svc.Join(e.ctx, enrollP, SourceEnroll, e.token("prod", TokenParams{}), "web1", rk); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Register(e.ctx, enrollP, SourceEnroll, cur, "alice", rk); code(t, err) != api.CodeConflict {
		t.Errorf("runner key registered: %v", err)
	}
	// only enroll principals
	if _, err := e.svc.Register(e.ctx, e.admin, SourceCLI, cur, "alice", key); code(t, err) != api.CodeForbidden {
		t.Errorf("admin register: %v", err)
	}
	if _, err := e.svc.Register(e.ctx, enrollP, SourceEnroll, cur, "BAD name", key); code(t, err) != api.CodeBadRequest {
		t.Errorf("bad name: %v", err)
	}
}

func TestRegisterIdempotentAndConflicts(t *testing.T) {
	e := newEnv(t)
	e.initSecret()
	c := e.currentCode()
	key := genKey(t)
	if _, err := e.svc.Register(e.ctx, enrollP, SourceEnroll, c, "alice", key); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		n := 0
		for _, a := range e.audit() {
			if a.Action == api.MethodEnrollRegister {
				n++
			}
		}
		return n
	}
	before := count()
	res, err := e.svc.Register(e.ctx, enrollP, SourceEnroll, c, "alice", key)
	if err != nil || res.Operator != "alice" {
		t.Fatalf("idempotent: %+v %v", res, err)
	}
	if count() != before+1 {
		t.Error("idempotent re-register not audited")
	}
	// same key, other name
	if _, err := e.svc.Register(e.ctx, enrollP, SourceEnroll, c, "mallory", key); code(t, err) != api.CodeConflict {
		t.Errorf("rename: %v", err)
	}
	// name held by another active operator
	if _, err := e.svc.Register(e.ctx, enrollP, SourceEnroll, c, "alice", genKey(t)); code(t, err) != api.CodeConflict {
		t.Errorf("name taken: %v", err)
	}
	// revoked key
	if _, err := e.svc.RevokeOperator(e.ctx, e.admin, SourceCLI, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Register(e.ctx, enrollP, SourceEnroll, c, "alice", key); code(t, err) != api.CodeConflict {
		t.Errorf("revoked key: %v", err)
	}
}

func TestInitAndRotateSecret(t *testing.T) {
	e := newEnv(t)
	e.initSecret()
	if err := e.svc.InitRegistrationSecret(e.ctx, e.admin, SourceCLI); code(t, err) != api.CodeConflict {
		t.Errorf("second init: %v", err)
	}
	c := e.currentCode()
	if _, err := e.svc.Register(e.ctx, enrollP, SourceEnroll, c, "alice", genKey(t)); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RotateRegistrationSecret(e.ctx, e.admin, SourceCLI); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Register(e.ctx, enrollP, SourceEnroll, c, "bob", genKey(t)); code(t, err) != api.CodeUnauthenticated {
		t.Errorf("old code after rotate: %v", err)
	}
	if c2 := e.currentCode(); c2 == c {
		t.Error("code unchanged after rotate")
	} else if _, err := e.svc.Register(e.ctx, enrollP, SourceEnroll, c2, "bob", genKey(t)); err != nil {
		t.Errorf("new code: %v", err)
	}
	var rot bool
	for _, a := range e.audit() {
		rot = rot || a.Action == api.MethodOperatorRotateSecret
	}
	if !rot {
		t.Error("rotate not audited")
	}
}

func TestOperatorCodeAuditedAndNeverLeaks(t *testing.T) {
	e := newEnv(t)
	e.initSecret()
	r, err := e.svc.OperatorCode(e.ctx, e.admin, SourceCLI)
	if err != nil {
		t.Fatal(err)
	}
	if r.SecondsLeft != 60 || r.HostFingerprint != ssh.FingerprintSHA256(e.host) {
		t.Errorf("%+v", r)
	}
	body, _, _ := tokens.ParseCode(r.Code)
	var shown bool
	for _, a := range e.audit() {
		if a.Action == api.MethodOperatorCode && a.Actor == e.admin.Name {
			shown = true
		}
		blob := fmt.Sprintf("%+v", a)
		if strings.Contains(blob, body) {
			t.Errorf("code in audit row %+v", a)
		}
	}
	if !shown {
		t.Error("code view not audited")
	}
	// Exercise register + failures, then scan logs and audit for the code body.
	_, _ = e.svc.Register(e.ctx, enrollP, SourceEnroll, r.Code, "alice", genKey(t))
	_, _ = e.svc.Register(e.ctx, enrollP, SourceEnroll, r.Code, "bad name", genKey(t))
	_, _ = e.svc.Register(e.ctx, enrollP, SourceEnroll, tokens.CodePrefix+body+".x", "alice", genKey(t))
	if strings.Contains(e.logs.String(), body) {
		t.Error("code in logs")
	}
	for _, a := range e.audit() {
		if strings.Contains(fmt.Sprintf("%+v", a), body) {
			t.Errorf("code in audit row %+v", a)
		}
	}
	// secret never in logs/audit either
	sec, _ := e.db.GetSetting(e.ctx, store.SettingRegistrationSecret)
	if strings.Contains(e.logs.String(), string(sec)) {
		t.Error("secret in logs")
	}
	// admin only
	op := e.operator("bob")
	for name, err := range map[string]error{
		"OperatorCode": errOf(e.svc.OperatorCode(e.ctx, op, SourceAPI)),
		"Rotate":       e.svc.RotateRegistrationSecret(e.ctx, op, SourceAPI),
		"Init":         e.svc.InitRegistrationSecret(e.ctx, op, SourceAPI),
		"List":         errOf(e.svc.ListOperators(e.ctx, op, SourceAPI, false)),
		"Revoke":       errOf(e.svc.RevokeOperator(e.ctx, op, SourceAPI, "alice")),
	} {
		if code(t, err) != api.CodeForbidden {
			t.Errorf("%s by operator: %v", name, err)
		}
	}
	// no secret -> error, no audit row
	e2 := newEnv(t)
	if _, err := e2.svc.OperatorCode(e2.ctx, e2.admin, SourceCLI); err == nil {
		t.Error("code without secret")
	}
}

func TestOperatorsAdminAndRevokeKicks(t *testing.T) {
	e := newEnv(t)
	conns := e.conns
	op := e.operator("alice")
	e.operator("bob")
	ops, err := e.svc.ListOperators(e.ctx, e.admin, SourceCLI, false)
	if err != nil || len(ops) != 2 {
		t.Fatalf("%v %v", ops, err)
	}
	if _, err := e.svc.RevokeOperator(e.ctx, e.admin, SourceCLI, "alice"); err != nil {
		t.Fatal(err)
	}
	if len(conns.kicked) != 1 || conns.kicked[0] != fmt.Sprintf("%d operator revoked", op.ID) {
		t.Errorf("kicks %v", conns.kicked)
	}
	var audited bool
	for _, a := range e.audit() {
		audited = audited || (a.Action == api.MethodOperatorRevoke && a.Subject == "alice" && a.Outcome == OutcomeOK)
	}
	if !audited {
		t.Error("revoke not audited")
	}
	if _, err := e.svc.RevokeOperator(e.ctx, e.admin, SourceCLI, "alice"); code(t, err) != api.CodeNotFound {
		t.Errorf("second revoke: %v", err)
	}
	if len(conns.kicked) != 1 {
		t.Error("kick on failed revoke")
	}
	if ops, _ := e.svc.ListOperators(e.ctx, e.admin, SourceCLI, false); len(ops) != 1 {
		t.Errorf("active ops %d", len(ops))
	}
	if _, err := e.svc.RevokeOperator(e.ctx, e.admin, SourceCLI, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RevokeOperator(e.ctx, e.admin, SourceCLI, "BAD NAME"); code(t, err) != api.CodeBadRequest {
		t.Errorf("bad name: %v", err)
	}
}
