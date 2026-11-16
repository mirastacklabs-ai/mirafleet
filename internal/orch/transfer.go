package orch

// Upload and download between the local project directory and a workspace. The local side is confined to the project directory through os.Root (a path or symlink
// leading out is refused), symlinks inside it are never followed, and a deny list keeps secrets
// and VCS/config state out. Pure os/filepath: this file builds on linux, darwin and windows.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
)

// MaxTransferBytes is the largest file moved in either direction.
const MaxTransferBytes = 64 << 20

// UploadParams: send the local file Local (relative to the project directory, or absolute inside
// it) to Remote in the workspace. Empty Remote means the same relative path. Mode 0 keeps the
// local executable bit (0755 or 0644). Confirm is the fleet name on guarded fleets.
type UploadParams struct {
	Workspace string
	Local     string
	Remote    string
	Mode      uint32
	Confirm   string
}

// DownloadParams: fetch Remote from the workspace into Local (relative to the project directory,
// or absolute inside it). An existing file is only replaced when Overwrite is set.
type DownloadParams struct {
	Workspace string
	Remote    string
	Local     string
	Overwrite bool
}

// TransferResult reports a finished transfer. Local is the project-relative path with forward
// slashes; absolute local paths never appear in results or errors.
type TransferResult struct {
	Local  string `json:"local"`
	Remote string `json:"remote"`
	Bytes  int64  `json:"bytes"`
}

// Names (any path component, matched case-insensitively on every OS) that are never transferred.
var denyNames = map[string]bool{
	".git": true, ".claude": true, ".ssh": true, ".gnupg": true, ".aws": true, ".mirafleet": true,
	".mirafleet.yaml": true, ".npmrc": true, ".netrc": true, "keychains": true,
	".mcp.json": true, ".vscode": true, ".opencode": true, "opencode.json": true, ".kilocode": true, ".husky": true,
}

// aliasRune reports invisible code points that make a name look like another one (HFS+ ignores
// them when comparing names).
func aliasRune(r rune) bool {
	return (r >= 0x200B && r <= 0x200F) || (r >= 0x202A && r <= 0x202E) || (r >= 0x2060 && r <= 0x206F) || r == 0xFEFF
}

// Glob patterns for the same purpose (path.Match syntax, applied to the lowercased component).
var denyGlobs = []string{".env*", "*.pem", "*.key", "id_*", "*.keychain", "*.keychain-db", "*.p12", "*.pfx"}

var reservedWin = map[string]bool{"con": true, "prn": true, "aux": true, "nul": true}

func reservedName(c string) bool {
	base := c
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if reservedWin[base] {
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "com") || strings.HasPrefix(base, "lpt")) && base[3] >= '1' && base[3] <= '9' {
		return true
	}
	return false
}

// denied reports whether one lowercased component is on the deny list.
func denied(c string) bool {
	if denyNames[c] {
		return true
	}
	for _, g := range denyGlobs {
		if ok, _ := path.Match(g, c); ok {
			return true
		}
	}
	return false
}

func tErr(code, format string, a ...any) error {
	return errf(code, nil, clean(fmt.Sprintf(format, a...)))
}

