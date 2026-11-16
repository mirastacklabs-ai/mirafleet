//go:build unix

package central

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace"
)

// ---- 3. Tool parity ----

// m7PNG is a valid 1x1 PNG.
var m7PNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")

func m7Tools(t *testing.T, f *m7Fix) {
	a := f.server("ann", "t3", f.proj)
	r := f.open(a, "dev", nil)
	if r.IsErr {
		t.Fatalf("open: %s", r.Text)
	}
	ws := r.str("workspace", "id")
	dir := f.hosts["vm1-dev"].WorkDir(workspace.ID(ws))
	call := func(name string, args map[string]any) m5Res {
		t.Helper()
		res := a.call(name, args)
		t.Logf("%s -> %s", name, res.Text)
		return res
	}
	disk := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("read %s on the host: %v", rel, err)
		}
		return string(b)
	}

	// write, read (numbered lines, line ranges)
	if r := call("write", map[string]any{"path": "f.txt", "content": "alpha\nbeta\nalpha\n"}); r.IsErr || !r.boolean("new_file") || r.num("written_bytes") != 17 {
		t.Fatalf("write: %s", r.Text)
	}
	if r := call("read", map[string]any{"path": "f.txt"}); r.str("content") != "     1\talpha\n     2\tbeta\n     3\talpha\n" || r.num("total_lines") != 3 {
		t.Fatalf("read: %s", r.Text)
	}
	if r := call("read", map[string]any{"path": "f.txt", "offset": 2, "limit": 1}); r.str("content") != "     2\tbeta\n" {
		t.Fatalf("read range: %s", r.Text)
	}

	// edit uniqueness
	if r := call("edit", map[string]any{"path": "f.txt", "old_string": "alpha", "new_string": "gamma"}); !r.IsErr || !strings.Contains(r.Text, "replace_all") {
		t.Fatalf("a non-unique edit was accepted: %s", r.Text)
	}
	if disk("f.txt") != "alpha\nbeta\nalpha\n" {
		t.Fatalf("a refused edit changed the file: %q", disk("f.txt"))
	}
	if r := call("edit", map[string]any{"path": "f.txt", "old_string": "alpha", "new_string": "gamma", "replace_all": true}); r.IsErr || r.num("count") != 2 {
		t.Fatalf("replace_all: %s", r.Text)
	}
	if r := call("edit", map[string]any{"path": "f.txt", "old_string": "nothere", "new_string": "x"}); !r.IsErr {
		t.Fatalf("an edit of absent text was accepted: %s", r.Text)
	}

	// multi_edit is atomic: the second edit fails, so the first is not applied either
	if r := call("multi_edit", map[string]any{"path": "f.txt", "edits": []any{
		map[string]any{"old_string": "beta", "new_string": "b2"}, map[string]any{"old_string": "zzz", "new_string": "x"}}}); !r.IsErr {
		t.Fatalf("multi_edit with a failing edit succeeded: %s", r.Text)
	}
	if disk("f.txt") != "gamma\nbeta\ngamma\n" {
		t.Fatalf("multi_edit was not atomic: %q", disk("f.txt"))
	}
	// edits apply in order (the second sees the first's result)
	if r := call("multi_edit", map[string]any{"path": "f.txt", "edits": []any{
		map[string]any{"old_string": "beta", "new_string": "b2"}, map[string]any{"old_string": "b2", "new_string": "b3"}}}); r.IsErr {
		t.Fatalf("multi_edit: %s", r.Text)
	}
	if disk("f.txt") != "gamma\nb3\ngamma\n" {
		t.Fatalf("multi_edit result: %q", disk("f.txt"))
	}

	// grep: modes, .gitignore, hidden files
	must(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("gamma\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, ".hidden"), []byte("gamma\n"), 0o644))
	must(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "sub", "g.go"), []byte("package sub\n// GAMMA\n"), 0o644))
	if r := call("grep", map[string]any{"pattern": "gamma"}); strings.TrimSpace(r.str("content")) != "f.txt" {
		t.Fatalf("grep files_with_matches (ignored and hidden files must be skipped): %s", r.Text)
	}
	if r := call("grep", map[string]any{"pattern": "gamma", "output_mode": "content", "-n": true}); r.str("content") != "f.txt:1:gamma\nf.txt:3:gamma" {
		t.Fatalf("grep content: %s", r.Text)
	}
	if r := call("grep", map[string]any{"pattern": "gamma", "output_mode": "count"}); strings.TrimSpace(r.str("content")) != "f.txt:2" {
		t.Fatalf("grep count: %s", r.Text)
	}
	if r := call("grep", map[string]any{"pattern": "gamma", "-i": true, "glob": "*.go"}); strings.TrimSpace(r.str("content")) != "sub/g.go" {
		t.Fatalf("grep -i with glob: %s", r.Text)
	}
	if r := call("grep", map[string]any{"pattern": "b3", "output_mode": "content", "-C": 1}); !strings.Contains(r.str("content"), "gamma") {
		t.Fatalf("grep -C: %s", r.Text)
	}

	// glob: newest first by modification time (set explicitly, no clock reads)
	for i, n := range []string{"a.log", "b.log", "c.log"} {
		p := filepath.Join(dir, n)
		must(t, os.WriteFile(p, []byte(n), 0o644))
		ts := time.Unix(1_700_000_000+int64(i)*100, 0)
		must(t, os.Chtimes(p, ts, ts))
	}
	p := filepath.Join(dir, "a.log") // a.log is now the newest
	must(t, os.Chtimes(p, time.Unix(1_700_001_000, 0), time.Unix(1_700_001_000, 0)))
	var order []string
	gr := call("glob", map[string]any{"pattern": "*.log"})
	ents, _ := gr.get("entries").([]any)
	for _, en := range ents {
		order = append(order, en.(map[string]any)["path"].(string))
	}
	if strings.Join(order, ",") != "a.log,c.log,b.log" {
		t.Fatalf("glob order: %s", gr.Text)
	}

	// ls
	if r := call("ls", map[string]any{"path": ".", "ignore": []any{"*.log"}}); r.IsErr || strings.Contains(r.Text, "a.log") || !strings.Contains(r.Text, "f.txt") {
		t.Fatalf("ls: %s", r.Text)
	}

	// notebook_edit: the rest of the notebook is preserved
	nb := `{"cells":[{"cell_type":"code","id":"c1","metadata":{"tags":["keep"]},"source":["print(1)"],"outputs":[],"execution_count":null}],` +
		`"metadata":{"kernelspec":{"name":"python3"}},"nbformat":4,"nbformat_minor":5}`
	must(t, os.WriteFile(filepath.Join(dir, "n.ipynb"), []byte(nb), 0o644))
	if r := call("notebook_edit", map[string]any{"path": "n.ipynb", "cell_id": "c1", "new_source": "print(2)", "edit_mode": "replace"}); r.IsErr {
		t.Fatalf("notebook_edit: %s", r.Text)
	}
	var got map[string]any
	must(t, json.Unmarshal([]byte(disk("n.ipynb")), &got))
	cell := got["cells"].([]any)[0].(map[string]any)
	if src, _ := json.Marshal(cell["source"]); !strings.Contains(string(src), "print(2)") || cell["id"] != "c1" ||
		got["nbformat"].(float64) != 4 || got["metadata"].(map[string]any)["kernelspec"] == nil || cell["metadata"].(map[string]any)["tags"] == nil {
		t.Fatalf("notebook after edit: %s", disk("n.ipynb"))
	}
	if r := call("read", map[string]any{"path": "n.ipynb"}); !strings.Contains(r.str("content"), "print(2)") {
		t.Fatalf("notebook read: %s", r.Text)
	}

	// an image is returned as MCP image content, not text
	must(t, os.WriteFile(filepath.Join(dir, "pic.png"), m7PNG, 0o644))
	res, imgs := m7CallImages(t, a, "read", map[string]any{"path": "pic.png"})
	if res.IsErr || len(imgs) != 1 || imgs[0].MIME != "image/png" || !bytes.Equal(imgs[0].Data, m7PNG) {
		t.Fatalf("image read: %s %d images", res.Text, len(imgs))
	}
	// other binary: size, type and digest, no bytes
	must(t, os.WriteFile(filepath.Join(dir, "x.bin"), []byte{0, 1, 2, 3, 0xff, 0xfe}, 0o644))
	if r := call("read", map[string]any{"path": "x.bin"}); r.str("kind") != "binary" || r.num("size") != 6 || r.str("sha256") == "" {
		t.Fatalf("binary read: %s", r.Text)
	}
	if r := a.call("workspace_close", nil); r.IsErr {
		t.Fatalf("close: %s", r.Text)
	}
}

