# @mirastacklabs/mirafleet

Launcher for [mirafleet](https://github.com/mirastacklabs-ai/mirafleet). It contains no code of
its own besides a small script: the binary comes from one of six platform packages
(`@mirastacklabs/mirafleet-<os>-<cpu>`) that npm installs as optional dependencies. There are no
install scripts and nothing is downloaded.

Use it as an MCP server:

```
npx -y @mirastacklabs/mirafleet@<version> mcp -project-dir .
```

On Windows, where the harness spawns without a shell, use `cmd /c npx ...`.

Install with optional dependencies enabled (the npm default). The launcher names the missing
platform package if you omitted them.

Apache-2.0. Third-party licences are in each platform package.
