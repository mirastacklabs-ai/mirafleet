//go:build linux && integration

package gitlab

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func httpsClient(s *Server) *http.Client {
	return &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: s.CA.Pool(), MinVersion: tls.VersionTLS12},
	}}
}

func get(t *testing.T, c *http.Client, u string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", u, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func basic(u, p string) string {
	return base64.StdEncoding.EncodeToString([]byte(u + ":" + p))
}

func TestHTTPSGitAuth(t *testing.T) {
	s := Start(t, Options{})
	commit := s.NewRepo(t, "acme/app", map[string]string{"README": "hi\n"})
	c := httpsClient(s)
	u := "https://" + s.HTTPSAddr + "/acme/app.git/info/refs?service=git-upload-pack"

	if r, _ := get(t, c, u, nil); r.StatusCode != 401 || !strings.HasPrefix(r.Header.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("no creds: %d %q", r.StatusCode, r.Header.Get("WWW-Authenticate"))
	}
	bad := "Basic " + basic("oauth2", "glpat-wrong")
	if r, _ := get(t, c, u, map[string]string{"Authorization": bad}); r.StatusCode != 401 {
		t.Fatalf("wrong token: %d", r.StatusCode)
	}
	good := "Basic " + basic("oauth2", s.ProjectToken)
	r, body := get(t, c, u, map[string]string{"Authorization": good})
	if r.StatusCode != 200 || !strings.Contains(body, commit) {
		t.Fatalf("good token: %d %q", r.StatusCode, body)
	}
	ev := s.Events()
	if len(ev) != 2 || ev[0].OK || !ev[1].OK || ev[1].Credential != s.ProjectToken {
		t.Fatalf("events: %+v", ev)
	}
	if r, _ := get(t, c, "https://"+s.HTTPSAddr+"/acme/%2e%2e/%2e%2e/etc/info/refs", map[string]string{"Authorization": good}); r.StatusCode == 200 {
		t.Fatal("traversal served")
	}
}

func gitEnv(t *testing.T, extra ...string) []string {
	return append([]string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x",
	}, extra...)
}

func run(t *testing.T, env []string, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir, cmd.Env = dir, env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func cloneAndPush(t *testing.T, s *Server, remote string, env []string) {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "w")
	run(t, env, "", "git", "clone", remote, dst)
	if b, err := os.ReadFile(filepath.Join(dst, "README")); err != nil || string(b) != "hi\n" {
		t.Fatalf("clone content %q %v", b, err)
	}
	if err := os.WriteFile(filepath.Join(dst, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, env, dst, "git", "add", "new.txt")
	run(t, env, dst, "git", "commit", "-m", "second")
	run(t, env, dst, "git", "push", "origin", "main")
	head := git(t, dst, "rev-parse", "HEAD")
	if got := s.Refs(t, "acme/app")["refs/heads/main"]; got != head {
		t.Fatalf("pushed ref %s, want %s", got, head)
	}
}

func TestHTTPSCloneAndPush(t *testing.T) {
	s := Start(t, Options{})
	s.NewRepo(t, "acme/app", map[string]string{"README": "hi\n"})
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, s.CA.PEM(), 0o644); err != nil {
		t.Fatal(err)
	}
	remote := "https://oauth2:" + url.PathEscape(s.ProjectToken) + "@" + s.HTTPSAddr + "/acme/app.git"
	cloneAndPush(t, s, remote, gitEnv(t, "GIT_SSL_CAINFO="+ca))
}

func TestSSHAuth(t *testing.T) {
	s := Start(t, Options{})
	dial := func(user string, k *DeployKey) error {
		signer, err := ssh.ParsePrivateKey(k.PrivatePEM)
		if err != nil {
			t.Fatal(err)
		}
		c, err := ssh.Dial("tcp", s.SSHAddr, &ssh.ClientConfig{
			User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
			HostKeyCallback: ssh.FixedHostKey(s.hostKey.PublicKey()), Timeout: 10 * time.Second,
		})
		if err == nil {
			c.Close()
		}
		return err
	}
	if dial("git", NewDeployKey(t)) == nil {
		t.Fatal("unknown key accepted")
	}
	if dial("root", s.DeployKey) == nil {
		t.Fatal("wrong user accepted")
	}
	if err := dial("git", s.DeployKey); err != nil {
		t.Fatalf("deploy key refused: %v", err)
	}
	ev := s.Events()
	if len(ev) != 3 || ev[0].OK || ev[1].OK || !ev[2].OK || ev[2].Credential != s.DeployKey.Fingerprint {
		t.Fatalf("events: %+v", ev)
	}
	if _, _, ok := s.parseGitCommand("sh -c id"); ok {
		t.Fatal("non-git command accepted")
	}
	if _, _, ok := s.parseGitCommand("git-upload-pack '../../etc'"); ok {
		t.Fatal("traversal accepted")
	}
}