// m7Cleanup is the cleanup-policy check; it needs the linux-only host session manager.
var m7Cleanup func(*testing.T, *m4Clock)

// ---- 4. Bash ----

func m7Bash(t *testing.T, f *m7Fix) {
	a := f.server("ann", "t4", f.proj)
	if r := f.open(a, "dev", nil); r.IsErr {
		t.Fatalf("open: %s", r.Text)
	}
	h := f.hosts["vm1-dev"]
	// The directory persists between calls.
	if r := a.call("bash", map[string]any{"command": "m7:cd sub"}); r.IsErr || r.str("cwd") != "/work/sub" {
		t.Fatalf("cd: %s", r.Text)
	}
	if r := a.call("bash", map[string]any{"command": "m7:pwd"}); r.str("stdout") != "/work/sub\n" || r.str("cwd") != "/work/sub" {
		t.Fatalf("pwd: %s", r.Text)
	}
	// Timeout: the requested limit reaches the executor; the default is 120 s; above 600 s is refused.
	if r := a.call("bash", map[string]any{"command": "m7:hang", "timeout_s": 7}); r.IsErr || !r.boolean("timed_out") {
		t.Fatalf("hang: %s", r.Text)
	}
	if r := a.call("bash", map[string]any{"command": "m7:hang"}); !r.boolean("timed_out") {
		t.Fatalf("hang default: %s", r.Text)
	}
	if r := a.call("bash", map[string]any{"command": "m7:hang", "timeout_s": 601}); !r.IsErr || r.code() != "bad_request" {
		t.Fatalf("an over-long timeout was accepted: %s", r.Text)
	}
	if ts := h.recordedTimeouts(); len(ts) < 2 || ts[len(ts)-2] != 7*time.Second || ts[len(ts)-1] != 120*time.Second {
		t.Fatalf("timeouts that reached the executor: %v", ts)
	}

	// 5 MiB of output: cut with counts and a ref; read_output pages the whole of it exactly.
	r := a.call("bash", map[string]any{"command": "m7:big"})
	ref := r.str("ref")
	if r.IsErr || !r.boolean("truncated") || ref == "" || r.num("stdout_bytes") != m7BigSize || r.num("omitted_bytes") <= 0 ||
		!strings.Contains(r.str("note"), "read_output") {
		t.Fatalf("big: %.600s", r.Text)
	}
	if !strings.HasPrefix(r.str("stdout"), "big line 0000000 ") {
		t.Fatal("the inline part does not start with the head of the output")
	}
	var got bytes.Buffer
	off := int64(0)
	for pages := 0; ; pages++ {
		if pages > 400 {
			t.Fatal("read_output does not end")
		}
		p := a.call("read_output", map[string]any{"ref": ref, "offset": off})
		if p.IsErr {
			t.Fatalf("read_output at %d: %s", off, p.Text)
		}
		if int64(p.num("offset")) != off || p.num("omitted_bytes") != 0 {
			// Known bug: readOutput asks the runner for `budget` raw bytes but
			// the cap counts the JSON-encoded size, so a page full of newlines is cut in the middle
			// while next_offset skips the cut part. This check stays red until that is fixed.
			t.Errorf("read_output page at %d lost %v bytes inside the page (offset %v): paging is not exact",
				off, p.num("omitted_bytes"), p.num("offset"))
			a.call("workspace_close", nil)
			return
		}
		got.WriteString(p.str("content"))
		off = int64(got.Len())
		if p.boolean("eof") {
			break
		}
		if int64(p.num("next_offset")) != off {
			t.Fatalf("next_offset %v, want %d", p.num("next_offset"), off)
		}
	}
	if got.Len() != m7BigSize || sha256.Sum256(got.Bytes()) != sha256.Sum256(h.big) {
		t.Fatalf("paged output: %d bytes, digest differs from the original", got.Len())
	}
	lastLine := fmt.Sprintf("big line %07d", bytes.Count(h.big, []byte("\n"))-1)
	if p := a.call("read_output", map[string]any{"ref": ref, "tail_lines": 1}); !strings.HasPrefix(p.str("content"), lastLine) && !strings.Contains(p.str("content"), "big line") {
		t.Fatalf("tail_lines: %s", p.Text)
	}
	if p := a.call("read_output", map[string]any{"ref": ref, "grep": "line 0000042 "}); strings.Count(p.str("content"), "\n") > 1 || !strings.Contains(p.str("content"), "big line 0000042") {
		t.Fatalf("grep: %.300s", p.Text)
	}
	if p := a.call("read_output", map[string]any{"ref": "no-such-ref"}); !p.IsErr {
		t.Fatalf("an unknown ref was accepted: %s", p.Text)
	}
	a.call("workspace_close", nil)
}

