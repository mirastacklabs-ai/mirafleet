package gui

// The screenshot server of `scripts/dev.sh gui-shots` (docs/M4.md Q12, S16). It is a test only so
// that it can reuse the harness (a real service on a temp SQLite file, seeded rows, a signed-in
// session). It skips unless MIRAFLEET_GUI_SHOTS_ADDR is set, so it never runs in the gate.
//
// It serves the GUI over plain HTTP on that address (loopback inside the container) and writes
// the list of screens to MIRAFLEET_GUI_SHOTS_URLS, one "name path" per line. A wrapper in front
// of the handler stands in for the browser's cookies: it adds the admin session cookie, a fixed
// Host the allowlist accepts, and the theme from the "_theme" query parameter. "_anon=1" leaves
// the session off (login, 404); "_setup=1" talks to a second server that has no admin yet.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store"
)

func TestGUIShotsServe(t *testing.T) {
	addr := os.Getenv("MIRAFLEET_GUI_SHOTS_ADDR")
	if addr == "" {
		t.Skip("MIRAFLEET_GUI_SHOTS_ADDR not set (screenshot server for scripts/dev.sh gui-shots)")
	}
	// The served GUI carries an admin session cookie: loopback only.
	if ap, err := netip.ParseAddrPort(addr); err != nil || !ap.Addr().IsLoopback() {
		t.Fatalf("MIRAFLEET_GUI_SHOTS_ADDR %q must be a loopback ip:port", addr)
	}
	listFile := os.Getenv("MIRAFLEET_GUI_SHOTS_URLS")
	if listFile == "" {
		t.Fatal("MIRAFLEET_GUI_SHOTS_URLS not set")
	}

	h := newAdminHarness(t, withRegistrationSecret(),
		withConfig(func(c *Config) { c.SSHAdvertise = "central-01.example.org:2222" }))
	h.mkFleet("prod")
	h.mkFleet("dev")
	if _, err := h.svc.SetFleetConfirm(context.Background(), h.adm, service.SourceGUI, "prod", true); err != nil {
		t.Logf("set fleet guard: %v", err)
	}
	h.addRunner("prod", "db-1", true, map[string]string{"role": "database", "zone": "eu-1"})
	h.addRunner("prod", "web-2", false, nil)
	h.addRunner("dev", "build-1", true, nil)
	alice := h.operator("alice", "")
	h.operator("bob", "")
	runner := h.runner("dev", "build-2")
	w := h.ws("dev", runner, alice, "feature-x", "ubuntu:24.04")
	if _, err := h.svc.CreateToken(context.Background(), h.adm, service.SourceGUI, service.TokenParams{Fleet: "prod", Approve: true}); err != nil {
		t.Fatal(err)
	}
	seedAudit(t, h,
		store.AuditEvent{Time: auditT0, Action: "fleet.create", Fleet: "prod"},
		store.AuditEvent{Time: auditT0.Add(time.Minute), Action: "runner.approve", Fleet: "prod", Runner: "db-1"},
		store.AuditEvent{Time: auditT0.Add(2 * time.Minute), Action: "runner.revoke", Fleet: "dev", Runner: "build-1", Outcome: "denied"},
	)
	tokID := ""
	if m := regexp.MustCompile(`/tokens/([^/"]+)/revoke`).FindStringSubmatch(h.get("/tokens").Body.String()); m != nil {
		tokID = m[1]
	}

	fresh := newGUIHarness(t) // no admin yet: the setup screen
	if _, _, err := fresh.srv.PrepareSetup(context.Background()); err != nil {
		t.Fatal(err)
	}

	screens := [][2]string{
		{"overview", "/"}, {"fleets", "/fleets"}, {"fleet-new", "/fleets/new"}, {"fleet-detail", "/fleets/prod"},
		{"fleet-workspace", "/fleets/prod/workspace"}, {"fleet-guard", "/fleets/prod/guard"},
		{"runners", "/runners"}, {"runner-detail", "/runners/prod/db-1"}, {"runner-revoke", "/runners/prod/db-1/revoke"},
		{"tokens", "/tokens"}, {"token-new", "/tokens/new"},
		{"operators", "/operators"}, {"operator-rotate", "/operators/rotate"}, {"operator-revoke", "/operators/bob/revoke"},
		{"workspaces", "/workspaces"}, {"workspace-kill", "/workspaces/" + w.WsID + "/kill"},
		{"audit", "/audit"}, {"audit-filtered", "/audit?action=runner."}, {"account", "/account"},
		{"login", "/login?_anon=1"}, {"setup", "/setup?_setup=1"}, {"not-found", "/nope?_anon=1"},
	}
	if tokID != "" {
		screens = append(screens, [2]string{"token-revoke", "/tokens/" + tokID + "/revoke"})
	}
	var list strings.Builder
	for _, s := range screens {
		fmt.Fprintf(&list, "%s %s\n", s[0], s[1])
	}
	if err := os.WriteFile(listFile, []byte(list.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	front := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		theme, anon, setup := q.Get("_theme"), q.Get("_anon") == "1", q.Get("_setup") == "1"
		q.Del("_theme")
		q.Del("_anon")
		q.Del("_setup")
		r.URL.RawQuery = q.Encode()
		r.RequestURI = r.URL.RequestURI()
		r.Host = testHost
		r.Header.Del("Cookie")
		srv, tok := h.srv, h.tok
		if setup {
			srv, tok = fresh.srv, ""
		}
		if !anon && tok != "" {
			r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: tok})
		}
		if theme == "light" || theme == "dark" {
			r.AddCookie(&http.Cookie{Name: ThemeCookieName, Value: theme})
		}
		srv.Handler().ServeHTTP(w, r)
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: front, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	defer hs.Close()
	t.Logf("gui-shots server on %s", ln.Addr())

	chrome, out := os.Getenv("MIRAFLEET_GUI_SHOTS_CHROME"), os.Getenv("MIRAFLEET_GUI_SHOTS_OUT")
	if chrome == "" || out == "" { // serve only, for a manual look; a forgotten run ends itself
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		select {
		case <-sig:
		case <-time.After(15 * time.Minute):
		}
		return
	}
	shoot(t, chrome, out, "http://"+ln.Addr().String(), screens)
}

