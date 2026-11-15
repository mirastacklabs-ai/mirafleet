//go:build linux && integration

package gitlab

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git runs git in dir with a hermetic environment and returns trimmed stdout.
func git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=fake", "GIT_AUTHOR_EMAIL=fake@gitlab.test",
		"GIT_COMMITTER_NAME=fake", "GIT_COMMITTER_EMAIL=fake@gitlab.test",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// RepoPath is where the bare repository for a project path such as "acme/app" lives.
func (s *Server) RepoPath(project string) string {
	return filepath.Join(s.Root, filepath.FromSlash(strings.TrimSuffix(project, ".git"))+".git")
}

// NewRepo creates the bare repository for project ("acme/app") with one commit on main holding
// files (path -> content) and returns the commit id.
func (s *Server) NewRepo(t testing.TB, project string, files map[string]string) string {
	t.Helper()
	bare := s.RepoPath(project)
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, bare, "init", "--bare", "-b", "main")
	work := t.TempDir()
	git(t, work, "init", "-b", "main")
	for name, body := range files {
		p := filepath.Join(work, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, work, "add", "-A")
	git(t, work, "commit", "-m", "initial")
	git(t, work, "push", bare, "main")
	return git(t, work, "rev-parse", "HEAD")
}

// Refs reads the refs (name -> commit id) of the bare repository of project, e.g. to assert what
// a runner pushed.
func (s *Server) Refs(t testing.TB, project string) map[string]string {
	t.Helper()
	out := git(t, s.RepoPath(project), "for-each-ref", "--format=%(refname) %(objectname)")
	refs := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) == 2 {
			refs[f[0]] = f[1]
		}
	}
	return refs
}
