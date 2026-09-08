# Security policy

Mellomting's security boundary is documented in:

- `docs/PLAN.md` §4 — security invariants (the contract)
- `THREAT_MODEL.md` — the threat model
- `HARDENING.md` — the concrete controls and deployment hardening

## Reporting a vulnerability

Report security issues privately:

1. Open the **Security** tab of this repository and choose
   **Report a vulnerability** (private disclosure).
2. Include: affected version (run `mellomting version`), configuration used,
   and a minimal reproduction if you have one. Remove API keys, credentials,
   and private prompts before sharing configuration or logs.

This is a small project maintained on a best-effort basis. There is no
guaranteed response time, but reports are read and answered. Please do not
disclose a vulnerability before it has been addressed or a decision is made.

## Out of scope

The following are outside Mellomting's security scope (PLAN §5.3) and are
not accepted as vulnerabilities:

- Kernel or host root compromise, malicious host administrators, physical
  attacks.
- Malicious model-server code (vLLM etc. runs as a separate process and is
  not sandboxed by Mellomting).
- Volumetric upstream DDoS and compromise of a TLS terminator in front of
  Mellomting.
- Landlock's stated limitations (PLAN §5.2): Landlock is defence in depth,
  not a post-compromise isolation guarantee.

## What Mellomting does not expose

Mellomting has:

- No web UI and no management HTTP API — configuration and API keys are
  managed by offline CLI commands over local files.
- No database and no plugin system.
- No telemetry. Outbound requests serve configured inference backends or
  explicit model discovery during setup.

## Verifying your deployment

- `mellomting config check --config <path>` — validates the configuration.
- `mellomting sandbox check --config <path>` — reports sandbox support
  and the configured startup behavior.
- `mellomting config show-effective --config <path>` — shows the effective
  configuration with defaults applied.