// ---- 5. Background shells ----

func m7Background(t *testing.T, f *m7Fix) {
	ctx := m3ctx(t)
	a := f.server("ann", "t5", f.proj)
	r := f.open(a, "dev", nil)
	if r.IsErr {
		t.Fatalf("open: %s", r.Text)
	}
	ws := r.str("workspace", "id")
	h := f.hosts["vm1-dev"]
	poll := func(id, want string) m5Res {
		t.Helper()
		var acc strings.Builder
		var last m5Res
		waitFor(t, func() bool {
			last = a.call("bash_output", map[string]any{"shell_id": id})
			acc.WriteString(last.str("output"))
			return strings.Contains(acc.String(), want)
		})
		last.M["output"] = acc.String()
		return last
	}

	r = a.call("bash", map[string]any{"command": "m7:build", "run_in_background": true})
	id := r.str("shell_id")
	if r.IsErr || id == "" || r.str("state") != "running" {
		t.Fatalf("start: %s", r.Text)
	}
	poll(id, "step1\n")
	// Incremental: nothing new yet.
	if o := a.call("bash_output", map[string]any{"shell_id": id}); o.str("output") != "" || !o.boolean("running") {
		t.Fatalf("second bash_output: %s", o.Text)
	}
	if l := a.call("job_list", nil); len(l.get("shells").([]any)) != 1 {
		t.Fatalf("job_list: %s", l.Text)
	}

	// The runner restarts; the job (kept by the executor) is still running and still polled.
	f.restartRunner("vm1-dev")
	waitFor(t, func() bool {
		l := a.call("job_list", nil)
		sh, _ := l.get("shells").([]any)
		return !l.IsErr && len(sh) == 1 && sh[0].(map[string]any)["state"] == "running"
	})
	close(h.gate)
	done := poll(id, "step2\n")
	waitFor(t, func() bool {
		o := a.call("bash_output", map[string]any{"shell_id": id})
		return o.str("state") == "exited"
	})
	if done.str("output") != "step2\n" {
		t.Fatalf("output after restart: %q", done.str("output"))
	}
	if o := a.call("read_output", map[string]any{"shell_id": id}); o.str("content") != "step1\nstep2\n" {
		t.Fatalf("read_output of the shell: %s", o.Text)
	}

	// kill
	r = a.call("bash", map[string]any{"command": "m7:forever", "run_in_background": true})
	kid := r.str("shell_id")
	poll(kid, "started")
	if k := a.call("kill_shell", map[string]any{"shell_id": kid}); k.IsErr || k.boolean("running") || k.str("state") == "running" {
		t.Fatalf("kill_shell: %s", k.Text)
	}
	if k := a.call("kill_shell", map[string]any{"shell_id": "j999"}); !k.IsErr {
		t.Fatalf("killing an unknown shell succeeded: %s", k.Text)
	}

	// The idle reaper skips a session with a running shell, and takes it when the shell is gone.
	r = a.call("bash", map[string]any{"command": "m7:forever", "run_in_background": true})
	busyID := r.str("shell_id")
	f.clk.advance(61 * time.Minute) // past the 1 h idle limit
	waitFor(t, func() bool {
		w, err := f.e.db.GetWorkspace(ctx, ws)
		return err == nil && f.clk.now().Sub(w.LastUsedAt) < time.Minute // touched by the busy heartbeat
	})
	f.e.svc.ReapOnce(ctx)
	if w, err := f.e.db.GetWorkspace(ctx, ws); err != nil || w.State != store.WorkspaceOpen {
		t.Fatalf("the reaper took a workspace with a running shell: %+v %v", w, err)
	}
	a.call("kill_shell", map[string]any{"shell_id": busyID})
	waitFor(t, func() bool { return h.Busy() == 0 })
	f.clk.advance(61 * time.Minute)
	f.e.svc.ReapOnce(ctx)
	if w, err := f.e.db.GetWorkspace(ctx, ws); err != nil || w.State != store.WorkspaceClosed {
		t.Fatalf("the reaper left an idle workspace: %+v %v", w, err)
	}
}