// LocalPath turns a user-supplied local path into a clean, slash-separated path relative to
// projectDir, or an error. It is lexical only (confinement against symlinks is os.Root's job in
// Upload and Download). Backslashes count as separators on every OS, so a Windows-style path is
// judged the same everywhere.
func LocalPath(projectDir, name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) {
		return "", tErr(api.CodeBadRequest, "invalid local path")
	}
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) {
		if !filepath.IsAbs(name) {
			return "", tErr(api.CodeForbidden, "local path is outside the project directory")
		}
		rel, err := filepath.Rel(filepath.Clean(projectDir), filepath.Clean(name))
		if err != nil {
			return "", tErr(api.CodeForbidden, "local path is outside the project directory")
		}
		name = rel
	}
	var comps []string
	for _, c := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		switch c {
		case ".":
			continue
		case "..":
			return "", tErr(api.CodeForbidden, "local path is outside the project directory")
		}
		comps = append(comps, c)
	}
	if len(comps) == 0 {
		return "", tErr(api.CodeBadRequest, "local path must name a file")
	}
	for _, c := range comps {
		if strings.ContainsRune(c, ':') || strings.HasSuffix(c, ".") || strings.HasSuffix(c, " ") ||
			strings.ContainsRune(c, '~') || strings.IndexFunc(c, aliasRune) >= 0 { // 8.3 short names, invisible aliases
			return "", tErr(api.CodeForbidden, "local path is not allowed")
		}
		l := strings.ToLower(c)
		if denied(l) || reservedName(l) {
			return "", tErr(api.CodeForbidden, "%s is not transferable (secrets and project state are excluded)", strings.Join(comps, "/"))
		}
	}
	return strings.Join(comps, "/"), nil
}

func openProject(dir string) (*os.Root, error) {
	if !filepath.IsAbs(dir) {
		return nil, tErr(api.CodeBadRequest, "project directory must be absolute")
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, tErr(api.CodeNotFound, "cannot open the project directory")
	}
	return r, nil
}

// noSymlinks refuses a path whose existing prefix contains a symlink (any link, even one that
// stays inside the project, could point at a denied name).
func noSymlinks(root *os.Root, rel string) error {
	comps := strings.Split(rel, "/")
	for i := range comps {
		p := filepath.FromSlash(strings.Join(comps[:i+1], "/"))
		st, err := root.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return tErr(api.CodeForbidden, "%s is not accessible", rel)
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			return tErr(api.CodeForbidden, "%s goes through a symlink", rel)
		}
	}
	return nil
}

func chunkSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Upload sends a local file to the workspace in chunks of at most api.MaxFileChunk bytes. Never
// retried per chunk; a failure part-way leaves a partial remote file.
func (o *Orch) Upload(ctx context.Context, cname, projectDir string, p UploadParams) (*TransferResult, error) {
	rel, err := LocalPath(projectDir, p.Local)
	if err != nil {
		return nil, err
	}
	remote := p.Remote
	if remote == "" {
		remote = rel
	}
	root, err := openProject(projectDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := noSymlinks(root, rel); err != nil {
		return nil, err
	}
	lp := filepath.FromSlash(rel)
	st, err := root.Lstat(lp)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, tErr(api.CodeNotFound, "no such file: %s", rel)
	}
	if err != nil || !st.Mode().IsRegular() {
		return nil, tErr(api.CodeBadRequest, "%s is not a regular file", rel)
	}
	if st.Size() > MaxTransferBytes {
		return nil, tErr(api.CodeTooLarge, "%s exceeds %d MiB", rel, MaxTransferBytes>>20)
	}
	f, err := root.Open(lp)
	if err != nil {
		return nil, tErr(api.CodeForbidden, "cannot read %s", rel)
	}
	defer f.Close()
	if fst, err := f.Stat(); err != nil || !fst.Mode().IsRegular() || !os.SameFile(st, fst) {
		return nil, tErr(api.CodeForbidden, "%s changed while opening", rel)
	}
	mode := p.Mode
	if mode == 0 {
		mode = 0o644
		if st.Mode()&0o111 != 0 {
			mode = 0o755
		}
	}

	buf := make([]byte, api.MaxFileChunk)
	var total int64
	for {
		n, rerr := io.ReadFull(f, buf)
		if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
			return nil, tErr(api.CodeInternal, "cannot read %s", rel)
		}
		last := rerr != nil // short or empty read: end of file
		if n > 0 || total == 0 {
			if total+int64(n) > MaxTransferBytes {
				return nil, tErr(api.CodeTooLarge, "%s exceeds %d MiB", rel, MaxTransferBytes>>20)
			}
			wp := api.WorkspaceWriteFileParams{
				Workspace: p.Workspace, Path: remote, Offset: total, Truncate: total == 0,
				Mode: mode, Length: n, SHA256: chunkSum(buf[:n]), Confirm: p.Confirm,
			}
			if _, err := o.WorkspaceWriteFile(ctx, cname, wp, buf[:n]); err != nil {
				return nil, err
			}
			total += int64(n)
		}
		if last {
			break
		}
	}
	return &TransferResult{Local: rel, Remote: remote, Bytes: total}, nil
}

