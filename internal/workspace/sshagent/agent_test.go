package sshagent

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type staticProvider []Identity

func (p staticProvider) Identities() []Identity { return p }

func newSigner(t *testing.T) ssh.Signer {
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

func userAuthData(sid []byte, user string, key ssh.PublicKey) []byte {
	return (&UserAuthRequest{SessionID: sid, User: user, Algorithm: key.Type(), PublicKey: key.Marshal()}).Marshal()
}

func bindPayload(t *testing.T, host ssh.Signer, sid []byte, forwarding bool) []byte {
	t.Helper()
	sig, err := host.Sign(rand.Reader, sid)
	if err != nil {
		t.Fatal(err)
	}
	return ssh.Marshal(struct {
		HostKey      []byte
		SessionID    []byte
		Signature    []byte
		IsForwarding bool
	}{host.PublicKey().Marshal(), sid, ssh.Marshal(sig), forwarding})
}

func TestSignUserAuthWithTestServer(t *testing.T) {
	key, host := newSigner(t), newSigner(t)
	ca := newConnAgent(staticProvider{{Signer: key, User: "git", Comment: "c"}})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	scfg := &ssh.ServerConfig{PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(k.Marshal(), key.PublicKey().Marshal()) {
			return nil, nil
		}
		return nil, errRefused
	}}
	scfg.AddHostKey(host)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(c, scfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				go func() {
					for range chans {
					}
				}()
				_ = sc
			}()
		}
	}()

	dial := func(user string) error {
		cc, cn := net.Pipe()
		go agent.ServeAgent(ca, cc)
		defer cn.Close()
		ac := agent.NewClient(cn)
		c, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{
			User: user, Auth: []ssh.AuthMethod{ssh.PublicKeysCallback(ac.Signers)},
			HostKeyCallback: ssh.FixedHostKey(host.PublicKey()),
		})
		if err == nil {
			c.Close()
		}
		return err
	}
	if err := dial("git"); err != nil {
		t.Fatalf("git user: %v", err)
	}
	if err := dial("root"); err == nil {
		t.Fatal("wrong user authenticated")
	}
}

func TestListOnlyAssignedKeys(t *testing.T) {
	k := newSigner(t)
	ca := newConnAgent(staticProvider{{Signer: k, User: "git", Comment: "deploy"}})
	keys, err := ca.List()
	if err != nil || len(keys) != 1 || keys[0].Comment != "deploy" || !bytes.Equal(keys[0].Blob, k.PublicKey().Marshal()) {
		t.Fatalf("keys=%v err=%v", keys, err)
	}
	empty, _ := newConnAgent(staticProvider{}).List()
	if len(empty) != 0 {
		t.Fatal("expected no keys")
	}
}

func TestSignRefusals(t *testing.T) {
	k, other := newSigner(t), newSigner(t)
	ca := newConnAgent(staticProvider{{Signer: k, User: "git"}})
	sid := bytes.Repeat([]byte{9}, 32)

	if _, err := ca.Sign(k.PublicKey(), userAuthData(sid, "git", k.PublicKey())); err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
	for name, tc := range map[string]struct {
		key  ssh.PublicKey
		data []byte
	}{
		"arbitrary data":    {k.PublicKey(), []byte("hello world, sign me")},
		"sshsig":            {k.PublicKey(), append([]byte("SSHSIG"), make([]byte, 40)...)},
		"wrong user":        {k.PublicKey(), userAuthData(sid, "root", k.PublicKey())},
		"unassigned key":    {other.PublicKey(), userAuthData(sid, "git", other.PublicKey())},
		"key/request split": {k.PublicKey(), userAuthData(sid, "git", other.PublicKey())},
	} {
		if sig, err := ca.Sign(tc.key, tc.data); err == nil || sig != nil {
			t.Errorf("%s: signed", name)
		}
	}
}

func TestHostBinding(t *testing.T) {
	k, pinned, rogue := newSigner(t), newSigner(t), newSigner(t)
	id := Identity{Signer: k, User: "git", HostBinding: true, HostKeys: []ssh.PublicKey{pinned.PublicKey()}}
	sid := bytes.Repeat([]byte{4}, 32)
	data := userAuthData(sid, "git", k.PublicKey())

	ca := newConnAgent(staticProvider{id})
	if _, err := ca.Sign(k.PublicKey(), data); err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("unbound session signed: %v", err)
	}
	if _, err := ca.Extension(sessionBindExt, bindPayload(t, rogue, sid, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := ca.Sign(k.PublicKey(), data); err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("unpinned host signed: %v", err)
	}

	ca = newConnAgent(staticProvider{id})
	if _, err := ca.Extension(sessionBindExt, bindPayload(t, pinned, sid, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := ca.Sign(k.PublicKey(), data); err != nil {
		t.Fatalf("pinned host refused: %v", err)
	}
	// A bind for another session id does not authorise this one.
	if _, err := ca.Sign(k.PublicKey(), userAuthData(bytes.Repeat([]byte{5}, 32), "git", k.PublicKey())); err == nil {
		t.Fatal("signed for an unbound session id")
	}

	// Bad binds: forwarding, forged signature, unknown extension.
	ca = newConnAgent(staticProvider{id})
	if _, err := ca.Extension(sessionBindExt, bindPayload(t, pinned, sid, true)); err == nil {
		t.Error("forwarding bind accepted")
	}
	forged := ssh.Marshal(struct {
		HostKey      []byte
		SessionID    []byte
		Signature    []byte
		IsForwarding bool
	}{pinned.PublicKey().Marshal(), sid, ssh.Marshal(&ssh.Signature{Format: "ssh-ed25519", Blob: make([]byte, 64)}), false})
	if _, err := ca.Extension(sessionBindExt, forged); err == nil {
		t.Error("forged bind accepted")
	}
	if _, err := ca.Extension("query", nil); err == nil {
		t.Error("unknown extension accepted")
	}

	// Binding off: no bind needed.
	id.HostBinding = false
	if _, err := newConnAgent(staticProvider{id}).Sign(k.PublicKey(), data); err != nil {
		t.Fatalf("binding off: %v", err)
	}
}

func TestOtherOpsRefused(t *testing.T) {
	k := newSigner(t)
	ca := newConnAgent(staticProvider{{Signer: k, User: "git"}})
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	for name, err := range map[string]error{
		"add":       ca.Add(agent.AddedKey{PrivateKey: priv}),
		"remove":    ca.Remove(k.PublicKey()),
		"removeall": ca.RemoveAll(),
		"lock":      ca.Lock([]byte("x")),
		"unlock":    ca.Unlock([]byte("x")),
	} {
		if err == nil {
			t.Errorf("%s allowed", name)
		}
	}
	if _, err := ca.Signers(); err == nil {
		t.Error("signers allowed")
	}
}

func TestParseKnownHosts(t *testing.T) {
	h := newSigner(t)
	line := "gitlab.example.com " + string(ssh.MarshalAuthorizedKey(h.PublicKey()))
	keys, err := ParseKnownHosts([]byte("# c\n\n" + line + "@revoked x " + string(ssh.MarshalAuthorizedKey(newSigner(t).PublicKey()))))
	if err != nil || len(keys) != 1 || !bytes.Equal(keys[0].Marshal(), h.PublicKey().Marshal()) {
		t.Fatalf("keys=%v err=%v", keys, err)
	}
}
