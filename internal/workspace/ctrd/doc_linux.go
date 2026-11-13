//go:build linux

package ctrd

// Blank import keeps github.com/containerd/containerd/v2 in go.mod across `go mod tidy` until the
// executor code uses it directly.
import _ "github.com/containerd/containerd/v2/client"