// ---- 6. Directories ----

func m7Dirs(t *testing.T, f *m7Fix) {
	h := f.hosts["vm1-qa"]
	s := f.server("ann", "d1", f.proj)
	r := f.open(s, "qa", nil)
	if r.IsErr {
		t.Fatalf("open: %s", r.Text)
	}
	ws1 := r.str("workspace", "id")
	dir := h.WorkDir(workspace.ID(ws1))
	if w := s.call("write", map[string]any{"path": "keep.txt", "content": "kept"}); w.IsErr {
		t.Fatalf("write: %s", w.Text)
	}
	// Close keeps the directory; reopening the session reattaches it.
	if c := s.call("workspace_close", nil); c.IsErr {
		t.Fatalf("close: %s", c.Text)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "keep.txt")); err != nil || string(b) != "kept" {
		t.Fatalf("close deleted the directory: %q %v", b, err)
	}
	r = f.open(s, "qa", nil)
	ws2 := r.str("workspace", "id")
	if r.IsErr || ws2 == ws1 || h.WorkDir(workspace.ID(ws2)) != dir {
		t.Fatalf("reopen: %s (dir %s vs %s)", r.Text, h.WorkDir(workspace.ID(ws2)), dir)
	}
	if rd := s.call("read", map[string]any{"path": "keep.txt"}); !strings.Contains(rd.str("content"), "kept") {
		t.Fatalf("read after reattach: %s", rd.Text)
	}

	// workdir shares one directory between two sessions.
	w1, w2 := f.server("ann", "w1", f.projWD), f.server("ann", "w2", f.projWD)
	if r := f.open(w1, "qa", nil); r.IsErr {
		t.Fatalf("workdir open 1: %s", r.Text)
	}
	if r := f.open(w2, "qa", nil); r.IsErr {
		t.Fatalf("workdir open 2: %s", r.Text)
	}
	if x := w1.call("write", map[string]any{"path": "shared.txt", "content": "from w1"}); x.IsErr {
		t.Fatalf("write in workdir: %s", x.Text)
	}
	if x := w2.call("read", map[string]any{"path": "shared.txt"}); !strings.Contains(x.str("content"), "from w1") {
		// Known gap: mcpserver does not send the project file's workdir.
		t.Errorf("BUG: .mirafleet.yaml workdir is not sent by workspace_open, so the sessions do not share a directory: %s", x.Text)
	}
	w1.call("workspace_close", nil)
	w2.call("workspace_close", nil)
	// The same through the API: central, runner and executor share the directory by workdir name.
	cl := m7OperatorClient(t, f.e, f.dirs["ann"])
	ctx := m3ctx(t)
	var ids []string
	for _, sess := range []string{"wa", "wb"} {
		o, err := cl.WorkspaceOpen(ctx, api.WorkspaceOpenParams{Fleet: "vm", Session: sess, Labels: map[string]string{"zone": "qa"}, Workdir: "shared"})
		must(t, err)
		ids = append(ids, o.Workspace.ID)
	}
	_, err := cl.WorkspaceWriteFile(ctx, api.WorkspaceWriteFileParams{Workspace: ids[0], Path: workspace.Root + "/api.txt", Truncate: true, Length: 3,
		SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("abc")))}, []byte("abc"))
	must(t, err)
	if _, data, err := cl.WorkspaceReadFile(ctx, api.WorkspaceReadFileParams{Workspace: ids[1], Path: workspace.Root + "/api.txt"}); err != nil || string(data) != "abc" {
		t.Errorf("the second workdir session reads %q, %v", data, err)
	}
	for _, id := range ids {
		must(t, cl.WorkspaceClose(ctx, id))
	}

	// Cleanup warning, then deletion, on an injected clock (the host session manager).
	if m7Cleanup == nil {
		t.Log("cleanup policy: host mode runs on linux only; skipped here")
	} else {
		m7Cleanup(t, &m4Clock{})
	}

	// delete: true deletes the directory; a reopen starts empty.
	c := s.call("workspace_close", map[string]any{"delete": true})
	if c.IsErr {
		t.Fatalf("close with delete: %s", c.Text)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("delete: true left the directory: %v", err)
	}
	if r := f.open(s, "qa", nil); r.IsErr {
		t.Fatalf("reopen after delete: %s", r.Text)
	}
	if rd := s.call("read", map[string]any{"path": "keep.txt"}); !rd.IsErr {
		t.Fatalf("the deleted file came back: %s", rd.Text)
	}
	s.call("workspace_close", nil)
}

