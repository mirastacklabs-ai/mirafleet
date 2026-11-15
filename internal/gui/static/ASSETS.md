# Vendored assets

Everything below is embedded in the binary and served from `/static/<hash>/<name>`.
`TestVendoredAssets` and `NewAssets` check each SHA-256 against this table and require the
licence file to exist. Update this file in the same change as any asset.

| File | Component | Version | Source URL | Licence | Licence file | Form | SHA-256 |
|---|---|---|---|---|---|---|---|
| `vendor/htmx.min.js` | htmx | 2.0.11 | https://registry.npmjs.org/htmx.org/-/htmx.org-2.0.11.tgz (`package/dist/htmx.min.js`) | 0BSD | `LICENSES/htmx-0BSD.txt` | unmodified | d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717 |
| `fonts/Oswald-latin-var.woff2` | Oswald (variable, wght 200-700) | Google Fonts v57 | https://fonts.gstatic.com/s/oswald/v57/TK3iWkUHHAIjg752GT8G.woff2 | SIL OFL-1.1 | `LICENSES/Oswald-OFL.txt` | unmodified; Google's latin subset | bd73278ee0c50041b91b4c03d1229e35b501637f46b6409e7da2d3a758446ea5 |
| `fonts/JetBrainsMono-400.woff2` | JetBrains Mono Regular | 2.304 | https://github.com/JetBrains/JetBrainsMono/releases/download/v2.304/JetBrainsMono-2.304.zip (`fonts/webfonts/JetBrainsMono-Regular.woff2`) | SIL OFL-1.1 | `LICENSES/JetBrainsMono-OFL.txt` | unmodified; renamed, full glyph set | a9cb1cd82332b23a47e3a1239d25d13c86d16c4220695e34b243effa999f45f2 |
| `fonts/JetBrainsMono-600.woff2` | JetBrains Mono SemiBold | 2.304 | same archive (`fonts/webfonts/JetBrainsMono-SemiBold.woff2`) | SIL OFL-1.1 | `LICENSES/JetBrainsMono-OFL.txt` | unmodified; renamed, full glyph set | 918edad542a1da608fd2ba8daebaff9ac802309103fe760eed465b8b4e47faf1 |
| `fonts/JetBrainsMono-700.woff2` | JetBrains Mono Bold | 2.304 | same archive (`fonts/webfonts/JetBrainsMono-Bold.woff2`) | SIL OFL-1.1 | `LICENSES/JetBrainsMono-OFL.txt` | unmodified; renamed, full glyph set | c503cc5ec5f8b2c7666b7ecda1adf44bd45f2e6579b2eba0fc292150416588a2 |

## Provenance

- htmx: the npm tarball's SHA-1 `a85363c4c5af94cd634df04c4b83d06dd0e91aa7` and SRI
  `sha512-Thx/WtpeOQqSrqBCw/A1cwGJGg4UrVa3+sW0GmrM3p4gJgO89ecH4qtbnyzDDWFvBTqjnIMCgELTNt636dtamA==`
  match the registry metadata (re-checked 2026-11-15). Licence text is the tarball's `package/LICENSE`.
- Oswald: licence text from https://github.com/google/fonts/blob/main/ofl/oswald/OFL.txt (the
  file's last change in google/fonts is commit `b6f0fe1740573b70ee367fbaba04b7586be85af3`, checked
  2026-11-15). Google serves only woff2 subsets, so the latin subset is used; no upstream checksum is published.
- JetBrains Mono: archive SHA-256 `6f6376c6ed2960ea8a963cd7387ec9d76e3f629125bc33d1fdcd7eb7012f7bbf`
  (GitHub publishes none); licence text is the archive's `OFL.txt`.
- Fonts are SIL OFL-1.1 (the font exception in CLAUDE.md): shipped with their licence text, never sold alone.
