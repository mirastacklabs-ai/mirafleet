//go:build linux

package hostx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// Cleanup defaults (docs/M7.md Q5).
const (
	DefaultDeleteAfter = 30 * 24 * time.Hour
	DefaultWarnBefore  = 3 * 24 * time.Hour
	touchEvery         = time.Minute // last-used is persisted at most this often while a session is busy
)

// ErrBusy is returned when a session is in use and the request would remove it.
var ErrBusy = errors.New("session is in use")

// ErrNoSession is returned for an unknown session id.
var ErrNoSession = errors.New("no such session")

// SessionsConfig configures a Manager.
type SessionsConfig struct {
	Root        string                      // parent of the per-session dirs (default Paths.SessionsRoot())
	StateDir    string                      // session metadata lives in StateDir/sessions
	DeleteAfter time.Duration               // 0 selects DefaultDeleteAfter; negative never deletes
	WarnBefore  time.Duration               // 0 selects DefaultWarnBefore
	Busy        func(sessionID string) bool // true while the session has a running job (set by the exec layer)
	Files       FilesOptions
	Now         func() time.Time
}

// meta is the persisted record of one session.
type meta struct {
	ID       string    `json:"id"`
	Dir      string    `json:"dir"` // the directory the tools are confined to
	Own      bool      `json:"own"` // Dir is the session's own directory (deleted with it); false for a workdir
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used"`
	HeldPID  int       `json:"held_pid,omitempty"` // process holding a live handle
}

// Manager owns the session directories of one runner user.
type Manager struct {
	cfg SessionsConfig

	mu    sync.Mutex
	held  map[string]int       // slug -> open handles
	saved map[string]time.Time // slug -> last persisted last_used
}

