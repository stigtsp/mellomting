# Threat model

This document is a working summary. `docs/PLAN.md` §5 is the source of
truth; when the two differ, the plan wins.

## Assumptions

1. **Clients are untrusted and may be adversarial.** Coding agents and
   third-party libraries call the proxy with arbitrary input. Assume they
   will attempt, in PLAN §5.1 terms:

   - bearer-token brute force and API-key enumeration;
   - malformed HTTP, malformed JSON, oversized bodies and huge headers;
   - slowloris and connection/request floods;
   - long-running stream and backend queue exhaustion;
   - excessive output requests, retry amplification, malformed SSE;
   - log injection and cross-key response-ID access;
   - reaching backend administrative endpoints and smuggling backend
     authentication headers;
   - bypassing model ACLs (including via raw backend model names);
   - exploiting the optional qualifier.

2. **Mellomting itself will contain bugs.** Landlock and host hardening
   exist specifically to reduce the impact of a post-compromise exploit
   (PLAN §5.2), not to prevent one.

3. **Backends are separate processes.** vLLM servers run outside Mellomting
   and are not sandboxed by it. Local backends should expose only the
   inference functionality they need, run as different unprivileged users,
   and keep admin/dev endpoints disabled (PLAN §67).

## What the sandbox does not solve

If Mellomting is compromised, an attacker may still misuse what Mellomting
is intentionally allowed to touch (PLAN §5.2): already-open files, the
accounting file descriptor, configured backend ports, backend credentials
already in memory, the qualifier backend, and anything the configured
destination policy allows. Remote provider credentials increase
post-compromise impact and are the reason they belong in secret files
referenced by configuration, never in the YAML itself (PLAN §17).

## Out of scope

Not protectable by Mellomting alone (PLAN §5.3): kernel/host compromise,
malicious administrators, malicious model-server code, physical attacks,
volumetric upstream DDoS, and compromise of a front-end TLS terminator.
