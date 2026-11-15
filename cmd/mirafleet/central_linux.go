package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mirastacklabs-ai/mirafleet/internal/api"
	"github.com/mirastacklabs-ai/mirafleet/internal/auth"
	"github.com/mirastacklabs-ai/mirafleet/internal/central"
	"github.com/mirastacklabs-ai/mirafleet/internal/central/registry"
	"github.com/mirastacklabs-ai/mirafleet/internal/keys"
	"github.com/mirastacklabs-ai/mirafleet/internal/policy"
	"github.com/mirastacklabs-ai/mirafleet/internal/service"
	"github.com/mirastacklabs-ai/mirafleet/internal/store/sqlite"
)

// prepareDataDir sets umask 077 (the database, -wal and -shm are then created 0600), creates the
// data dir 0700 (tightened if it exists) and takes the exclusive data-dir lock. The umask comes
// first, before anything touches the directory.
func prepareDataDir(data string) (release func(), err error) {
	syscall.Umask(0o077)
	if err := keys.EnsureDir(data); err != nil {
		return nil, err
	}
	return lockDataDir(data)
}

// centralDeps is everything built from a prepared data dir.
type centralDeps struct {
	db     *sqlite.DB
	signer ssh.Signer
	reg    *registry.Registry
	ops    *central.OperatorConns
	svc    *service.Service
}

// loadCentral loads the host key, opens (and migrates) the database and builds the service.
func loadCentral(ctx context.Context, data string) (*centralDeps, error) {
	signer, err := keys.LoadSigner(filepath.Join(data, "host_ed25519"))
	if err != nil {
		return nil, fmt.Errorf("load host key (run `mirafleet central init` first): %w", err)
	}
	db, err := sqlite.Open(ctx, filepath.Join(data, "mirafleet.db"))
	if err != nil {
		return nil, err
	}
	reg, err := registry.New(registry.Options{StaleAfter: 3 * heartbeatEvery, Logf: logf})
	if err != nil {
		db.Close()
		return nil, err
	}
	ops := central.NewOperatorConns()
	cc, err := credentialsConfig() // MIRAFLEET_SECRETS_KEY; unset = credentials off
	if err != nil {
		db.Close()
		return nil, err
	}
	cc.Sender = reg // creds_sync over the runner's live connection
	if cc.RotateClient, err = rotateClient(); err != nil {
		db.Close()
		return nil, err
	}
	svc, err := service.New(service.Config{Store: db, Policy: policy.TablePolicy{}, Live: reg,
		HostKey: signer.PublicKey(), Now: time.Now, OperatorConns: ops, Logf: logf, Credentials: cc})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &centralDeps{db: db, signer: signer, reg: reg, ops: ops, svc: svc}, nil
}

func centralInit(args []string) error {
	f := flag.NewFlagSet("central init", flag.ContinueOnError)
	data := f.String("data", defaultCentralData, "data directory")
	listen := f.String("listen", "0.0.0.0:2222", "SSH listen address (shown in the next-steps hint)")
	if err := f.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errAdminUsage, err)
	}
	if f.NArg() > 0 {
		return fmt.Errorf("%w: central init takes no arguments", errAdminUsage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return doCentralInit(ctx, *data, *listen, os.Stdout)
}

// doCentralInit is offline and idempotent: it never overwrites an existing host key or
// registration secret.
func doCentralInit(ctx context.Context, data, listen string, out io.Writer) error {
	release, err := prepareDataDir(data)
	if err != nil {
		return err
	}
	defer release()
	hostKey := filepath.Join(data, "host_ed25519")
	if _, err := os.Stat(hostKey); errors.Is(err, fs.ErrNotExist) {
		if _, err := keys.Generate(data, "host_ed25519", ""); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	d, err := loadCentral(ctx, data) // creates and migrates the database
	if err != nil {
		return err
	}
	defer d.db.Close()
	state := "created the registration secret"
	err = d.svc.InitRegistrationSecret(ctx, auth.LocalAdmin(os.Getuid()), service.SourceCLI)
	var ae *api.Error
	switch {
	case err == nil:
	case errors.As(err, &ae) && ae.Code == api.CodeConflict:
		state = "already initialised; registration secret kept"
	default:
		return err
	}
	fmt.Fprintf(out, "data dir:  %s\nhost key:  %s\nfingerprint: %s\nlisten:    %s\n%s\n"+
		"next: mirafleet central run -data %s -listen %s\n",
		data, hostKey, ssh.FingerprintSHA256(d.signer.PublicKey()), listen, state, data, listen)
	return nil
}

// runningCentral is a started central: SSH server, admin socket and the data-dir lock.
type runningCentral struct {
	Addr    string // actual SSH listen address
	GUIAddr string // actual GUI listen address ("" when the GUI is off)
	Close   func()
}

// startCentral takes the data-dir lock, opens the database, starts the SSH server and then the
// admin socket (which is safe to clean up stale sockets only under that lock). The lock is held
// until Close. The GUI starts only when gui[0] enables it (no argument: no GUI); Close stops it
// before the store closes.
func startCentral(ctx context.Context, data, listen string, gui ...guiOptions) (*runningCentral, error) {
	release, err := prepareDataDir(data)
	if err != nil {
		return nil, err
	}
	d, err := loadCentral(ctx, data)
	if err != nil {
		release()
		return nil, err
	}
	cctx, cancel := context.WithCancel(ctx)
	closeAll := func() { cancel(); d.db.Close(); release() }
	srv, err := central.New(central.Config{Listen: listen, HostKey: d.signer, Store: d.db, Service: d.svc,
		Registry: d.reg, Operators: d.ops, Logf: logf})
	if err != nil {
		closeAll()
		return nil, err
	}
	addr, err := srv.Start()
	if err != nil {
		closeAll()
		return nil, err
	}
	if err := srv.ServeAdmin(cctx, adminSock(data)); err != nil {
		srv.Close()
		closeAll()
		return nil, err
	}
	var g *runningGUI
	if len(gui) > 0 && gui[0].enabled() {
		if g, err = startGUI(ctx, data, d.svc, gui[0], ssh.FingerprintSHA256(d.signer.PublicKey())); err != nil {
			srv.Close()
			closeAll()
			return nil, err
		}
	}
	rc := &runningCentral{Addr: addr, Close: func() { cancel(); g.stop(); srv.Close(); d.db.Close(); release() }}
	if g != nil {
		rc.GUIAddr = g.addr
	}
	return rc, nil
}

func centralRun(args []string) error {
	cfg, err := parseCentralRun(args, os.Stderr)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rc, err := startCentral(ctx, cfg.Data, cfg.Listen, cfg.GUI)
	if err != nil {
		return err
	}
	defer rc.Close()
	logf("central listening on %s, admin socket %s", rc.Addr, adminSock(cfg.Data))
	if rc.GUIAddr == "" {
		logf("GUI is off (-gui-listen off)")
	}
	<-ctx.Done()
	return nil
}
