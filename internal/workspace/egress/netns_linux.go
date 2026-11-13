//go:build linux

package egress

import (
	"fmt"
	"net"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// ListenInNetns opens a TCP listener on addr (e.g. "127.0.0.1:3128") inside the network
// namespace at nsPath (e.g. /proc/<pid>/ns/net), then returns to the caller's namespace.
//
// The work runs on a dedicated goroutine pinned to its OS thread. If the thread cannot be moved
// back to its original namespace it is never unlocked: the goroutine exits while locked, which
// makes the runtime terminate that thread instead of reusing it. The listener's socket keeps
// living in the target namespace; accepted connections belong to it as well.
func ListenInNetns(nsPath, addr string) (net.Listener, error) {
	type result struct {
		l   net.Listener
		err error
	}
	ch := make(chan result, 1)
	go func() {
		l, err := listenInNetns(nsPath, addr)
		ch <- result{l, err}
	}()
	r := <-ch
	return r.l, r.err
}

func listenInNetns(nsPath, addr string) (net.Listener, error) {
	runtime.LockOSThread()
	// Per-thread namespace of the locked thread (not /proc/self, which is the main thread's).
	cur, err := os.Open(fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid()))
	if err != nil {
		runtime.UnlockOSThread() // not switched yet: the thread is clean
		return nil, fmt.Errorf("egress: open current netns: %w", err)
	}
	defer cur.Close()
	target, err := os.Open(nsPath)
	if err != nil {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("egress: open netns %s: %w", nsPath, err)
	}
	defer target.Close()

	if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
		// A failed setns does not move the thread.
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("egress: setns into %s: %w", nsPath, err)
	}
	l, lerr := net.Listen("tcp", addr)
	if err := unix.Setns(int(cur.Fd()), unix.CLONE_NEWNET); err != nil {
		// Thread stays locked (and in the wrong namespace) so it dies with this goroutine.
		if l != nil {
			_ = l.Close()
		}
		return nil, fmt.Errorf("egress: return to original netns failed (thread discarded): %w", err)
	}
	runtime.UnlockOSThread()
	if lerr != nil {
		return nil, fmt.Errorf("egress: listen %s in %s: %w", addr, nsPath, lerr)
	}
	return l, nil
}
