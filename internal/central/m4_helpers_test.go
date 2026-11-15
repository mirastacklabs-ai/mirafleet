package central

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/actions"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/gui"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store/sqlite"
)

// Helpers of TestM4Done (m4_done_test.go): an offset clock, a central whose database path is
// known, and a browser stand-in (no JavaScript) that talks to the GUI over TLS pinned to the
// GUI's self-signed certificate.

// m4Actions are the actions the prod runner advertises.
const m4Actions = `
actions:
  tail-logs:
    run: ["/usr/bin/tail", "-n", "{{lines}}", "/var/log/syslog"]
    params: { lines: { pattern: "[0-9]{1,4}" } }
  long:
    run: ["/bin/sleep", "30"]
    timeout: 60s
`

// m4Clock is real time plus an offset the test moves. Every advance must be a whole number of
// minutes so the registration-code steps stay aligned with real time.
type m4Clock struct{ off atomic.Int64 }

func (c *m4Clock) now() time.Time { return time.Now().Add(time.Duration(c.off.Load())) }

func (c *m4Clock) advance(d time.Duration) {
	if d%time.Minute != 0 {
		panic("m4Clock: advance by whole minutes only")
	}
	c.off.Add(int64(d))
}

// newM4Env is newEnvOpts with a service on the injected clock and a database file whose path the
// test knows (the GUI-restart step opens a second handle on it).
func newM4Env(t *testing.T, clock func() time.Time) (*env, string) {
	t.Helper()
	ctx := context.Background()
	e := &env{t: t, dir: t.TempDir(), ids: map[string]int64{}, admin: auth.LocalAdmin(os.Getuid())}
	_, err := keys.Generate(e.dir, "host", "host")
	must(t, err)
	e.hostSigner, err = keys.LoadSigner(filepath.Join(e.dir, "host"))
	must(t, err)
	e.knownHosts = filepath.Join(e.dir, "known_hosts")
	must(t, os.WriteFile(e.knownHosts, []byte("* "+keys.PubLine(e.hostSigner.PublicKey(), "")+"\n"), 0o600))
	e.acts, err = actions.Parse([]byte(m4Actions))
	must(t, err)

	dbPath := filepath.Join(t.TempDir(), "mirafleet.db")
	e.db, err = sqlite.Open(ctx, dbPath)
	must(t, err)
	t.Cleanup(func() { e.db.Close() })
	e.reg, err = registry.New(registry.Options{StaleAfter: 45 * time.Second, Logf: e.logf})
	must(t, err)
	e.ops = NewOperatorConns()
	e.svc, err = service.New(service.Config{Store: e.db, Policy: policy.TablePolicy{}, Live: e.reg,
		HostKey: e.hostSigner.PublicKey(), Now: clock, OperatorConns: e.ops, Logf: e.logf, ReaperInterval: time.Hour})
	must(t, err)
	e.startCentral("127.0.0.1:0")
	return e, dbPath
}

// ---- the browser ----

// m4Resp is one response the GUI sent (headers and body as text), for the hygiene step.
type m4Resp struct {
	Seq          int
	Method, Path string
	Status       int
	Text         string
}

// m4World is what every browser of a test shares: the GUI's address, the pinned certificate, the
// local addresses the browsers dialed from (the "remote" the GUI sees) and every response.
type m4World struct {
	mu      sync.Mutex
	base    string
	pin     [32]byte
	locals  map[string]bool
	resps   []m4Resp
	cookies map[string]bool // every session cookie value a response set
}

func newM4World(pinDER []byte) *m4World {
	return &m4World{pin: sha256.Sum256(pinDER), locals: map[string]bool{}, cookies: map[string]bool{}}
}

func (w *m4World) setBase(u string) { w.mu.Lock(); w.base = u; w.mu.Unlock() }

func (w *m4World) baseURL() string { w.mu.Lock(); defer w.mu.Unlock(); return w.base }

func (w *m4World) localAddrs() map[string]bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]bool{}
	for k := range w.locals {
		out[k] = true
	}
	return out
}

func (w *m4World) responses() []m4Resp {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]m4Resp(nil), w.resps...)
}

func (w *m4World) sessionCookies() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for c := range w.cookies {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func (w *m4World) record(method, path string, p *m4Page) {
	var hb strings.Builder
	p.Header.Write(&hb)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.resps = append(w.resps, m4Resp{Seq: len(w.resps), Method: method, Path: path, Status: p.Status, Text: hb.String() + "\n" + p.Body})
	for _, c := range p.Cookies {
		if c.Name == gui.SessionCookieName && c.Value != "" {
			w.cookies[c.Value] = true
		}
	}
}

// m4Page is one response.
type m4Page struct {
	Status  int
	Header  http.Header
	Cookies []*http.Cookie
	Body    string
	Seq     int // index in the world's response list
}

func (p *m4Page) loc() string { return p.Header.Get("Location") }

// m4Browser is a browser without JavaScript: a cookie jar, form posts with a CSRF token taken
// from the page, same-origin fetch metadata, no automatic redirects.
type m4Browser struct {
	t   *testing.T
	w   *m4World
	cl  *http.Client
	jar *cookiejar.Jar
}

func (w *m4World) newBrowser(t *testing.T) *m4Browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	must(t, err)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			// The self-signed certificate is trusted by fingerprint alone: normal chain
			// verification is replaced by this pin.
			InsecureSkipVerify: true,
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				if len(raw) == 0 || sha256.Sum256(raw[0]) != w.pin {
					return errors.New("m4: certificate does not match the pinned fingerprint")
				}
				return nil
			},
		},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err == nil {
				w.mu.Lock()
				w.locals[c.LocalAddr().String()] = true
				w.mu.Unlock()
			}
			return c, err
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	cl := &http.Client{Transport: tr, Jar: jar, Timeout: 5 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &m4Browser{t: t, w: w, cl: cl, jar: jar}
}