func TestSSHCloneAndPush(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh client not installed")
	}
	if _, err := user.LookupId(strconv.Itoa(os.Getuid())); err != nil {
		t.Skip("ssh needs a passwd entry for the current uid (dev.sh runs as the host uid); runs under integration-host")
	}
	s := Start(t, Options{})
	s.NewRepo(t, "acme/app", map[string]string{"README": "hi\n"})
	d := t.TempDir()
	key := filepath.Join(d, "id")
	if err := os.WriteFile(key, s.DeployKey.PrivatePEM, 0o600); err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(s.SSHAddr)
	kh := filepath.Join(d, "known_hosts")
	if err := os.WriteFile(kh, []byte(s.KnownHostsLine("["+host+"]:"+port)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sshCmd := "ssh -i " + key + " -o IdentitiesOnly=yes -o UserKnownHostsFile=" + kh + " -o StrictHostKeyChecking=yes -o BatchMode=yes"
	cloneAndPush(t, s, "ssh://git@"+s.SSHAddr+"/acme/app.git", gitEnv(t, "GIT_SSH_COMMAND="+sshCmd))
}

func TestRegistryTokenExchange(t *testing.T) {
	s := Start(t, Options{})
	digest := s.AddImage(t, "acme/app", "v1")
	c := httpsClient(s)
	base := "https://" + s.HTTPSAddr
	mfURL := base + "/v2/acme/app/manifests/v1"

	r, _ := get(t, c, mfURL, nil)
	ch := r.Header.Get("WWW-Authenticate")
	if r.StatusCode != 401 || !strings.HasPrefix(ch, "Bearer realm=\""+base+"/jwt/auth\"") ||
		!strings.Contains(ch, `scope="repository:acme/app:pull"`) {
		t.Fatalf("challenge: %d %q", r.StatusCode, ch)
	}
	tokURL := base + "/jwt/auth?service=fake-registry&scope=repository:acme/app:pull"
	if r, _ := get(t, c, tokURL, map[string]string{"Authorization": "Basic " + basic(s.RegistryUser, "nope")}); r.StatusCode != 401 {
		t.Fatalf("bad registry password: %d", r.StatusCode)
	}
	_, body := get(t, c, tokURL, map[string]string{"Authorization": "Basic " + basic(s.RegistryUser, s.RegistryPassword)})
	var tr struct{ Token string }
	if err := json.Unmarshal([]byte(body), &tr); err != nil || tr.Token == "" {
		t.Fatalf("token body %q: %v", body, err)
	}
	// The basic credentials themselves never open the registry; the token does.
	if r, _ := get(t, c, mfURL, map[string]string{"Authorization": "Basic " + basic(s.RegistryUser, s.RegistryPassword)}); r.StatusCode != 401 {
		t.Fatalf("basic on registry: %d", r.StatusCode)
	}
	bearer := map[string]string{"Authorization": "Bearer " + tr.Token}
	r, mf := get(t, c, mfURL, bearer)
	if r.StatusCode != 200 || r.Header.Get("Docker-Content-Digest") != digest {
		t.Fatalf("manifest: %d %v", r.StatusCode, r.Header)
	}
	var m struct{ Layers []struct{ Digest string } }
	if err := json.Unmarshal([]byte(mf), &m); err != nil || len(m.Layers) != 1 {
		t.Fatalf("manifest body %q", mf)
	}
	if r, b := get(t, c, base+"/v2/acme/app/blobs/"+m.Layers[0].Digest, bearer); r.StatusCode != 200 || sum([]byte(b)) != m.Layers[0].Digest {
		t.Fatalf("blob: %d", r.StatusCode)
	}
	// A token scoped to one repository does not open another.
	s.AddImage(t, "acme/other", "v1")
	if r, _ := get(t, c, base+"/v2/acme/other/manifests/v1", bearer); r.StatusCode != 401 {
		t.Fatalf("cross-scope: %d", r.StatusCode)
	}
}
