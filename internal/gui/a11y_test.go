package gui

// Accessibility and layout rules that can be checked without a browser: WCAG contrast of every text token pair in app.css, light and dark; the 360 px layout
// rules; and the keyboard and landmark basics of every screen.

import (
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

type rgb struct{ r, g, b float64 }

func parseHex(t *testing.T, s string) rgb {
	t.Helper()
	if len(s) != 7 || s[0] != '#' {
		t.Fatalf("not a #RRGGBB colour: %q", s)
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		t.Fatalf("colour %q: %v", s, err)
	}
	return rgb{float64(v >> 16 & 255), float64(v >> 8 & 255), float64(v & 255)}
}

// luminance is the WCAG 2.x relative luminance.
func (c rgb) luminance() float64 {
	f := func(v float64) float64 {
		v /= 255
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*f(c.r) + 0.7152*f(c.g) + 0.0722*f(c.b)
}

func contrast(a, b rgb) float64 {
	la, lb := a.luminance(), b.luminance()
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// cssBlock returns the declarations of the first rule whose selector line starts with sel,
// searching from the first occurrence of start (so the dark rule inside @media can be told from
// the data-theme rule).
func cssBlock(t *testing.T, css, sel string) string {
	t.Helper()
	i := strings.Index(css, sel)
	if i < 0 {
		t.Fatalf("app.css has no %q rule", sel)
	}
	j := strings.Index(css[i:], "{")
	k := strings.Index(css[i:], "}")
	if j < 0 || k < j {
		t.Fatalf("app.css: malformed rule %q", sel)
	}
	return css[i+j+1 : i+k]
}

var cssTokenRE = regexp.MustCompile(`--([a-z-]+):\s*(#[0-9A-Fa-f]{6})`)

func cssTokens(t *testing.T, block string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, x := range cssTokenRE.FindAllStringSubmatch(block, -1) {
		m[x[1]] = strings.ToUpper(x[2])
	}
	return m
}

// TestS16ContrastAA computes the WCAG ratio of every foreground/background token pair that the
// stylesheet uses for text and requires AA (4.5:1; all text here is below the large-text size).
func TestS16ContrastAA(t *testing.T) {
	b, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	light := cssTokens(t, cssBlock(t, css, ":root {"))
	dark := cssTokens(t, cssBlock(t, css, ":root[data-theme=\"dark\"] {"))
	// The prefers-color-scheme block must carry the same values as the data-theme block.
	osDark := cssTokens(t, cssBlock(t, css, ":root:not([data-theme=\"light\"]) {"))
	for k, v := range osDark {
		if dark[k] != v {
			t.Errorf("dark token --%s: media query %s, data-theme %s", k, v, dark[k])
		}
	}
	surfaces := []string{"bg", "surface", "bg-band"}
	texts := []string{"text", "ink", "muted", "accent"}
	type pair struct{ fg, bg string }
	var pairs []pair
	for _, bg := range surfaces {
		for _, fg := range texts {
			pairs = append(pairs, pair{fg, bg})
		}
		pairs = append(pairs, pair{"bad", bg}) // field errors
	}
	pairs = append(pairs, pair{"btn-text", "btn-bg"}, pair{"muted", "off-bg"})
	for _, s := range []string{"ok", "warn", "off", "bad"} { // badges, flashes, danger buttons
		pairs = append(pairs, pair{s, s + "-bg"})
	}
	for _, s := range []string{"ok-bg", "warn-bg", "bad-bg"} { // body text inside those panels
		pairs = append(pairs, pair{"text", s}, pair{"ink", s})
	}
	for name, toks := range map[string]map[string]string{"light": light, "dark": dark} {
		for _, p := range pairs {
			fg, ok1 := toks[p.fg]
			bg, ok2 := toks[p.bg]
			if !ok1 || !ok2 {
				t.Errorf("%s: token --%s or --%s missing from app.css", name, p.fg, p.bg)
				continue
			}
			if r := contrast(parseHex(t, fg), parseHex(t, bg)); r < 4.5 {
				t.Errorf("%s: --%s %s on --%s %s = %.2f:1, below AA 4.5:1", name, p.fg, fg, p.bg, bg, r)
			}
		}
	}
}

// TestS16ContrastMath pins the formula against two known values.
func TestS16ContrastMath(t *testing.T) {
	if r := contrast(parseHex(t, "#000000"), parseHex(t, "#FFFFFF")); math.Abs(r-21) > 0.001 {
		t.Errorf("black on white = %.3f, want 21", r)
	}
	if r := contrast(parseHex(t, "#777777"), parseHex(t, "#FFFFFF")); math.Abs(r-4.478) > 0.01 {
		t.Errorf("#777 on white = %.3f, want about 4.48", r)
	}
}

// TestS16NarrowLayoutRules checks the 360 px rules: the viewport meta, the narrow breakpoints,
// the nav menu, a 16px field font (no iOS zoom on focus) and that every table scrolls inside a
// .scroll wrapper instead of widening the page.
func TestS16NarrowLayoutRules(t *testing.T) {
	b, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	for _, want := range []string{"@media (max-width: 600px)", "@media (max-width: 400px)", ".scroll { overflow: auto; max-width: 100%", ".menu"} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %q", want)
		}
	}
	narrow := css[strings.Index(css, "@media (max-width: 600px)"):]
	if !regexp.MustCompile(`input[^{]*\{[^}]*font-size:\s*16px`).MatchString(narrow) {
		t.Error("narrow rules do not raise form fields to 16px")
	}
	h := newAdminHarness(t)
	rec := h.get("/")
	if !strings.Contains(rec.Body.String(), `<meta name="viewport" content="width=device-width, initial-scale=1">`) {
		t.Error("layout has no responsive viewport meta")
	}
	if !strings.Contains(rec.Body.String(), `<details class="menu">`) {
		t.Error("layout has no <details> nav menu for narrow screens")
	}
	files, _ := os.ReadDir("templates")
	tableRE := regexp.MustCompile(`<table`)
	wrapRE := regexp.MustCompile(`<div class="scroll"><table`)
	for _, f := range files {
		if f.Name() == "styleguide.html" { // shows the bare component on purpose
			continue
		}
		src, _ := os.ReadFile("templates/" + f.Name())
		if n, w := len(tableRE.FindAll(src, -1)), len(wrapRE.FindAll(src, -1)); n != w {
			t.Errorf("%s: %d tables but %d inside <div class=\"scroll\">", f.Name(), n, w)
		}
	}
}

// TestS16KeyboardBasics: every screen has the skip link to a focusable main landmark, a labelled
// nav, one h1, and no positive tabindex; every form control has a label or a hidden label.
func TestS16KeyboardBasics(t *testing.T) {
	h := newAdminHarness(t, withRegistrationSecret())
	h.mkFleet("prod")
	h.addRunner("prod", "db-1", true, nil)
	paths := []string{"/", "/fleets", "/fleets/new", "/fleets/prod", "/fleets/prod/guard",
		"/runners", "/runners/prod/db-1", "/runners/prod/db-1/revoke", "/tokens", "/tokens/new", "/operators",
		"/operators/rotate", "/workspaces", "/audit", "/account"}
	idRE := regexp.MustCompile(`\sid="([^"]+)"`)
	forRE := regexp.MustCompile(`<label[^>]*\sfor="([^"]+)"`)
	ctlRE := regexp.MustCompile(`<(input|select|textarea)\b[^>]*>`)
	for _, p := range paths {
		rec := h.get(p)
		body := wantBody(t, rec, http.StatusOK, `href="#main"`, `<main id="main" tabindex="-1">`, `aria-label="Main"`)
		if n := strings.Count(body, "<h1"); n != 1 {
			t.Errorf("%s: %d <h1>, want 1", p, n)
		}
		if regexp.MustCompile(`tabindex="[1-9]`).MatchString(body) {
			t.Errorf("%s: positive tabindex", p)
		}
		labelled := map[string]bool{}
		for _, m := range forRE.FindAllStringSubmatch(body, -1) {
			labelled[m[1]] = true
		}
		for _, c := range ctlRE.FindAllString(body, -1) {
			if strings.Contains(c, `type="hidden"`) {
				continue
			}
			id := idRE.FindStringSubmatch(c)
			if (id == nil || !labelled[id[1]]) && !strings.Contains(c, "aria-label") {
				t.Errorf("%s: control without a label: %s", p, c)
			}
		}
	}
	for _, p := range []string{"/login", "/no-such-page"} {
		c := h.newClient()
		body := c.get(p).Body.String()
		if !strings.Contains(body, `href="#main"`) || !strings.Contains(body, `<main id="main" tabindex="-1">`) {
			t.Errorf("%s: no skip link or main landmark", p)
		}
	}
}
