# Hand-reviewed licence texts

`relgen licenses` appends every file listed here to `THIRD_PARTY_LICENSES.txt`, after the
`go-licenses save` output. These are the licences `go-licenses` cannot see or classify.
Update this table in the same change as any `--ignore` in `scripts/dev.sh` or any asset in
`internal/gui/static/ASSETS.md`; `go test ./tools/relgen` fails if they drift.

The `Component` column is the Go module path for `--ignore`d modules, otherwise the name.

| File | Component | Licence | Why it is here |
|---|---|---|---|
| `modernc.org-mathutil.txt` | modernc.org/mathutil | BSD-3-Clause | `go-licenses` cannot classify the "The mathutil Authors" wording; reviewed by hand (pinned v1.7.1 in dev.sh) |
| `github.com-segmentio-asm.txt` | github.com/segmentio/asm | MIT-0 | `go-licenses` v1.6.0 has no MIT-0 classifier (pinned v1.2.1 in dev.sh) |
| `musl-libc-notice.txt` | musl libc | MIT | musl-derived code bundled in `modernc.org/libc`, invisible to `go-licenses` |
| `htmx-0BSD.txt` | htmx | 0BSD | vendored GUI asset (`internal/gui/static/vendor/htmx.min.js`) |
| `Oswald-OFL.txt` | Oswald | OFL-1.1 | embedded GUI font, shipped with its licence text |
| `JetBrainsMono-OFL.txt` | JetBrains Mono | OFL-1.1 | embedded GUI font, shipped with its licence text |

`htmx-0BSD.txt`, `Oswald-OFL.txt` and `JetBrainsMono-OFL.txt` are byte-identical copies of
`internal/gui/static/LICENSES/*` (a test enforces it).
