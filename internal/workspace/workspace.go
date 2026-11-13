// Package workspace defines the runner-side Executor interface behind workspace mode, the
// types it exchanges, and the validation helpers shared by central and the runner. Pure Go,
// stdlib only. The runner re-validates everything; central is not trusted.
package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"time"
)

// Root is the working directory every workspace path is relative to.
const Root = "/workspace"

// ID is a central-assigned workspace id: 32 lowercase hex characters.
type ID string

// PullPolicy says when the executor may pull the image.
type PullPolicy string

const (
	PullIfMissing PullPolicy = "if-missing" // default
	PullNever     PullPolicy = "never"
	PullAlways    PullPolicy = "always"
)

// Valid reports whether p is a known policy (the empty string means the default).
func (p PullPolicy) Valid() bool {
	switch p {
	case "", PullIfMissing, PullNever, PullAlways:
		return true
	}
	return false
}

// NetworkMode is the workspace network mode.
type NetworkMode string

const (
	NetNone   NetworkMode = "none"   // default: no network
	NetEgress NetworkMode = "egress" // allowlisted egress through the proxy only
)

// State is a workspace's runtime state.
type State string

const (
	StateCreated State = "created"
	StateRunning State = "running"
	StateStopped State = "stopped"
	StateUnknown State = "unknown"
)

// Limits are per-workspace resource limits. Zero means the executor's configured default.
type Limits struct {
	CPUMilli    int64 // 1000 = one CPU
	MemoryBytes int64
	Pids        int64
	TmpfsBytes  int64
	DiskBytes   int64 // soft quota for /workspace
}

// EgressRule allows one destination host (name or IP literal) and TCP port.
type EgressRule struct {
	Host string
	Port int
}

// Network is the workspace network policy.
type Network struct {
	Mode  NetworkMode // "" means none
	Allow []EgressRule
}

// Spec describes a workspace to create.
type Spec struct {
	ID                   ID
	Image                string
	Pull                 PullPolicy
	Limits               Limits
	Network              Network
	RequireSecureRuntime bool
	Labels               map[string]string
	MaxAge               time.Duration // 0 = no executor-side age limit
}

// ExecRequest is one command run inside a workspace. The timeout comes from the context.
type ExecRequest struct {
	Argv   []string
	Cwd    string // "" = /workspace
	Env    map[string]string
	Stdin  io.Reader // may be nil
	Stdout io.Writer // may be nil (discarded)
	Stderr io.Writer // may be nil (discarded)
}

// ExecResult is the outcome of an Exec that ran to completion or was stopped by the context.
type ExecResult struct {
	ExitCode int
	TimedOut bool
	Duration time.Duration
}

// FileInfo describes a file in a workspace.
type FileInfo struct {
	Size    int64
	Mode    fs.FileMode
	ModTime time.Time
}

// Info describes a live workspace, for the reaper and reconcile.
type Info struct {
	ID        ID
	Image     string
	State     State
	Created   time.Time
	DiskBytes int64
	Labels    map[string]string
}

// Errors returned by executors. Wrap them with %w; test with errors.Is.
var (
	ErrNotFound    = errors.New("workspace: not found")
	ErrRefused     = errors.New("workspace: refused")
	ErrBusy        = errors.New("workspace: busy")
	ErrTooLarge    = errors.New("workspace: too large")
	ErrQuota       = errors.New("workspace: quota exceeded")
	ErrUnavailable = errors.New("workspace: unavailable")
	ErrUnsupported = errors.New("workspace: unsupported")
)

// Executor runs workspaces. Scheduling, TTLs, slots, audit and policy live above it.
type Executor interface {
	// Name is the executor kind, e.g. "containerd" or "opensandbox".
	Name() string
	// Create makes a workspace; idempotent on s.ID. Errors: ErrRefused, ErrUnavailable.
	Create(ctx context.Context, s Spec) error
	// Exec runs a command. The context deadline is the timeout; cancellation kills the command.
	Exec(ctx context.Context, id ID, r ExecRequest) (ExecResult, error)
	// ReadFile reads up to n bytes from off; n <= 0 reads to the end.
	ReadFile(ctx context.Context, id ID, path string, off, n int64) (io.ReadCloser, FileInfo, error)
	// WriteFile writes r at off (truncating first when truncate is set) and returns the new info.
	WriteFile(ctx context.Context, id ID, path string, r io.Reader, off int64, truncate bool, mode fs.FileMode) (FileInfo, error)
	// Destroy removes the workspace; idempotent (absent is nil).
	Destroy(ctx context.Context, id ID) error
	// List returns all workspaces this executor holds.
	List(ctx context.Context) ([]Info, error)
	Close() error
}

// NewID returns a fresh random workspace id.
func NewID() ID {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("workspace: crypto/rand failed: " + err.Error())
	}
	return ID(hex.EncodeToString(b[:]))
}

// ValidID reports whether id is 32 lowercase hex characters.
func ValidID(id ID) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
