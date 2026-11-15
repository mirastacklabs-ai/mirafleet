package sshagent

import (
	"bytes"
	"testing"
)

func FuzzParseUserAuthRequest(f *testing.F) {
	good := (&UserAuthRequest{SessionID: bytes.Repeat([]byte{7}, 32), User: "git", Algorithm: "ssh-ed25519", PublicKey: []byte("blob")}).Marshal()
	f.Add(good)
	f.Add([]byte{})
	f.Add([]byte("SSHSIG\x00\x00\x00\x01"))
	f.Add(good[:len(good)-1])
	f.Add(append(bytes.Clone(good), 0))
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := ParseUserAuthRequest(data)
		if err != nil {
			return
		}
		// Accepted data must be exactly the canonical encoding of what was parsed.
		if !bytes.Equal(r.Marshal(), data) {
			t.Fatalf("accepted non-canonical data %x", data)
		}
		if r.User == "" || len(r.SessionID) == 0 || len(r.PublicKey) == 0 {
			t.Fatalf("accepted empty field: %+v", r)
		}
	})
}

func TestParseUserAuthRequestRefuses(t *testing.T) {
	good := (&UserAuthRequest{SessionID: bytes.Repeat([]byte{1}, 32), User: "git", Algorithm: "ssh-ed25519", PublicKey: []byte("k")}).Marshal()
	if _, err := ParseUserAuthRequest(good); err != nil {
		t.Fatal(err)
	}
	bad := map[string][]byte{
		"empty":      nil,
		"sshsig":     []byte("SSHSIG\x00\x00\x00\x03git\x00\x00\x00\x00"),
		"trailing":   append(bytes.Clone(good), 'x'),
		"truncated":  good[:len(good)-2],
		"random":     bytes.Repeat([]byte{0xff}, 64),
		"wrong msg":  append(append(good[:0:0], good[:36]...), append([]byte{51}, good[37:]...)...),
		"no userset": (&UserAuthRequest{SessionID: []byte{1}, Algorithm: "a", PublicKey: []byte("k")}).Marshal(),
	}
	for name, d := range bad {
		if _, err := ParseUserAuthRequest(d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
