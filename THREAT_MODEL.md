# Threat model

This summarizes the threats and limits defined in [the design](docs/PLAN.md).

## Assumptions

1. Clients are untrusted. Threats include:

   - bearer-token brute force and API-key enumeration;
   - malformed HTTP, malformed JSON, oversized bodies and huge headers;
   - slowloris and connection/request floods;
   - long-running stream and backend queue exhaustion;
   - excessive output requests, retry amplification, malformed SSE;
   - log injection and cross-key response-ID access;
   - reaching backend administrative endpoints and smuggling backend
     authentication headers;
   - bypassing model access rules, including through backend model names.

2. The proxy may be compromised. Sandboxing and host hardening limit the
   resources a compromised process can access.

3. Backends are separate processes. vLLM servers run outside Mellomting
   and are not sandboxed by it. Local backends should expose only the
   inference functionality they need, run as different unprivileged users,
   and keep admin/dev endpoints disabled (PLAN §67).

4. Attached agents (PLAN §104) are backends that dial in, run by someone
   other than the proxy's operator. An agent key authorizes serving models
   under its namespace and nothing else. The agent sees every prompt
   routed to it and chooses the responses. Its usage claims and response
   headers are bounded, its model names cannot shadow configured models or
   another key's, and attaching never makes the proxy dial out. Per
   deployment, short-lived agent keys are advised (PLAN §104.7).

## What the sandbox does not solve

If Mellomting is compromised, an attacker may still misuse what Mellomting
is intentionally allowed to touch (PLAN §5.2): already-open files, the
accounting file descriptor, configured backend ports, backend credentials
already in memory, anything the configured destination policy allows, and,
when a backend is named by hostname, the resolver: on Linux, read access to
the directories holding the resolver configuration, usually all of `/etc`.
Keep backend credentials in protected files referenced by configuration,
rather than in the YAML itself.

## Out of scope

Not protectable by Mellomting alone (PLAN §5.3): kernel/host compromise,
malicious administrators, malicious model-server code, physical attacks,
volumetric upstream DDoS, and compromise of a front-end TLS terminator.
