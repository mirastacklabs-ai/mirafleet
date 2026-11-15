# Security policy

mirafleet runs commands on servers, so we take security reports seriously.

## Supported versions

mirafleet is in 0.x. Only the two newest minor releases are supported, which matches the
protocol version-skew rule: the protocol level is the minor version (v0.6.x is level 6), and
central at level N accepts runners and operators at N and N-1. A release older than N-1 cannot
talk to a current central anyway.

| Version | Security fixes |
|---|---|
| Latest minor (N) | Yes |
| Previous minor (N-1) | Yes |
| Older | No: upgrade |

Upgrade order: central first, then runners, then operators. Fixes land on `main` and ship in a
new patch release of the latest minor; the previous minor gets a patch when the fix is
practical to backport.

## Report a vulnerability privately

Do not open a public issue, discussion or pull request for a security problem.

Use GitHub private vulnerability reporting: open the repository's **Security** tab, choose
**Report a vulnerability**, and fill in the form
(<https://github.com/mirastacklabs-ai/mirafleet/security/advisories/new>). Only the maintainers
can see it.

Please include:

- the version (`mirafleet version`) and how you run it (central, runner, or `mcp`; OS);
- what you did, what you expected, and what happened;
- the impact you see, and a minimal proof of concept if you have one;
- whether you want to be credited, and how.

Never include real secrets, tokens or private keys in a report.

## What is in scope

- Bypassing enrollment, approval, tokens, operator registration or session checks.
- Escaping the runner-side actions guard: running anything other than the configured argv,
  shell injection, or parameter validation bypass.
- Reading or writing another fleet's data, runners or workspaces.
- Flaws in the transport (SSH), the admin GUI (sessions, CSRF, TLS handling), the audit log,
  or the install, update and release chain (checksums, signatures, packages).
- Leaks of secrets through logs, argv, files or error messages.

Out of scope:

- Anything that needs an attacker who already has root on the central or runner host, or who
  holds a valid operator or admin credential acting within its permissions.
- Findings that depend on a deployment you configured unsafely (for example exposing the GUI
  on the internet without TLS in front of it) when the documentation already warns against it.
- Denial of service by sending an unreasonable volume of authenticated requests.
- Missing hardening headers or best-practice notes without a demonstrable impact.
- Vulnerabilities in third-party dependencies with no effect on mirafleet; report those
  upstream.

## What to expect

These are targets, not guarantees. mirafleet is maintained by a small team.

| Step | Target |
|---|---|
| Acknowledge your report | 3 working days |
| First assessment (accepted, need more info, or out of scope) | 7 days |
| Fix or mitigation plan for a confirmed issue | 30 days |

We will keep you informed, agree a disclosure date with you (90 days at most unless we both
agree otherwise), publish a GitHub security advisory with a CVE where one applies, and credit
you if you want that. We ask that you do not disclose details before the fix is released.

## No telemetry

mirafleet has no telemetry: no usage data, crash reports or update checks are sent anywhere.
Its only network connections are the ones you configure. If you find code that makes an
unconfigured outbound connection, treat it as a security bug and report it as above.

## Safe harbor

If you act in good faith, test only against systems you own or are allowed to test, and avoid
harming other people's data or services, we will not take action against you for the research.