// at returns the browser with its failures reported on t (a subtest); the cookie jar is shared.
func (b *m4Browser) at(t *testing.T) *m4Browser { c := *b; c.t = t; return &c }

// do sends one request. hdr overrides headers ("" deletes one; "Host" sets the Host header).
func (b *m4Browser) do(method, path string, form url.Values, hdr map[string]string) *m4Page {
	b.t.Helper()
	base := b.w.baseURL()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, base+path, body)
	must(b.t, err)
	req.Header.Set("User-Agent", "M4Browser/1.0")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", base)
	}
	for k, v := range hdr {
		switch {
		case k == "Host":
			req.Host = v
		case v == "":
			req.Header.Del(k)
		default:
			req.Header.Set(k, v)
		}
	}
	resp, err := b.cl.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	must(b.t, err)
	p := &m4Page{Status: resp.StatusCode, Header: resp.Header.Clone(), Cookies: resp.Cookies(), Body: string(data)}
	b.w.record(method, path, p)
	p.Seq = len(b.w.responses()) - 1
	return p
}

func (b *m4Browser) get(path string) *m4Page { b.t.Helper(); return b.do("GET", path, nil, nil) }

// follow follows redirects (at most five).
func (b *m4Browser) follow(p *m4Page) *m4Page {
	b.t.Helper()
	for i := 0; i < 5 && p.Status >= 300 && p.Status < 400 && p.loc() != ""; i++ {
		p = b.get(p.loc())
	}
	return p
}

var m4CSRF = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)

// token returns the hidden _csrf value of the page at path.
func (b *m4Browser) token(path string) string {
	b.t.Helper()
	p := b.get(path)
	m := m4CSRF.FindStringSubmatch(p.Body)
	if m == nil {
		b.t.Fatalf("GET %s: status %d, no _csrf field", path, p.Status)
	}
	return m[1]
}

// post fetches the form page for its token, then posts form (plus _csrf) to target.
func (b *m4Browser) post(formPage, target string, form url.Values) *m4Page {
	b.t.Helper()
	f := url.Values{}
	for k, v := range form {
		f[k] = v
	}
	f.Set("_csrf", b.token(formPage))
	return b.do("POST", target, f, nil)
}

func (b *m4Browser) cookie(name string) string {
	u, err := url.Parse(b.w.baseURL())
	must(b.t, err)
	for _, c := range b.jar.Cookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func (b *m4Browser) session() string { return b.cookie(gui.SessionCookieName) }

func (b *m4Browser) setCookie(name, value string) {
	u, err := url.Parse(b.w.baseURL())
	must(b.t, err)
	b.jar.SetCookies(u, []*http.Cookie{{Name: name, Value: value, Path: "/", Secure: true}})
}

// alive reports whether the session still works: a normal page answers 200.
func (b *m4Browser) alive() bool { b.t.Helper(); return b.get("/account").Status == http.StatusOK }

// refused reports whether p sent the browser to /login or /setup.
func m4Refused(p *m4Page) bool {
	return p.Status == http.StatusSeeOther && (strings.HasPrefix(p.loc(), "/login") || strings.HasPrefix(p.loc(), "/setup"))
}

// ---- reading the GUI's pages ----

var m4RowID = regexp.MustCompile(`<summary>Row (\d+)</summary>`)

// m4AuditIDs returns the audit row ids of an audit page, in page order.
func m4AuditIDs(body string) []int64 {
	var out []int64
	for _, m := range m4RowID.FindAllStringSubmatch(body, -1) {
		var n int64
		fmt.Sscan(m[1], &n)
		out = append(out, n)
	}
	return out
}

// m4Link returns the (unescaped) href of the link with the given text, "" if absent.
func m4Link(body, text string) string {
	m := regexp.MustCompile(`href="([^"]+)">` + regexp.QuoteMeta(text) + `</a>`).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.ReplaceAll(m[1], "&amp;", "&")
}

func m4EqualIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- the session table, read straight from the SQLite file ----

type m4SessRow struct {
	Hash                               []byte
	CreatedMS, LastActiveMS, ExpiresMS int64
}

func m4Sessions(t *testing.T, dbPath string) []m4SessRow {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	must(t, err)
	defer db.Close()
	rows, err := db.Query(`SELECT token_hash, created_at, last_active_at, expires_at FROM gui_sessions ORDER BY id`)
	must(t, err)
	defer rows.Close()
	var out []m4SessRow
	for rows.Next() {
		var r m4SessRow
		must(t, rows.Scan(&r.Hash, &r.CreatedMS, &r.LastActiveMS, &r.ExpiresMS))
		out = append(out, r)
	}
	must(t, rows.Err())
	return out
}

func m4PasswordHashes(t *testing.T, dbPath string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	must(t, err)
	defer db.Close()
	rows, err := db.Query(`SELECT pw_hash FROM gui_admins`)
	must(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		must(t, rows.Scan(&h))
		out = append(out, h)
	}
	must(t, rows.Err())
	return out
}

func m4HasSession(rows []m4SessRow, cookie string) bool {
	h := sha256.Sum256([]byte(cookie))
	for _, r := range rows {
		if bytes.Equal(r.Hash, h[:]) {
			return true
		}
	}
	return false
}

// m4Encodings lists the ways a secret could show up in a response or a table.
func m4Encodings(secret []byte) []string {
	return []string{string(secret), hex.EncodeToString(secret), base64.StdEncoding.EncodeToString(secret),
		base64.RawStdEncoding.EncodeToString(secret), base64.URLEncoding.EncodeToString(secret),
		base64.RawURLEncoding.EncodeToString(secret)}
}
