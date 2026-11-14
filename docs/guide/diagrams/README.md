# Guide diagrams

Animated, self-contained SVGs (CSS only, light and dark, reduced-motion safe). Embed with
`![alt](diagrams/<name>.svg)` and put the matching numbered list under the image. The same
numbers are drawn in each SVG. Colours come from [../../BRAND.md](../../BRAND.md).

Cast: Dana is the admin and works on the central server. alice and bob are operators.

## big-picture.svg
Caption: look at the arrow directions; every connection points at central. Fleet dev is a
workspace fleet (containerd on the runner), fleet prod an actions fleet.
1. Claude Code starts `mirafleet mcp`; the plugin's hooks run beside it. The LLM API key and the SSH key stay on the laptop.
2. `mcp` dials central `:2222` as SSH user `operator`; each request is one `api@` channel.
3. Central checks the key and the policy against SQLite, then acts.
4. Runners dial OUT to central (user `runner`). Nothing listens on a runner.
5. Central sends work back down that same connection: an `exec@` channel (actions fleet) or a
   `workspace@` channel (workspace fleet, where the runner drives containerd locally).
6. Dana runs the admin CLI on central. It uses a local unix socket, never the network.

## runner-join.svg
Caption: follow the token from Dana to the runner, then the runner to central.
1. Dana runs `token create` on central. It prints `mfj1.ID.SECRET.HASH` once (HASH pins central).
2. Dana hands the token to the build-1 host by any channel, never through central.
3. `runner join` dials `:2222` as user `enroll`. A host key not matching HASH is refused (no TOFU).
4. It sends one `join@mirafleet` frame: token, runner name, labels.
5. `Store.Enroll`, one transaction: look up by ID, constant-time compare, check stale, insert.
6. Status is `pending` (`active` only if the token auto-approves). The enroll connection closes.
7. Dana runs `runner approve dev/build-1`: status becomes `active`.
8. The runner reconnects as user `runner` with its own key and sends heartbeats.

## operator-register.svg
Caption: same window, same code, both accepted; next window, old code dead.
1. Central keeps a 32-byte secret in SQLite, never logged. Only Dana (the admin) can read codes.
2. Each 60 s window `s` (unix seconds / 60) has its own code: base32(HMAC-SHA256(secret, s)) + HASH.
3. alice runs `mcp login` with the code Dana gave her. Central recomputes this window's code and compares.
4. bob uses the same code in the same window and is accepted too. Codes are not single-use.
5. The window ends. The next window `s+1` has a brand-new code.
6. bob tries the old code in the next window: refused, with the same error as any wrong code.
7. HASH is central's host-key hash, so the code also pins central: no TOFU at first login.