// shoot drives headless Chromium over the DevTools protocol: every screen, light and dark, at
// 1280 and 390 px, as a full-page PNG named <screen>-<theme>-<width>.png in out.
func shoot(t *testing.T, chrome, out, base string, screens [][2]string) {
	t.Helper()
	profile := t.TempDir()
	cmd := exec.Command(chrome, "--no-sandbox", "--disable-gpu", "--hide-scrollbars", "--user-data-dir="+profile,
		"--remote-debugging-port=9223", "about:blank")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	var wsURL string
	for i := 0; i < 100 && wsURL == ""; i++ {
		time.Sleep(200 * time.Millisecond)
		resp, err := http.Get("http://127.0.0.1:9223/json/version")
		if err != nil {
			continue
		}
		var v struct {
			WS string `json:"webSocketDebuggerUrl"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		wsURL = v.WS
	}
	if wsURL == "" {
		t.Fatal("Chromium did not come up")
	}
	c, err := dialCDP(wsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.conn.Close()
	var target struct {
		TargetID string `json:"targetId"`
	}
	c.call(t, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &target)
	var att struct {
		SessionID string `json:"sessionId"`
	}
	c.call(t, "", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &att)
	sess := att.SessionID

	n := 0
	for _, theme := range []string{"light", "dark"} {
		for _, width := range []int{1280, 390} {
			for _, s := range screens {
				sep := "?"
				if strings.Contains(s[1], "?") {
					sep = "&"
				}
				metrics := func(h int) map[string]any {
					return map[string]any{"width": width, "height": h, "deviceScaleFactor": 1, "mobile": width < 600}
				}
				c.call(t, sess, "Emulation.setDeviceMetricsOverride", metrics(900), nil)
				c.call(t, sess, "Page.navigate", map[string]any{"url": base + s[1] + sep + "_theme=" + theme}, nil)
				var ev struct {
					Result struct {
						Value float64 `json:"value"`
					} `json:"result"`
				}
				c.call(t, sess, "Runtime.evaluate", map[string]any{"awaitPromise": true, "returnByValue": true,
					"expression": `new Promise(r => document.readyState === "complete" ? r() : addEventListener("load", r))` +
						`.then(() => document.fonts.ready).then(() => document.documentElement.scrollHeight)`}, &ev)
				h := int(ev.Result.Value)
				if h < 400 {
					h = 400
				}
				if h > 5000 {
					h = 5000
				}
				c.call(t, sess, "Emulation.setDeviceMetricsOverride", metrics(h), nil)
				var shot struct {
					Data string `json:"data"`
				}
				c.call(t, sess, "Page.captureScreenshot", map[string]any{"format": "png"}, &shot)
				png, err := base64.StdEncoding.DecodeString(shot.Data)
				if err != nil || len(png) == 0 {
					t.Fatalf("%s: empty screenshot", s[0])
				}
				if err := os.WriteFile(filepath.Join(out, fmt.Sprintf("%s-%s-%d.png", s[0], theme, width)), png, 0o644); err != nil {
					t.Fatal(err)
				}
				n++
			}
		}
	}
	t.Logf("screenshots: %d", n)
}

// cdp is a minimal DevTools-protocol client over a hand-rolled WebSocket (client frames are
// masked, server frames may be fragmented); it exists so that the tool needs no new dependency.
type cdp struct {
	conn net.Conn
	br   *bufio.Reader
	id   int
}

func dialCDP(raw string) (*cdp, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		u.RequestURI(), u.Host, base64.StdEncoding.EncodeToString(key))
	if _, err := io.WriteString(conn, req); err != nil {
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("websocket upgrade: %s", resp.Status)
	}
	return &cdp{conn: conn, br: br}, nil
}

func (c *cdp) write(payload []byte) error {
	var hdr []byte
	hdr = append(hdr, 0x81)
	switch n := len(payload); {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n < 65536:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127, 0, 0, 0, 0, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	mask := make([]byte, 4)
	_, _ = rand.Read(mask)
	hdr = append(hdr, mask...)
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	_, err := c.conn.Write(append(hdr, masked...))
	return err
}

// read returns the next complete text message.
func (c *cdp) read() ([]byte, error) {
	var msg []byte
	for {
		var h [2]byte
		if _, err := io.ReadFull(c.br, h[:]); err != nil {
			return nil, err
		}
		fin, op, n := h[0]&0x80 != 0, h[0]&0x0f, uint64(h[1]&0x7f)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				return nil, err
			}
			n = uint64(b[0])<<8 | uint64(b[1])
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				return nil, err
			}
			n = 0
			for _, x := range b {
				n = n<<8 | uint64(x)
			}
		}
		if n > 64<<20 {
			return nil, fmt.Errorf("websocket frame of %d bytes", n)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(c.br, buf); err != nil {
			return nil, err
		}
		if op == 8 {
			return nil, io.EOF
		}
		if op == 9 || op == 10 { // ping/pong: nothing to do
			continue
		}
		msg = append(msg, buf...)
		if fin {
			return msg, nil
		}
	}
}

// call sends one command and waits for its reply (events in between are dropped).
func (c *cdp) call(t *testing.T, session, method string, params, result any) {
	t.Helper()
	c.id++
	req := map[string]any{"id": c.id, "method": method, "params": params}
	if session != "" {
		req["sessionId"] = session
	}
	b, _ := json.Marshal(req)
	if err := c.write(b); err != nil {
		t.Fatal(err)
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	for {
		m, err := c.read()
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		var r struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(m, &r) != nil || r.ID != c.id {
			continue
		}
		if r.Error != nil {
			t.Fatalf("%s: %s", method, r.Error.Message)
		}
		if result != nil {
			_ = json.Unmarshal(r.Result, result)
		}
		return
	}
}
