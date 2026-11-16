package main

// GUI wiring for `central run`: flag parsing, the setup-token file and the
// start/stop of internal/gui. Linux only, like all of central.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/gui"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
)

// guiOff is the -gui-listen value that disables the GUI.
const guiOff = "off"

// guiOptions are the GUI settings of `central run`. The zero value (Listen "") means no GUI,
// which is what tests that only need the SSH server and the admin socket get.
type guiOptions struct {
	Listen    string // "" or "off": no GUI
	CertFile  string
	KeyFile   string
	Hosts     []string
	Advertise string
	Logf      func(format string, args ...any) // default logf
}

func (o guiOptions) enabled() bool { return o.Listen != "" && o.Listen != guiOff }

// stringList is a repeatable flag; each value may itself be a comma-separated list.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*l = append(*l, s)
		}
	}
	return nil
}

// centralRunConfig is the parsed `central run` command line.
type centralRunConfig struct {
	Data   string
	Listen string
	GUI    guiOptions
}

// parseCentralRun parses and validates the `central run` flags.
func parseCentralRun(args []string, errOut io.Writer) (centralRunConfig, error) {
	f := flag.NewFlagSet("central run", flag.ContinueOnError)
	if errOut != nil {
		f.SetOutput(errOut)
	}
	var hosts stringList
	data := f.String("data", defaultCentralData, "data directory")
	listen := f.String("listen", "0.0.0.0:2222", "SSH listen address")
	guiListen := f.String("gui-listen", gui.DefaultListen, "admin GUI listen address, or off")
	guiCert := f.String("gui-cert", "", "GUI TLS certificate (PEM); needs -gui-key")
	guiKey := f.String("gui-key", "", "GUI TLS key (PEM, mode 0600); needs -gui-cert")
	advertise := f.String("advertise", "", "SSH HOST:PORT shown in copy-paste hints")
	f.Var(&hosts, "gui-host", "host name the GUI accepts (repeatable or comma-separated)")
	if err := f.Parse(args); err != nil {
		return centralRunConfig{}, fmt.Errorf("%w: %v", errAdminUsage, err)
	}
	if f.NArg() > 0 {
		return centralRunConfig{}, fmt.Errorf("%w: central run takes no arguments", errAdminUsage)
	}
	if (*guiCert == "") != (*guiKey == "") {
		return centralRunConfig{}, fmt.Errorf("%w: -gui-cert and -gui-key must be given together", errAdminUsage)
	}
	gl := *guiListen
	if gl == "" {
		return centralRunConfig{}, fmt.Errorf("%w: -gui-listen must be an address or %q", errAdminUsage, guiOff)
	}
	if gl != guiOff {
		_, port, err := net.SplitHostPort(gl)
		if err != nil {
			return centralRunConfig{}, fmt.Errorf("%w: -gui-listen %q: %v", errAdminUsage, gl, err)
		}
		if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
			return centralRunConfig{}, fmt.Errorf("%w: -gui-listen %q: bad port", errAdminUsage, gl)
		}
	}
	for _, h := range hosts {
		if err := gui.CheckHostName(h); err != nil {
			return centralRunConfig{}, fmt.Errorf("%w: %v", errAdminUsage, err)
		}
	}
	if *advertise != "" {
		h, p, err := net.SplitHostPort(*advertise)
		if err != nil || h == "" {
			return centralRunConfig{}, fmt.Errorf("%w: -advertise %q must be HOST:PORT", errAdminUsage, *advertise)
		}
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return centralRunConfig{}, fmt.Errorf("%w: -advertise %q: bad port", errAdminUsage, *advertise)
		}
	}
	return centralRunConfig{Data: *data, Listen: *listen, GUI: guiOptions{Listen: gl, CertFile: *guiCert,
		KeyFile: *guiKey, Hosts: hosts, Advertise: *advertise}}, nil
}

// runningGUI is a started GUI.
type runningGUI struct {
	srv  *gui.Server
	addr string
}

// startGUI builds and starts the GUI on svc. On the first start (no admin yet) it writes the
// setup token to <data>/gui/setup-token and logs only the path. The token is never logged.
func startGUI(ctx context.Context, data string, svc *service.Service, o guiOptions, hostKeyFP string) (*runningGUI, error) {
	lg := o.Logf
	if lg == nil {
		lg = logf
	}
	srv, err := gui.New(gui.Config{Backend: svc, Version: version, Logf: lg, Listen: o.Listen,
		DataDir: data, CertFile: o.CertFile, KeyFile: o.KeyFile, GUIHosts: o.Hosts,
		SSHAdvertise: o.Advertise, HostKeyFingerprint: hostKeyFP})
	if err != nil {
		return nil, err
	}
	path, created, err := srv.PrepareSetup(ctx)
	if err != nil {
		return nil, fmt.Errorf("gui setup: %w", err)
	}
	if created {
		lg("GUI setup: no admin account yet. The one-time setup token (valid 24h) is in %s; open the GUI URL below and paste it.", path)
	}
	if err := srv.Start(); err != nil {
		return nil, err
	}
	if o.Advertise != "" {
		lg("GUI: SSH address shown in command hints: %s", o.Advertise)
	}
	return &runningGUI{srv: srv, addr: srv.Addr()}, nil
}

// stop drains the GUI; it must run before the store closes.
func (g *runningGUI) stop() {
	if g == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := g.srv.Shutdown(ctx); err != nil {
		// Drain timed out: cancel the in-flight requests' context and drop the connections so no
		// backend call is still running when the store closes.
		_ = g.srv.Close()
	}
}
