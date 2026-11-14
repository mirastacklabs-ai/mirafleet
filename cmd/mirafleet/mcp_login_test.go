package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
)

// Concurrent logins to different centrals must keep every context and every host pin.
func TestRecordLoginConcurrentKeepsAll(t *testing.T) {
	dir := t.TempDir()
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		hk, err := ssh.NewPublicKey(pub)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- recordLogin(dir, fmt.Sprintf("c%d", i), fmt.Sprintf("127.0.0.1:%d", 1000+i), hk)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	cs, err := opconf.LoadContexts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Contexts) != n {
		t.Fatalf("contexts = %d, want %d: %+v", len(cs.Contexts), n, cs.Contexts)
	}
	b, err := os.ReadFile(opconf.KnownHostsPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if !strings.Contains(string(b), fmt.Sprintf("127.0.0.1:%d ", 1000+i)) {
			t.Fatalf("host pin %d lost:\n%s", i, b)
		}
	}
}
