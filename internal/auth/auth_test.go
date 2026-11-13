package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

type fakeStore struct {
	runners   map[string]store.Runner
	operators map[string]store.Operator
}

func (f fakeStore) GetRunnerByKey(_ context.Context, k []byte) (store.Runner, error) {
	if r, ok := f.runners[string(k)]; ok {
		return r, nil
	}
	return store.Runner{}, store.ErrNotFound
}
func (f fakeStore) GetOperatorByKey(_ context.Context, k []byte) (store.Operator, error) {
	if o, ok := f.operators[string(k)]; ok {
		return o, nil
	}
	return store.Operator{}, store.ErrNotFound
}

type meta struct{ user string }

func (m meta) User() string          { return m.user }
func (m meta) SessionID() []byte     { return nil }
func (m meta) ClientVersion() []byte { return nil }
func (m meta) ServerVersion() []byte { return nil }
func (m meta) RemoteAddr() net.Addr  { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5} }
func (m meta) LocalAddr() net.Addr   { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 6} }

func edSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func fixture(t *testing.T) (*KeyAuth, map[string]ssh.Signer) {
	t.Helper()
	s := map[string]ssh.Signer{}
	for _, n := range []string{"runner", "pending", "revoked", "op", "oprev", "unknown"} {
		s[n] = edSigner(t)
	}
	k := func(n string) string { return string(s[n].PublicKey().Marshal()) }
	fs := fakeStore{
		runners: map[string]store.Runner{
			k("runner"):  {ID: 1, Name: "r1", Status: store.StatusActive},
			k("pending"): {ID: 2, Name: "r2", Status: store.StatusPending},
			k("revoked"): {ID: 3, Name: "r3", Status: store.StatusRevoked},
		},
		operators: map[string]store.Operator{
			k("op"):    {ID: 7, Name: "alice", Status: store.OperatorActive},
			k("oprev"): {ID: 8, Name: "bob", Status: store.OperatorRevoked},
		},
	}
	return NewKeyAuth(fs), s
}

func TestKeyAuth(t *testing.T) {
	a, s := fixture(t)
	cases := []struct {
		name, user, key string
		ok              bool
		kind            Kind
		status          string
	}{
		{"runner ok", "runner", "runner", true, KindRunner, "active"},
		{"pending runner ok", "runner", "pending", true, KindRunner, "pending"},
		{"revoked runner", "runner", "revoked", false, "", ""},
		{"unknown runner", "runner", "unknown", false, "", ""},
		{"operator ok", "operator", "op", true, KindOperator, "active"},
		{"revoked operator", "operator", "oprev", false, "", ""},
		{"unknown operator", "operator", "unknown", false, "", ""},
		{"runner key as operator", "operator", "runner", false, "", ""},
		{"operator key as runner", "runner", "op", false, "", ""},
		{"enroll any key", "enroll", "unknown", true, KindEnroll, ""},
		{"enroll with registered key", "enroll", "runner", true, KindEnroll, ""},
		{"bad user", "root", "runner", false, "", ""},
		{"admin user", "admin", "op", false, "", ""},
		{"empty user", "", "runner", false, "", ""},
	}
	for _, c := range cases {
		p, err := a.Authenticate(meta{c.user}, s[c.key].PublicKey())
		if c.ok != (err == nil) {
			t.Errorf("%s: err=%v", c.name, err)
			continue
		}
		if c.ok && (p.Kind != c.kind || p.Status != c.status || p.Fingerprint == "" || p.Remote == "") {
			t.Errorf("%s: got %+v", c.name, p)
		}
	}
	p, _ := a.Authenticate(meta{"runner"}, s["runner"].PublicKey())
	if p.ID != 1 || p.Name != "r1" {
		t.Errorf("identity: %+v", p)
	}
}

func TestKeyAuthRejectsNonEd25519(t *testing.T) {
	a, _ := fixture(t)
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	for name, k := range map[string]any{"rsa": rk, "ecdsa": ek} {
		sg, err := ssh.NewSignerFromKey(k)
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range []string{"runner", "operator", "enroll"} {
			if _, err := a.Authenticate(meta{u}, sg.PublicKey()); err == nil {
				t.Errorf("%s accepted for %s", name, u)
			}
		}
	}
	if _, err := a.Authenticate(meta{"enroll"}, nil); err == nil {
		t.Error("nil key accepted")
	}
}

func TestLocalAdmin(t *testing.T) {
	if p := LocalAdmin(501); p.Kind != KindAdmin {
		t.Fatal(p)
	}
}

