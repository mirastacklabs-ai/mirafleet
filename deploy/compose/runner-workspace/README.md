# Container-workspace runner with Docker Compose

A workspace-mode runner with the container executor, from the published image. It lets a Docker
host serve container workspaces (private containers for agent sessions) without our packages.
Host mode (`executor: host`) is not available here; use the packages for it.

**Security: this runner is root-equivalent on the host.** It runs as root in the host's pid
namespace with the host containerd's socket and `SYS_ADMIN`. That is the same trust as the
packaged `mirafleet-runner-workspace` service. Compose adds convenience, not isolation. Use a
dedicated machine.

**Workspaces here run without the mirafleet AppArmor profile.** The image has no
`apparmor_parser`, so the runner cannot load the profile (`apparmor: off` in `workspace.yaml`;
`auto` would refuse to start on an AppArmor host). Every other layer is applied as with the
packages: a user namespace per slot, all capabilities dropped, `no_new_privs`, containerd's
default seccomp profile, a read-only root filesystem, an empty network namespace (egress only
through the allow-list proxy), and cgroup limits for CPU, memory and pids. If you want AppArmor
too, use the packages.

## Prerequisites

- A Linux host with Docker Engine **and its containerd 2.x** at `/run/containerd/containerd.sock`
  (`docker-ce` uses it; check with `containerd --version`). Docker Desktop and rootless Docker
  are not supported: the runner needs the real host containerd and the host pid namespace.
- Kernel 5.19 or newer with user namespaces on (`userns: required` in `workspace.yaml`).
- containerd's runc uses the `cgroupfs` cgroup driver (the default; `SystemdCgroup = false`).
- The CA bundle at `/etc/ssl/certs/ca-certificates.crt` (Debian, Ubuntu). On RHEL or Fedora,
  `export MIRAFLEET_CA_BUNDLE=/etc/pki/tls/certs/ca-bundle.crt`. The image has no CA bundle and
  the runner pulls images itself.
- `apparmor=unconfined` in the compose file is for hosts with AppArmor. Docker's default profile
  blocks what the egress proxy needs. If your Docker refuses it on a host without AppArmor,
  delete that line.

## Set up

1. Create the workspace root once, on the host. It must be root-owned with mode 0700, and
   Compose must not create it for you (it would get the wrong mode, and the runner refuses that):

   ```sh
   sudo install -d -m 0700 /var/lib/mirafleet/ws
   ```

2. Edit `workspace.yaml`: the `images` allowlist (the boundary; prefer digests), `limits`, and
   `network`. It is mounted read-only. Unknown fields are errors.
3. On central, create a workspace fleet and a join token. The fleet's image must match `images`:

   ```sh
   mirafleet central fleet create ws -mode workspace -image docker.io/library/busybox@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec
   mirafleet central token create -fleet ws
   ```

   (With Compose on central: `docker compose exec central mirafleet central ...`.)
4. Join once. The token goes in on standard input, never on the command line:

   ```sh
   echo 'PASTE-THE-TOKEN' | docker compose run --rm -T runner runner join CENTRAL_HOST:2222 -name ws-1 -data /var/lib/mirafleet/runner -token -
   ```

5. An admin approves it on central: `mirafleet central runner approve ws/ws-1`.
6. Start it: `docker compose up -d`

The runner key and pin live in the `runner-data` volume. To join again, use a new volume.
Workspace files live on the host in `/var/lib/mirafleet/ws`; image layers in containerd's own
storage, in the namespace `mirafleet` (never Docker's `moby`).

## Why each setting

| Setting | Why |
|---|---|
| `user: "0:0"` | The containerd socket and the workspace root (root, 0700) need root. |
| `pid: host` | The runner reads `/proc/<pid>/ns/net` of workspace tasks and `/proc/<pid>/task/*/children` of an exec. Those pids are host pids. |
| `cap_add: [SYS_ADMIN]` | `setns` into a workspace's network namespace, where the egress proxy listens. Not `privileged`. |
| `apparmor=unconfined` | Docker's default AppArmor profile denies those `/proc/<pid>/ns/net` opens. This is about the runner's own container, not the workspaces. |
| `/run/containerd` mounted at the same path (`rslave`) | The socket, and the stdio fifos of an exec (made by the runner, opened by the containerd shim on the host). The directory, not just the socket file, because a bind-mounted socket goes stale when containerd restarts. |
| `/var/lib/mirafleet/ws` mounted at the same path | containerd bind-mounts directories from it into workspaces, resolving the path on the host. The runner also keeps a copy of its own binary there (`.init/`), the PID 1 of every workspace. |
| CA bundle mounted read-only | Image pulls over TLS; the image ships no CA bundle. |
| `runner-data` volume at `/var/lib/mirafleet/runner` | The runner's key and pin. |

The same trust applies to anyone who can edit this compose file or reach the Docker socket:
treat both as root on the host.

## Limits

- Private registries are not supported (pulls are anonymous). Pre-pull and use a fleet with
  `-pull never`, or use public images.
- Several runners on one host need their own namespace and workspace root.
- Tested on Docker Engine with containerd 2.x, cgroup v2, `cgroupfs` driver. Not tested: rootless
  Docker, gVisor or Kata (`secure_runtime`), `userns: off` (that mode may also need `SYS_PTRACE`).
