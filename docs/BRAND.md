# mirafleet — Brand

Taken from the MIRASTACK LABS site (www.mirastacklabs.ai, `assets/css/site.css`), approved
13 Nov 2026. Visual reference: https://claude.ai/artifact/5r6eUzmoH4MVYBKoMQpGt5 (private).

## Identity
- **Mark:** the parent three-bar mark, unchanged.
  `<svg viewBox="0 0 32 26"><polygon points="0,26 7,0 12,0 5,26" fill="#00C9B1"/><polygon points="9,26 16,0 21,0 14,26" fill="#FF671F"/><polygon points="18,26 25,0 30,0 23,26" fill="#CED4DA"/></svg>`
  The first bar uses `--logo-teal` (`#007A6E` light, `#00C9B1` dark).
- **Wordmark:** JetBrains Mono. `MIRAFLEET` at weight 700 with letter-spacing .08em, then
  `BY MIRASTACK LABS` at weight 400 in `--muted` where space allows.

## Tokens
Same names as the site. Dark follows `prefers-color-scheme` unless `data-theme` is set on the root.

| Token | Light | Dark |
|---|---|---|
| `--bg` | `#FFFFFF` | `#0A0A0A` |
| `--bg-band` | `#F6F5F1` | `#000000` |
| `--surface` | `#FFFFFF` | `#191919` |
| `--ink` | `#0A0A0A` | `#FFFFFF` |
| `--text` | `#1C1C1C` | `#E6E6E6` |
| `--muted` | `#5E5E5E` | `#8F8F8F` |
| `--line` | `#E4E2DC` | `#272727` |
| `--accent` | `#00695F` | `#2BD4BF` |
| `--signal` | `#FF671F` | `#FF671F` |
| `--btn-bg` / `--btn-text` | `#0A0A0A` / `#FFFFFF` | `#FFFFFF` / `#0A0A0A` |

Status colours (mirafleet addition; text / background):

| State | Light | Dark |
|---|---|---|
| Active (`--ok`) | `#00695F` / `#E0F0EE` | `#2BD4BF` / `#0F2C29` |
| Pending (`--warn`) | `#8A5300` / `#FBEFDC` | `#F0B45A` / `#2E2410` |
| Disabled (`--off`) | `#5E5E5E` / `#EFEEEA` | `#8F8F8F` / `#1F1F1F` |
| Revoked (`--bad`) | `#B42318` / `#FDE8E6` | `#FF8A7A` / `#3A1715` |

Signal orange is for emphasis only (active tab underline, 2px section rules, focus ring), never a state.

## Type
- **Display:** Oswald 600, uppercase for H1/H2, letter-spacing −.01em.
- **Body:** Oswald 300–400, 16px, line-height 1.55.
- **Mono:** JetBrains Mono for the wordmark, labels, IDs, table data, code and the CLI.
- **Labels:** JetBrains Mono 600, 11–12px, uppercase, letter-spacing .14em.
- Fallbacks: `"Arial Narrow", Arial, sans-serif` and `ui-monospace, SFMono-Regular, Menlo, Consolas, monospace`.

## Shape
2px radius on controls, 4px on panels, 1px hairline borders, no shadows.

## CLI
Teal for success and names, orange for warnings and prompts, grey for secondary text.
Colour off when `NO_COLOR` is set or stdout is not a terminal.

## Fonts and licences
Oswald and JetBrains Mono are SIL OFL-1.1. They are self-hosted (embedded in the binary for the
GUI) with their OFL licence text shipped alongside. See the font exception in CLAUDE.md.
