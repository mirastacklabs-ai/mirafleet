// Package ctrd is the containerd executor for workspaces. The implementation is
// linux-only; this file keeps the package buildable on every platform so darwin and windows
// binaries never link containerd or gRPC.
package ctrd
