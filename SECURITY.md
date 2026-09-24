# Security policy

## Reporting a vulnerability

Please report security issues **privately**, not in a public issue:

1. Go to the repository's
   [Security → Advisories](https://github.com/stump-wtf/harness/security/advisories/new)
   page and choose **Report a vulnerability**.
2. Describe the issue, the version (`harness --version`), and how to reproduce
   it. Leave out any real credentials; a redacted config is enough.

You'll get an acknowledgement within a few days. Please give us a reasonable
window to ship a fix before you disclose publicly.

Things worth reporting include a way for a harness, a project `harness.toml`, a
drop-in, or an MCP/webhook trigger to gain more than its configuration grants; a
credential written somewhere other than the `env_file` or state it belongs in;
and the control socket, SSH cockpit or metrics listener being reachable or
usable by someone who shouldn't have them.

## Supported versions

Harness is pre-1.0. Only the **latest release** receives security fixes, and a
fix ships as a new release rather than a backport. Builds from `main` get it as
soon as it merges.
