//go:build linux

package ctrd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	containerd "github.com/containerd/containerd/v2/client"
	"golang.org/x/sys/unix"
)

// kernelRelease returns the running kernel's major and minor version.
func kernelRelease() (major, minor int, err error) {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return 0, 0, err
	}
	rel := unix.ByteSliceToString(u.Release[:])
	parts := strings.SplitN(rel, ".", 3)
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("unparseable kernel release %q", rel)
	}
	major, err1 := strconv.Atoi(parts[0])
	minPart := parts[1]
	if i := strings.IndexFunc(minPart, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		minPart = minPart[:i]
	}
	minor, err2 := strconv.Atoi(minPart)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("unparseable kernel release %q", rel)
	}
	return major, minor, nil
}

// probeUserNS reports why this host cannot give every workspace its own user namespace with an
// idmapped root filesystem, or nil when it can. Requirements: user namespaces enabled, kernel
// >= 5.19 (idmapped overlayfs mounts) and a snapshotter that advertises remap-ids (containerd
// 2.x overlayfs does). The hard proof is Create itself; this fails early with a clear message.
func probeUserNS(ctx context.Context, c *containerd.Client, snapshotter string) error {
	if b, err := os.ReadFile("/proc/sys/user/max_user_namespaces"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n <= 0 {
			return errors.New("user namespaces are disabled (user.max_user_namespaces = 0)")
		}
	}
	maj, min, err := kernelRelease()
	if err != nil {
		return err
	}
	if maj < 5 || (maj == 5 && min < 19) {
		return fmt.Errorf("kernel %d.%d lacks idmapped overlayfs mounts (need >= 5.19)", maj, min)
	}
	caps, err := c.GetSnapshotterCapabilities(ctx, snapshotter)
	if err != nil {
		return fmt.Errorf("snapshotter %q capabilities: %w", snapshotter, err)
	}
	if !slices.Contains(caps, "remap-ids") {
		return fmt.Errorf("snapshotter %q does not support remap-ids (idmapped snapshots)", snapshotter)
	}
	return nil
}