// NewManager creates the root and state directories (0700).
func NewManager(cfg SessionsConfig) (*Manager, error) {
	if cfg.Root == "" || cfg.StateDir == "" {
		return nil, errors.New("hostx: Root and StateDir are required")
	}
	if !filepath.IsAbs(cfg.Root) || !filepath.IsAbs(cfg.StateDir) {
		return nil, errors.New("hostx: Root and StateDir must be absolute")
	}
	if cfg.DeleteAfter == 0 {
		cfg.DeleteAfter = DefaultDeleteAfter
	}
	if cfg.WarnBefore == 0 {
		cfg.WarnBefore = DefaultWarnBefore
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.Root, cfg.StateDir = filepath.Clean(cfg.Root), filepath.Clean(cfg.StateDir)
	for _, d := range []string{cfg.Root, cfg.metaDir()} {
		if err := ensureDir(d); err != nil {
			return nil, err
		}
	}
	return &Manager{cfg: cfg, held: map[string]int{}, saved: map[string]time.Time{}}, nil
}

func (c SessionsConfig) metaDir() string { return filepath.Join(c.StateDir, "sessions") }

var rawID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Slug maps a session id to its directory name: the id itself when it is a short safe name,
// else "h_" plus a hash. It never contains a separator and never starts with a dot.
func Slug(id string) string {
	if rawID.MatchString(id) && !strings.HasPrefix(id, "h_") {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "h_" + hex.EncodeToString(sum[:12])
}

func (m *Manager) metaPath(slug string) string { return filepath.Join(m.cfg.metaDir(), slug+".json") }

func (m *Manager) load(slug string) (*meta, error) {
	b, err := os.ReadFile(m.metaPath(slug))
	if err != nil {
		return nil, err
	}
	var mt meta
	if err := json.Unmarshal(b, &mt); err != nil {
		return nil, fmt.Errorf("hostx: corrupt session record %s: %w", slug, err)
	}
	return &mt, nil
}

func (m *Manager) save(slug string, mt *meta) error {
	b, err := json.Marshal(mt)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.cfg.metaDir(), ".tmp-")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return errors.Join(werr, cerr)
	}
	if err := os.Rename(tmp.Name(), m.metaPath(slug)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	m.saved[slug] = mt.LastUsed
	return nil
}

// SessionDir is a handle on one session directory. Close it when the workspace closes; the
// directory stays.
type SessionDir struct {
	m    *Manager
	ID   string
	Dir  string // the confined directory (own dir or workdir)
	Own  bool
	slug string

	// Warning is set when the directory was close to deletion at the time it was reattached
	// (surface it in the next tool result); it is cleared by use.
	Warning string

	once   sync.Once
	files  *Files
	ferr   error
	closed sync.Once
}

// Open creates or reattaches the session. workdir is "" for the session's own directory, else an
// absolute path of an existing directory (a shared checkout) the tools are confined to.
func (m *Manager) Open(id, workdir string) (*SessionDir, error) {
	if id == "" || len(id) > 256 || strings.ContainsRune(id, 0) {
		return nil, errors.New("hostx: invalid session id")
	}
	slug := Slug(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.cfg.Now()

	mt, err := m.load(slug)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if mt == nil {
		mt = &meta{ID: id, Created: now}
	}
	warn := m.warningFor(mt, now)

	if workdir == "" {
		dir := filepath.Join(m.cfg.Root, slug)
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		fi, err := os.Lstat(dir)
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() { // includes a symlink planted in the root
			return nil, fmt.Errorf("hostx: %s is not a directory", dir)
		}
		mt.Dir, mt.Own = dir, true
	} else {
		if !filepath.IsAbs(workdir) {
			return nil, errors.New("hostx: workdir must be an absolute path")
		}
		real, err := filepath.EvalSymlinks(filepath.Clean(workdir))
		if err != nil {
			return nil, fmt.Errorf("hostx: workdir: %w", err)
		}
		fi, err := os.Stat(real)
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("hostx: workdir %s is not a directory", workdir)
		}
		mt.Dir, mt.Own = real, false
	}
	mt.LastUsed = now
	mt.HeldPID = os.Getpid()
	if err := m.save(slug, mt); err != nil {
		return nil, err
	}
	m.held[slug]++
	return &SessionDir{m: m, ID: id, Dir: mt.Dir, Own: mt.Own, slug: slug, Warning: warn}, nil
}

func (m *Manager) warningFor(mt *meta, now time.Time) string {
	if m.cfg.DeleteAfter < 0 || !mt.Own {
		return ""
	}
	left := mt.LastUsed.Add(m.cfg.DeleteAfter).Sub(now)
	if left > m.cfg.WarnBefore {
		return ""
	}
	if left < 0 {
		left = 0
	}
	return fmt.Sprintf("this session directory is deleted in %s because it has not been used for a while; using it keeps it", left.Round(time.Hour))
}

// Touch records use of the session (cheap; persisted at most once a minute).
func (s *SessionDir) Touch() {
	s.Warning = ""
	m := s.m
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.cfg.Now()
	if now.Sub(m.saved[s.slug]) < touchEvery {
		return
	}
	if mt, err := m.load(s.slug); err == nil {
		mt.LastUsed = now
		_ = m.save(s.slug, mt)
	}
}

// Files returns the confined file tools for the session.
func (s *SessionDir) Files() (*Files, error) {
	s.once.Do(func() { s.files, s.ferr = OpenFiles(s.Dir, s.m.cfg.Files) })
	return s.files, s.ferr
}

// Close releases the handle; the directory stays. Safe to call more than once.
func (s *SessionDir) Close() error {
	s.closed.Do(func() {
		if s.files != nil {
			s.files.Close()
		}
		m := s.m
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.held[s.slug]--; m.held[s.slug] <= 0 {
			delete(m.held, s.slug)
		}
		if mt, err := m.load(s.slug); err == nil {
			mt.LastUsed = m.cfg.Now()
			if m.held[s.slug] == 0 {
				mt.HeldPID = 0
			}
			_ = m.save(s.slug, mt)
		}
	})
	return nil
}

// Info describes one stored session.
type Info struct {
	ID        string
	Slug      string
	Dir       string
	Own       bool // false: a shared workdir, never deleted by this package
	Created   time.Time
	LastUsed  time.Time
	ExpiresAt time.Time // zero when cleanup is off or the dir is a workdir
	Warn      bool      // inside the warning window
	Busy      bool
	Size      int64 // bytes under Dir (own dirs only)
}

// busy reports a live handle here, a live holder process, or a running job. Caller holds m.mu.
func (m *Manager) busy(slug string, mt *meta) bool {
	if m.held[slug] > 0 {
		return true
	}
	if mt.HeldPID != 0 && mt.HeldPID != os.Getpid() && pidAlive(mt.HeldPID) {
		return true
	}
	return m.cfg.Busy != nil && m.cfg.Busy(mt.ID)
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (m *Manager) info(slug string, mt *meta, now time.Time, size bool) Info {
	in := Info{ID: mt.ID, Slug: slug, Dir: mt.Dir, Own: mt.Own, Created: mt.Created, LastUsed: mt.LastUsed, Busy: m.busy(slug, mt)}
	if mt.Own && m.cfg.DeleteAfter > 0 {
		in.ExpiresAt = mt.LastUsed.Add(m.cfg.DeleteAfter)
		in.Warn = in.ExpiresAt.Sub(now) <= m.cfg.WarnBefore
	}
	if size && mt.Own {
		in.Size = dirSize(mt.Dir)
	}
	return in
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, e := d.Info(); e == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// List returns the stored sessions, newest use first. withSize also totals each own directory.
func (m *Manager) List(withSize bool) ([]Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.list(withSize)
}

func (m *Manager) list(withSize bool) ([]Info, error) {
	ents, err := os.ReadDir(m.cfg.metaDir())
	if err != nil {
		return nil, err
	}
	now := m.cfg.Now()
	var out []Info
	for _, e := range ents {
		slug, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || strings.HasPrefix(slug, ".") {
			continue
		}
		mt, err := m.load(slug)
		if err != nil {
			continue
		}
		out = append(out, m.info(slug, mt, now, withSize))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUsed.After(out[j].LastUsed) })
	return out, nil
}

// Delete removes the session record and, for an own directory, the directory. A workdir is
// never touched. A session in use is refused with ErrBusy.
func (m *Manager) Delete(id string) error {
	slug := Slug(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	mt, err := m.load(slug)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNoSession
	} else if err != nil {
		return err
	}
	if m.busy(slug, mt) {
		return ErrBusy
	}
	return m.remove(slug, mt)
}

// remove deletes the data of one session. Caller holds m.mu and has checked busy.
func (m *Manager) remove(slug string, mt *meta) error {
	if mt.Own {
		// The path is Root/slug by construction; never follow a symlink swapped in.
		dir := filepath.Join(m.cfg.Root, slug)
		if mt.Dir != dir {
			return fmt.Errorf("hostx: session %s records an unexpected directory", slug)
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	delete(m.saved, slug)
	if err := os.Remove(m.metaPath(slug)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Do runs one fs op in the session, records the use and returns the expiry warning (if any,
// once) to put in the tool result.
func (s *SessionDir) Do(ctx context.Context, op transport.FSOp, body []byte) (res any, warning string, err error) {
	f, err := s.Files()
	if err != nil {
		return nil, "", err
	}
	warning = s.Warning
	s.Touch()
	res, err = f.Do(ctx, op, body)
	return res, warning, err
}