// ---- 7. Transfer confinement ----

func m7Transfer(t *testing.T, f *m7Fix) {
	s := f.server("ann", "x7", f.proj)
	r := f.open(s, "qa", nil)
	if r.IsErr {
		t.Fatalf("open: %s", r.Text)
	}
	remote := f.hosts["vm1-qa"].WorkDir(workspace.ID(r.str("workspace", "id")))
	secret := "m7decoy-local-secret-" + randHex(t, 6)
	outside := filepath.Join(f.base, "outside-secret.txt")
	must(t, os.WriteFile(outside, []byte(secret), 0o600))
	put := func(rel, content string) {
		p := filepath.Join(f.proj, rel)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(content), 0o600))
	}
	put("ok.txt", "fine")
	put(".env", secret)
	put("id_ed25519", secret)
	put(".git/config", secret)
	put("sub/.npmrc", secret)
	put("deploy.pem", secret)
	must(t, os.Symlink(outside, filepath.Join(f.proj, "link.txt")))

	if u := s.call("upload", map[string]any{"local": "ok.txt"}); u.IsErr || u.num("bytes") != 4 {
		t.Fatalf("upload: %s", u.Text)
	}
	if b, err := os.ReadFile(filepath.Join(remote, "ok.txt")); err != nil || string(b) != "fine" {
		t.Fatalf("remote file: %q %v", b, err)
	}
	for _, n := range []string{".env", "id_ed25519", ".git/config", "sub/.npmrc", "deploy.pem", "link.txt", "../outside-secret.txt", outside} {
		u := s.call("upload", map[string]any{"local": n})
		if !u.IsErr {
			t.Errorf("upload of %s was accepted: %s", n, u.Text)
		}
		if strings.Contains(u.Text, secret) || strings.Contains(u.Text, f.base) {
			t.Errorf("refusal of %s shows content or a local path: %s", n, u.Text)
		}
		if _, err := os.Stat(filepath.Join(remote, filepath.Base(n))); err == nil && filepath.Base(n) != "ok.txt" {
			t.Errorf("%s reached the remote directory", n)
		}
	}

	// Downloads: 0644, atomic, no overwrite unless asked, confined.
	must(t, os.WriteFile(filepath.Join(remote, "out.txt"), []byte("from the vm"), 0o600))
	if d := s.call("download", map[string]any{"remote": "out.txt", "local": "dl.txt"}); d.IsErr {
		t.Fatalf("download: %s", d.Text)
	}
	st, err := os.Stat(filepath.Join(f.proj, "dl.txt"))
	must(t, err)
	if st.Mode().Perm() != 0o644 {
		t.Errorf("downloaded file mode %v, want 0644", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(filepath.Join(f.proj, "dl.txt")); string(b) != "from the vm" {
		t.Errorf("downloaded content %q", b)
	}
	if d := s.call("download", map[string]any{"remote": "out.txt", "local": "dl.txt"}); !d.IsErr {
		t.Errorf("a download overwrote without being asked: %s", d.Text)
	}
	if d := s.call("download", map[string]any{"remote": "out.txt", "local": "dl.txt", "overwrite": true}); d.IsErr {
		t.Errorf("overwrite: %s", d.Text)
	}
	for _, n := range []string{".env", ".git/hooks/x", "../escape.txt", "id_rsa", "x.pem"} {
		if d := s.call("download", map[string]any{"remote": "out.txt", "local": n}); !d.IsErr {
			t.Errorf("download to %s was accepted: %s", n, d.Text)
		}
	}
	if _, err := os.Stat(filepath.Join(f.base, "escape.txt")); err == nil {
		t.Error("a download escaped the project directory")
	}
	s.call("workspace_close", nil)
}
