# Developer guide — style rules

Audience: engineers fresh out of college. They know basic programming and some Go or another
language; they may never have used SSH keys, SQLite, containers or MCP. Goal: after reading the
guide they understand the whole design and code and never need to ask the authors.

## Voice
- Plain English, short sentences, active voice. Explain every term the first time it appears
  and link it to [glossary.md](glossary.md). No unexplained acronyms.
- Teach the *why*, not only the *what*: for every rule or defence, say what would go wrong
  without it, with a concrete example.
- Use one running example everywhere: company **Acme**, central `central.example.com:2222`,
  fleets `dev` and `prod`, runners `build-1` and `db-1`, operators `alice` and `bob`.
  Never real hostnames, people or secrets.
- Every chapter starts with **"What you will learn"** (3–5 bullets) and ends with
  **"Check your understanding"** (3–5 questions, answers in a collapsed `<details>` block).
- Facts come from the code, not from memory. Name files and functions (`internal/service/exec.go`,
  `service.RunAction`) rather than line numbers, so the guide doesn't rot.
- Accurate over exhaustive: if a feature is planned (M3+), say "planned" and link the milestone.

## Diagrams
- **Mermaid** (rendered by GitHub) for flowcharts, sequence, state and ER diagrams, inline in the
  Markdown. Keep each diagram under ~25 nodes; split big ones. Label every arrow.
- **Animated SVG** for the key flows, stored in `docs/guide/diagrams/<name>.svg` and embedded as
  `![<alt text>](diagrams/<name>.svg)`. Rules:
  - Self-contained: no scripts, no external fonts/images/links; CSS animations only.
  - Light and dark: colours via CSS variables with `@media (prefers-color-scheme: dark)` inside
    the SVG, using the tokens in [../BRAND.md](../BRAND.md) (`--accent` teal for data/requests,
    `--signal` orange only for emphasis, status colours for runner states).
  - `@media (prefers-reduced-motion: reduce)` stops all animation and shows the final state.
  - Loops of 6–12 s with numbered steps visible in the drawing, matching the numbered list
    directly under the image in the Markdown (so the diagram also works without animation).
  - Fonts: `"JetBrains Mono", ui-monospace, Menlo, Consolas, monospace` for labels;
    `Oswald, "Arial Narrow", Arial, sans-serif` for titles. Readable at 800 px wide.
- Every diagram has a one-line caption saying what to look at.

## Shared file names (writers reference these; the diagram author creates them)
| File | Shows |
|---|---|
| `diagrams/big-picture.svg` | operator → central → runner; all connections outbound to central; where keys live |
| `diagrams/runner-join.svg` | join token → enroll connection → host-key pinning → pending → approve |
| `diagrams/operator-register.svg` | rotating 60 s registration code, HMAC, two operators in one window |
| `diagrams/action-run.svg` | one `action.run` through every layer and back, with both audit rows |
| `diagrams/revoke.svg` | tombstone → kick → in-flight cancel → commit → reconnect refused |
| `diagrams/framing.svg` | the 4-byte length prefix + JSON envelope on an SSH channel |
| `diagrams/workspace-open.svg` | `workspace.open`: checks, sticky reuse, placement, reserve, create on the runner |
| `diagrams/workspace-exec.svg` | one `workspace.exec` through every layer and back, with both audit rows |
| `diagrams/workspace-boundary.svg` | the container boundary: what is locked down, what crosses, what cannot |
| `diagrams/mcp-tool-call.svg` | one `exec` MCP tool call: harness → `mcp` → central → runner, with the output cap on the way back |
| `diagrams/session-handoff.svg` | the session handoff file: hook writes `cc:<id>`, server reads it, `--resume` reattaches |

## Chapter files
`README.md` (index and reading order), `glossary.md`, `01-what-is-mirafleet.md`,
`02-background.md`, `03-architecture.md`, `04-flows.md`, `05-security.md`, `06-data-model.md`,
`07-protocols.md`, `08-code-tour.md`, `09-dev-setup-and-testing.md`, `10-operations.md`,
`11-contributing.md`, `12-why-mirafleet.md`. Link between chapters with relative links.