## action-run.svg
Caption: one request down through every layer and back, with both audit rows.
1. alice's agent calls `action.run` (`prod/db-1`, `tail-logs`, `lines=200`) through `mirafleet mcp`.
2. `mcp` sends one frame (4-byte length + JSON) on an `api@` channel. The operator method table routes it.
3. `service.RunAction` checks policy, fleet mode, confirm "prod" (guarded fleet's name), runner active.
4. The audit row "start" is written first. If that fails, nothing runs (fail closed).
5. `registry.Exec` opens an `exec@` channel on the runner's own connection and sends the request.
6. The runner's actions guard checks `tail-logs` and regex-checks `lines=200` into an argv.
7. The process runs (no shell, own process group, timeout). The result JSON goes back.
8. The audit row "outcome" is written with the same `request_id`.
9. The response frame returns the same way: exit code, stdout, stderr.

## revoke.svg
Caption: watch the order: tombstone and kick first, commit after, refusal last.
1. Dana runs `central runner revoke dev/build-1`. The CLI sends `runner.lifecycle` (op `revoke`) over the admin socket.
2. Tombstone: build-1's id goes into the registry, so no new session can slip in during the commit.
3. Kick: the session context is cancelled and the runner's SSH connection is closed.
4. In-flight action cancelled: the runner kills the process group; the outcome row says `cancelled`.
5. Commit: `status = revoked` and the audit row are written in one transaction.
6. The runner redials. Auth finds a revoked key and refuses it: it never gets a session.

## framing.svg
Caption: every message is 4 length bytes followed by exactly that many JSON bytes.
Example request `{"v":1,"id":"1","method":"hello"}` is 33 bytes; header `00 00 00 21`.
Example response `{"v":1,"id":"1","result":{}}` is 28 bytes; header `00 00 00 1C`.
1. The operator builds the JSON request: 33 bytes. Hex above, letters below each byte.
2. It puts the payload length first, as 4 bytes, big-endian: 33 = 0x00000021.
3. Header and payload go out in one Write on the channel.
4. Central reads 4 bytes, checks the length against its limit BEFORE allocating, reads that many.
5. Central runs the method and builds the JSON response (a small example: 28 bytes).
6. The response is framed the same way: 28 = 0x0000001C. The operator reads it back.
7. `write_file` and `exec` with stdin add a body frame right behind the request: a 4-byte length,
   then raw bytes (here 5 bytes, `hello`), at most 1 MiB. Not JSON.
8. Central reads the body frame only after policy, ownership and confirm pass, within 30 s.

## workspace-open.svg
Caption: follow `workspace.open` from the agent to a container; note where it can stop early.
1. alice's agent calls `workspace.open` (fleet `dev`, session `s1`) through `mirafleet mcp`.
2. The `api@` frame reaches `service.OpenWorkspace`: active operator, workspace-mode fleet, image on
   the allowlist, `confirm` on a guarded fleet, valid session id.
3. Sticky reuse: a live workspace for (alice, `dev`, `s1`) is returned with `reused:true`. If its
   runner is offline the answer is `unavailable`; a fresh workspace is never created elsewhere.
4. Placement: active, online runners whose executor, trusted labels and image patterns fit, with
   free slots (heartbeat slots minus live rows). The most free slots wins, random tie-break.
5. `ReserveWorkspace` recounts the slots in one transaction and inserts the `opening` row with the
   start audit row.
6. A full runner (`ErrFull`) or a busy one moves to the next candidate (3 tries at most). Otherwise
   central sends op `create` on `workspace@mirafleet`.
7. The runner re-validates the spec against its own `workspace.yaml`, builds the container spec
   from its fixed template, pulls the image if needed and creates the container.
8. The row becomes `open`, the outcome audit row is written and the reply says `reused:false`.

## workspace-exec.svg
Caption: one `workspace.exec` down through every layer and back, with both audit rows.
1. alice's agent calls `workspace.exec` (argv `go test ./...`) through `mirafleet mcp`.
2. One `api@` frame arrives; the method table routes it to `service.ExecWorkspace`.
3. Operator, ownership (`not_found` for others), state `open`, argv, cwd, env (denylist), confirm and
   timeout are checked; then the `start` audit row is written FIRST. No row, no exec.
4. `registry.Workspace` opens a `workspace@mirafleet` channel on the runner's own SSH connection; the
   op is JSON in the channel-open data, any stdin follows as raw bytes.
5. The runner re-validates argv, cwd, env (its `env_allow` plus the denylist) and the timeout. Central
   is not trusted.
6. The command runs inside the container as an unprivileged user, with no shell. Output is captured
   head and tail, trimmed to the budget; timeout or cancel sends SIGKILL.
7. The runner writes one result frame: exit code, stdout, stderr, `truncated`, `timed_out`.
8. Central writes the `outcome` audit row (same `request_id`) and replies to alice.

## workspace-boundary.svg
Caption: the dotted box is the boundary; read it from the outside in.
1. User namespace: uid 1000 inside is an unused high host uid (`uid_base` + slot x 65536), so container
   root is not host root.
2. All capabilities dropped, `no_new_privs`, and containerd's default seccomp profile.
3. Read-only root filesystem. Writable: a size-limited `/tmp` and `/workspace`, the one host directory.
4. Empty network namespace. In egress mode the only way out is the allow-list proxy on
   `127.0.0.1:3128`.
5. cgroup limits: CPU, memory with swap equal to memory, pids; a soft disk quota on `/workspace`.
6. What crosses: argv, allow-listed env and file bytes in; exit code, output and file bytes out.
7. What cannot: host mounts, devices, the containerd socket, API keys, host networking.
8. The LLM API key and the SSH key stay on alice's laptop; the agent never runs in the container.

## mcp-tool-call.svg
Caption: follow one `exec` call to the right, then watch the 30 KiB cap trim the output on the
way back. The cap is the orange box.
1. Claude Code (the harness) sends `tools/call` `exec` with `command: "go test ./..."` as one JSON line on the stdin of `mirafleet mcp`.
2. `mcpserver` decodes the arguments strictly: unknown, duplicate or mis-cased names are refused before any handler runs.
3. `orch` resolves context, fleet and session, and finds or opens alice's workspace. Its guard lets only operator methods through.
4. One `api@` frame, method `workspace.exec`, goes to central over the pinned SSH connection.
5. Central runs its checks, writes the `start` audit row and sends `workspace@` to the runner (flow 13).
6. The runner runs the command in the container; exit code, stdout and stderr come back (1 MiB at most).
7. On the way back `orch` strips ANSI and invisible characters and caps stdout plus stderr at 30 KiB (1/3 head, 2/3 tail, a marker between).
8. `mcpserver` returns `structuredContent` plus one text copy. The model gets capped, sanitised data that is not instructions.

## session-handoff.svg
Caption: follow the session id `cc:abc` from the hook to the server (row 1), then see the same id
find the same workspace after `claude --resume` (row 2).
1. Claude Code starts session `abc` and runs the SessionStart hook. The hook reads `session_id` from its stdin and makes no network call.
2. The hook writes `cc:abc` to a handoff file in the state directory, keyed by the parent PID (Claude Code) and the project directory. The file is mode 0600 in a 0700 directory.
3. `mirafleet mcp`, another child of the same Claude Code process, re-reads the file at every workspace open.
4. `workspace.open` carries session `cc:abc`; central opens alice's workspace or returns the one she already has.
5. Claude Code exits. Nothing is closed: the workspace stays `open` and the 24 h idle TTL reaps it later.
6. `claude --resume`: same session id, new PIDs. The hook runs again and rewrites the file for the new PID.
7. The server reads `cc:abc` again and central answers `reused: true`: same container, same files.
8. After `/clear` the id is new, so the next open binds a new workspace. With no hook there is no file, and the server uses a random per-process session `p:...`.