func TestEncodeDecode(t *testing.T) {
	for _, p := range []Principal{
		{KindRunner, 1, "r1", "SHA256:x", "1.2.3.4:5", "pending"},
		{KindOperator, 9, "alice", "SHA256:y", "1.2.3.4:5", "active"},
		{KindEnroll, 0, "", "SHA256:z", "1.2.3.4:5", ""},
	} {
		got, err := Decode(Encode(p))
		if err != nil || got != p {
			t.Errorf("%+v: got %+v err %v", p, got, err)
		}
	}
}

func TestDecodeFailsClosed(t *testing.T) {
	good := Principal{KindRunner, 1, "r1", "SHA256:x", "r", "active"}
	if _, err := Decode(nil); err == nil {
		t.Error("nil")
	}
	if _, err := Decode(&ssh.Permissions{}); err == nil {
		t.Error("empty")
	}
	mut := func(name string, f func(m map[string]string)) {
		perm := Encode(good)
		f(perm.Extensions)
		if _, err := Decode(perm); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, k := range []string{extKind, extID, extName, extFP, extRemote, extStatus} {
		k := k
		mut("missing "+k, func(m map[string]string) { delete(m, k) })
	}
	mut("kind admin", func(m map[string]string) { m[extKind] = "admin" })
	mut("kind junk", func(m map[string]string) { m[extKind] = "root" })
	mut("id junk", func(m map[string]string) { m[extID] = "x1" })
	mut("id negative", func(m map[string]string) { m[extID] = "-1" })
	mut("id leading zero", func(m map[string]string) { m[extID] = "01" })
	mut("id zero runner", func(m map[string]string) { m[extID] = "0" })
	mut("empty fp", func(m map[string]string) { m[extFP] = "" })
	mut("empty name", func(m map[string]string) { m[extName] = "" })
	mut("enroll with id", func(m map[string]string) { m[extKind] = "enroll" })
}

func TestPublicKeyCallbackEndToEnd(t *testing.T) {
	a, s := fixture(t)
	host := edSigner(t)
	got := make(chan Principal, 4)
	cfg := &ssh.ServerConfig{PublicKeyCallback: PublicKeyCallback(a)}
	cfg.AddHostKey(host)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc, _, _, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				defer sc.Close()
				p, err := Decode(sc.Permissions)
				if err != nil {
					p = Principal{Name: "DECODE-FAIL"}
				}
				got <- p
			}()
		}
	}()

	dial := func(user string, sg ssh.Signer) error {
		c, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{
			User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(sg)},
			HostKeyCallback: ssh.FixedHostKey(host.PublicKey()), Timeout: 5 * time.Second,
		})
		if err == nil {
			c.Close()
		}
		return err
	}
	if err := dial("operator", s["op"]); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-got:
		if p.Kind != KindOperator || p.ID != 7 || p.Name != "alice" || p.Remote == "" {
			t.Fatalf("got %+v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
	if err := dial("operator", s["runner"]); err == nil {
		t.Error("runner key accepted as operator")
	}
	if err := dial("runner", s["revoked"]); err == nil {
		t.Error("revoked accepted")
	}
	if err := dial("enroll", edSigner(t)); err != nil {
		t.Errorf("enroll: %v", err)
	}
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	rs, _ := ssh.NewSignerFromKey(rk)
	if err := dial("enroll", rs); err == nil {
		t.Error("rsa accepted")
	}
}

type errStore struct{ err error }

func (e errStore) GetRunnerByKey(context.Context, []byte) (store.Runner, error) {
	return store.Runner{}, e.err
}
func (e errStore) GetOperatorByKey(context.Context, []byte) (store.Operator, error) {
	return store.Operator{}, e.err
}

func TestStoreErrorsLoggedServerSideOnly(t *testing.T) {
	var logged []string
	a := NewKeyAuth(errStore{errors.New("disk on fire")})
	a.Logf = func(f string, args ...any) { logged = append(logged, fmt.Sprintf(f, args...)) }
	_, err := a.Authenticate(meta{"runner"}, edSigner(t).PublicKey())
	if !errors.Is(err, ErrDenied) || strings.Contains(err.Error(), "fire") {
		t.Fatalf("err = %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "disk on fire") {
		t.Fatalf("logged = %v", logged)
	}
	// not-found is routine and not logged
	logged = nil
	a = NewKeyAuth(errStore{store.ErrNotFound})
	a.Logf = func(f string, args ...any) { logged = append(logged, f) }
	if _, err := a.Authenticate(meta{"operator"}, edSigner(t).PublicKey()); !errors.Is(err, ErrDenied) || len(logged) != 0 {
		t.Fatalf("err = %v logged = %v", err, logged)
	}
}