// Download fetches a workspace file into the project directory (mode 0644), writing a temporary
// file in the same directory and renaming it into place, so a failed download leaves nothing.
func (o *Orch) Download(ctx context.Context, cname, projectDir string, p DownloadParams) (*TransferResult, error) {
	if p.Remote == "" {
		return nil, tErr(api.CodeBadRequest, "remote path required")
	}
	local := p.Local
	if local == "" {
		local = path.Base(strings.ReplaceAll(p.Remote, `\`, "/"))
	}
	rel, err := LocalPath(projectDir, local)
	if err != nil {
		return nil, err
	}
	root, err := openProject(projectDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := noSymlinks(root, rel); err != nil {
		return nil, err
	}
	lp := filepath.FromSlash(rel)
	if st, err := root.Lstat(lp); err == nil {
		if !st.Mode().IsRegular() {
			return nil, tErr(api.CodeBadRequest, "%s exists and is not a regular file", rel)
		}
		if !p.Overwrite {
			return nil, tErr(api.CodeConflict, "%s already exists; ask for overwrite to replace it", rel)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, tErr(api.CodeForbidden, "%s is not accessible", rel)
	}
	if dir := filepath.Dir(lp); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return nil, tErr(api.CodeForbidden, "cannot create the folder for %s", rel)
		}
	}
	tmpName := filepath.Join(filepath.Dir(lp), fmt.Sprintf(".mirafleet-dl-%x", chunkSumShort(rel, p.Remote)))
	tmp, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, tErr(api.CodeConflict, "cannot create a temporary file for %s", rel)
	}
	done := false
	defer func() {
		tmp.Close()
		if !done {
			_ = root.Remove(tmpName)
		}
	}()

	var off int64
	for {
		r, body, err := o.WorkspaceReadFile(ctx, cname, api.WorkspaceReadFileParams{Workspace: p.Workspace, Path: p.Remote, Offset: off})
		if err != nil {
			return nil, err
		}
		if r.Size > MaxTransferBytes || off+int64(len(body)) > MaxTransferBytes {
			return nil, tErr(api.CodeTooLarge, "the remote file exceeds %d MiB", MaxTransferBytes>>20)
		}
		if r.Offset != off || r.Length != len(body) || (len(body) == 0 && !r.EOF) {
			return nil, tErr(api.CodeUnavailable, "central sent an unexpected reply")
		}
		if _, err := tmp.Write(body); err != nil {
			return nil, tErr(api.CodeInternal, "cannot write %s", rel)
		}
		off += int64(len(body))
		if r.EOF {
			if off != r.Size {
				return nil, tErr(api.CodeUnavailable, "the download of %s ended early (%d of %d bytes)", p.Remote, off, r.Size)
			}
			break
		}
	}
	if err := tmp.Close(); err != nil {
		return nil, tErr(api.CodeInternal, "cannot write %s", rel)
	}
	if err := root.Rename(tmpName, lp); err != nil {
		return nil, tErr(api.CodeInternal, "cannot move the download into place at %s", rel)
	}
	done = true
	return &TransferResult{Local: rel, Remote: p.Remote, Bytes: off}, nil
}

// chunkSumShort names the temporary file after the transfer so concurrent downloads of other
// files do not collide.
func chunkSumShort(a, b string) []byte {
	s := sha256.Sum256([]byte(a + "\x00" + b))
	return s[:6]
}
