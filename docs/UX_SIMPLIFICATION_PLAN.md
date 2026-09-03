# Mellomting UX Simplification Plan

Status: ready for implementation
Created: 2026-09-03
Scope: operator-facing CLI, setup flow, generated configuration, errors, and
documentation

## 1. Objective

Make the first successful Mellomting request substantially easier without
making the proxy less explicit, less auditable, or less secure.

The desired mental model remains:

> A small authenticated inference router, not an AI platform.

The primary usability problem is that a new operator currently encounters too
many implementation details and too much explanatory output before the first
request: configuration structure, the users file, HMAC pepper generation,
model ACLs, Landlock behavior, install modes, several validation commands, and
long inventories of completed operations.

This work should establish two short, well-supported journeys:

### Local journey (Linux with supported Landlock)

```sh
mellomting init --server http://127.0.0.1:8000
mellomting key create --name local
mellomting serve
```

On a host where Landlock is unavailable, the operator must choose the explicit
degraded posture:

```sh
mellomting init --landlock best-effort --server http://127.0.0.1:8000
mellomting key create --name local
mellomting serve
```

`best-effort` emits one concise warning that filesystem confinement is
unavailable. There is no silent platform-dependent downgrade.

### Production systemd journey

```sh
sudo mellomting install --systemd
sudo editor /etc/mellomting/config.yaml
sudo mellomting key create --name production
sudo systemctl enable --now mellomting
```

These commands are acceptance contracts. If implementation proves one
impossible, update this plan before changing the documented journey.

## 2. Non-negotiable constraints

UX simplification MUST NOT weaken the security requirements in `docs/PLAN.md`
or expand Mellomting into a management platform.

- Authentication, authorization, malformed configuration, and required
  sandbox setup continue to fail closed.
- No management HTTP API, web UI, database, plugin system, OAuth/OIDC, or
  telemetry is introduced.
- Secrets are never printed except for the newly created raw client key, once.
- Pepper and users files are created with restrictive permissions.
- Setup commands refuse symlinks and non-regular targets.
- Existing configuration, pepper, and users files are never overwritten by an
  initialization shortcut.
- Wildcard model access is never inferred. It must remain explicit.
- The daemon does not become responsible for mutating its configuration or key
  store.
- The full multi-backend configuration remains available and deterministic.
- Every security-sensitive shortcut receives automated tests.

When convenience and an easily understood security boundary conflict, preserve
the security boundary.

## 3. UX principles

### 3.1 Optimize the common path

A local operator with one backend and one public model should not need to
manually create YAML, generate a pepper with shell commands, or repeat a model
name when creating the first key.

### 3.2 Keep automation mechanical

Commands may generate random material, create known files, validate inputs,
query servers explicitly supplied by the operator, and render deterministic
configuration. They should not scan for backends, silently broaden access,
guess network exposure, or choose production policy on the operator's behalf.

Model discovery is an initialization operation, not a runtime behavior. Once
written, the configuration is an explicit snapshot: `serve` never changes its
public model surface merely because a server later advertises something
different.

### 3.3 Progressive disclosure

Top-level help and the quick start should show the ordinary workflow. Detailed
limits, sandbox diagnostics, effective configuration, and advanced routing
remain available without dominating the first-run experience.

### 3.4 Make every result actionable

Success output should state only what matters next. Errors should identify:

1. what is wrong;
2. the relevant file, field, or argument; and
3. the next command or edit when one is unambiguous.

### 3.5 One vocabulary everywhere

README examples, command help, generated files, error messages, and installer
output must describe the same command shapes and setup order.

### 3.6 Quiet on success

Successful commands should confirm the outcome in one short line and show only
the next action, if an action is still required. Do not narrate internal steps,
repeat defaults, print security rationale, or list every file and permission in
the ordinary success path.

Detailed state remains available through dedicated inspection commands. Do not
add `--verbose` in this implementation. Quiet output must not hide a degraded
security state, ignored input, partial result, or required operator action.

## 4. Proposed command surface

```text
mellomting init
mellomting serve
mellomting install
mellomting key create|list|enable|disable|revoke
mellomting config check|show-effective
mellomting usage report
mellomting sandbox check
mellomting version
```

Every command should support focused `--help`. Top-level help should be short
and group commands conceptually:

- Start: `init`, `serve`
- Operate: `key`, `usage`
- Inspect: `config`, `sandbox`, `version`
- Install: `install`

### 4.1 Configuration flag

Adopt `--config` as the documented spelling and accept it consistently after
every config-dependent command or subcommand. It is not a root-level flag; the
exact lookup and placement contract is fixed by D1.

### 4.2 Installation command

```sh
mellomting install [--systemd]
```

This is the only installation command shape. Do not add a top-level
`--install` alias for unreleased behavior.

## 5. `mellomting init`

`init` is the central simplification. It owns safe, deterministic creation of
the local configuration inputs that are currently manual.

### 5.1 Responsibilities

When provided one or more inference-server URLs, `init` should:

1. validate the target paths and arguments without writing anything;
2. apply the configured backend-network policy before connecting;
3. query each explicitly supplied server's OpenAI-compatible `/v1/models`
   endpoint with strict time and size bounds;
4. validate the returned model identifiers and construct an explicit routing
   snapshot;
5. show a bounded summary of the discovered routing relationships;
6. render the minimal server-oriented configuration schema;
7. generate a cryptographically random pepper;
8. create a valid empty users file;
9. set restrictive file modes;
10. validate all three rendered artifacts in memory before publication; and
11. print the next commands for key creation and serving.

Candidate interface:

```sh
mellomting init \
  --server http://127.0.0.1:8000 \
  --server http://127.0.0.1:8010
```

Candidate optional flags:

```text
--config PATH     configuration destination
--listen ADDRESS  Unix socket path or TCP host:port
--landlock MODE   required | best-effort | disabled
--dry-run         discover and print the config without writing
```

The first implementation MUST keep this list small. Backend credentials,
remote-network policy, model types, model exclusions, aliases, and custom auth
file locations are configured by editing the generated file. Options that
merely expose the configuration schema do not belong in `init`.

### 5.2 Listener contract

`--listen` accepts one address, without a separate network or plaintext
confirmation flag:

```text
127.0.0.1:8080                  loopback IPv4
[::1]:8080                      loopback IPv6
192.168.1.20:8080               one non-loopback interface
0.0.0.0:8080                    all IPv4 interfaces
[::]:8080                       all IPv6 interfaces
:8080                           all available interfaces
/absolute/path/mellomting.sock  Unix socket
```

An absolute path selects a Unix socket. Every other value must be a valid
`host:port`; the host is empty or an IP literal, and the port is numeric in
1..65535. Reject relative paths, hostnames, URL schemes, bare ports, and
unbracketed IPv6.

The default is `127.0.0.1:8080`. An explicit non-loopback or wildcard address
is itself sufficient operator intent. When TLS is absent, `init` writes the
existing `allow_plaintext_nonloopback: true` setting automatically; there is
no redundant `--allow-plaintext` flag. Runtime may retain one concise warning.

Unix sockets use the existing default mode and safety checks. TLS is invalid on
a Unix socket.

### 5.3 Discovery contract

Discovery MUST be narrow, bounded, and reproducible:

- Query only server URLs explicitly provided by the operator. Do not scan the
  host, network, DNS, container runtime, or service manager.
- Use `GET /v1/models` and require a valid, bounded OpenAI-compatible response.
- Apply the same URL validation and backend-network restrictions used for
  serving before the first connection is made.
- Bound connect, response-header, and total discovery time independently.
- Bound response bytes, number of models, and model-identifier length.
- Treat server responses and model identifiers as untrusted input.
- Reject duplicate server names and malformed, empty, ambiguous, or unsafe
  model identifiers.
- Group the same model identifier across multiple servers so it becomes one
  public model with multiple replicas.
- Preserve the order in which servers were supplied and generate stable names
  so identical inputs and responses produce identical configuration.
- Display a bounded discovered-routing summary before committing files. Show
  at most 20 public models, then the omitted count; the saved config and
  `config show-effective` provide the complete table.
- Fail before writing if any server cannot be validated or queried. Partial
  discovery is not part of the first implementation.
- Never perform model discovery during `serve`, config loading, reload, or a
  background task.
- Never transmit Mellomting client credentials to an inference server.

The first implementation supports unauthenticated discovery only. If a server
requires authentication for `/v1/models`, `init` fails with a concise message
and the operator must author that advanced configuration explicitly. Do not add
a credential flag merely to make discovery cover every deployment.

Example preview for a small result:

```text
Discovered:

  qwen3.8-27b
    - local-a
    - local-b

  qwen-coder
    - local-b

Writing config.yaml
Generated auth.pepper
Created empty users.yaml

Next:
  mellomting key create --name local
  mellomting serve
```

### 5.4 Default local files

For a non-system installation, the recommended default is to place the three
files together in the current working directory:

```text
config.yaml
users.yaml
auth.pepper
```

The generated config references absolute users and pepper paths, so behavior
does not depend on the working directory used by `serve`.

### 5.5 File safety

`init` MUST:

- perform a complete preflight before its first mutation;
- complete all network discovery before its first mutation;
- refuse if any destination already exists;
- reject symlinks and other non-regular destination conditions;
- require the destination directory to be owned by the effective user and not
  group- or world-writable;
- avoid following a hostile final path component;
- create the pepper and users file as `0600` for local use;
- use atomic writes and sync behavior consistent with existing secure file
  helpers;
- clean up only files created by the failed invocation, and only when their
  identity can be established safely; and
- never print the pepper.

Crash recovery is fail-closed: clearly report which create-only files remain
for manual inspection, and never overwrite them on a re-run.

### 5.6 Bare invocation

`mellomting init` without `--server` is a usage error and prints one short
example. Only systemd installation creates the deliberately invalid scaffold.

## 6. Key-management simplification

### 6.1 Safe model inference

For `mellomting key create`:

- If `--models` is supplied, it is authoritative.
- If it is absent and the config contains exactly one public model, grant that
  model.
- If it is absent and the config contains zero or multiple public models, fail
  with an actionable error.
- Never infer `*`.

Example:

```sh
mellomting key create --name local
```

The output should make the inferred ACL visible before or beside the one-time
key output without making the raw key difficult to capture safely.

### 6.2 Output contract

The raw key is printed exactly once with a trailing newline, and it is the only
stdout content. Explanatory text goes to stderr. This supports:

```sh
MELL_KEY=$(mellomting key create --name automation)
```

without fragile text parsing.

### 6.3 Reload guidance

After a mutation, print only the action that actually applies the change:

- ordinary mode: reload or restart, according to supported behavior;
- required Landlock with pinned users-file inode: restart.

Do not describe the underlying inode mechanics in the primary success path.
Detailed rationale belongs in diagnostics and documentation.

## 7. Configuration simplification

### 7.1 User-facing server-oriented schema

The configuration should describe the concepts operators directly manage:
inference servers and the public models routed to them. For example, discovery
from two explicitly supplied servers may produce:

```yaml
version: 1

servers:
  local-a:
    url: http://127.0.0.1:8000
  local-b:
    url: http://127.0.0.1:8010

models:
  qwen3.8-27b:
    servers: [local-a, local-b]
  qwen-coder:
    servers: [local-b]
```

Safe defaults supply the listener, auth-file locations, routing strategy,
limits, timeouts, logging, and Landlock settings. Fields only need to appear
when the operator changes those defaults.

This object-valued YAML representation is the canonical compact form. The
result MUST remain approximately this small for the ordinary one- or
two-server case.

### 7.2 Normalization and runtime behavior

The loader should normalize the server-oriented form into the existing internal
backend/model routing representation. Runtime routing remains explicit and
deterministic:

- each public model names the servers allowed to serve it;
- the same discovered ID on several servers forms a replica set;
- the existing deterministic routing strategies continue to apply;
- a server advertises no new public models after initialization;
- a missing model at runtime is a bounded backend failure, not a reason to
  rewrite configuration; and
- reload never performs discovery or widens Landlock policy.

The normalized representation should remain internal. Operators should not
have to understand duplicated synthetic backend entries created solely because
one server hosts multiple models.

### 7.3 Source schema

The server-oriented schema is the sole accepted source schema. It must expose
advanced fields for credentials, timeouts, weights, admission settings,
upstream model names, and routing strategies without exposing synthetic
internal backend entries. The loader normalizes it into the existing internal
representation before strict validation. There is no parser or migration path
for the development-era `backends:` source form.

### 7.4 Naming and collisions

By default, a discovered model ID becomes its public model name and its
upstream model name. Model exclusion, type correction, and public aliases are
performed by editing the generated config in the first implementation.

When two servers advertise the same model ID, group them. When different model
IDs are assigned the same public alias, fail and require an explicit correction.
Never silently overwrite a mapping.

Server names generated from repeated `--server` arguments must be stable,
valid configuration identifiers and collision-free. Avoid deriving names from
credentials, full hostnames, or other sensitive URL components.

### 7.5 Scaffold and effective config

- Keep the installed scaffold short and focused on required edits.
- Omit commented settings whose defaults are safe and discoverable.
- Keep `config show-effective` as a diagnostic/reference command rather than a
  setup step.
- Make `config show-effective` expose the normalized routing view clearly
  enough to audit what the minimal form means.
- Make `config check` success concise and failures actionable.

### 7.6 TLS

Static TLS uses the presence of its two file fields; there is no mode field:

```yaml
server:
  listen:
    network: tcp
    address: :8443
  tls:
    cert_file: /etc/mellomting/tls/cert.pem
    key_file: /etc/mellomting/tls/key.pem
```

Presence of both file fields enables static TLS. Both are required together;
TLS remains invalid for Unix sockets. A `mode` field is rejected as unknown.

Native ACME remains shelved. If implemented later, it should use an explicit
nested `tls.acme` block rather than forcing static-certificate users to select
a generic mode.

## 8. Systemd installation

`mellomting install --systemd` remains the supported production provisioning
path.

It should:

- install the binary;
- create or validate the service account and directories;
- create the scaffold, pepper, and empty users store only when appropriate;
- install the hardened unit and logrotate policy;
- reload systemd; and
- print only the remaining operator actions.

### 8.1 Existing configuration

The installer must not preserve an existing config while blindly claiming that
fixed default auth files are its active credentials.

For an existing config, leave auth artifacts operator-managed: do not create
custom parent directories, custom auth files, or unrelated default auth files.
A retry may create a missing fixed default auth artifact only when the existing
regular, non-symlink config explicitly names that exact default path. This rule
works across scaffold revisions and does not claim ownership based on byte
identity. Existing files are never rewritten. The installer reports only
remaining work.

This is also the medium-severity issue recorded in
`DIFFERENTIAL_REVIEW_REPORT.md`.

### 8.2 Output

Routine success should resemble:

```text
Mellomting installed.

Next:
  1. Run: sudo editor /etc/mellomting/config.yaml
  2. Run: sudo mellomting key create --name production
  3. Run: sudo systemctl enable --now mellomting
```

Detailed artifact paths and modes remain available in documentation, not mixed
into the normal next-step instructions.

The installer MUST NOT start a deliberately invalid configuration
automatically.

## 9. Errors and diagnostics

Adopt a consistent error shape for human-facing CLI commands:

```text
mellomting: <short problem>
  <relevant context>
  next: <specific command or edit>
```

Examples:

```text
mellomting: configuration has no backends
  file: /etc/mellomting/config.yaml
  next: add an entry under backends, then run `mellomting config check`
```

```text
mellomting: no API keys exist
  next: run `mellomting key create --name production`
```

Requirements:

- Never include raw backend response bodies, secrets, prompts, responses,
  filesystem content, or stack traces.
- Preserve stable non-zero exit classes: operational/configuration failure
  versus command-usage failure.
- Do not add a new output framework unless repetition demonstrates a real need.
- Keep detailed security rationale out of routine messages while retaining it
  in documentation.

## 10. Human output policy

CLI output is part of the simplified product surface. All existing and new
commands should follow these rules.

### 10.1 Default success output

- Prefer one line for a completed operation.
- Add a `Next:` block only when the operator must still do something.
- Keep the block to at most three actions and place them in execution order.
- Do not print an inventory of files, owners, modes, directories, defaults, or
  internal phases after an ordinary successful command.
- Do not repeat information already supplied in the command unless it confirms
  an important security decision, such as an inferred model ACL.
- Do not include design citations or implementation rationale.

Examples:

```text
Configuration is valid.
```

```text
stdout: sk-local-4f92c16a0b7de831-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
stderr: Created API key "local" for qwen3.8-27b.
stderr: Restart Mellomting to apply it.
```

```text
Mellomting installed.

Next:
  sudo editor /etc/mellomting/config.yaml
  sudo mellomting key create --name production
  sudo systemctl enable --now mellomting
```

### 10.2 Warnings and errors

- Print a warning only when the operator can act on it or when security is
  operating below the normal posture.
- Deduplicate warnings arising from the same root cause.
- State the consequence before the underlying mechanism.
- Keep the ordinary message short; point to an inspection command or
  documentation for details.
- Never turn a fail-closed condition into a warning merely to reduce text.

For example, prefer:

```text
Key saved. Restart Mellomting to apply it.
```

over an explanation of atomic replacement, pinned inodes, SIGHUP behavior, and
Landlock in every key command.

### 10.3 Detailed and machine-readable output

- Inspection commands such as `config show-effective`, `sandbox check`, and
  `key list` may remain naturally detailed because their purpose is to display
  state.
- Secret-producing commands must keep their stdout contract stable and easy to
  capture; explanatory text and warnings should not be interleaved with a raw
  secret on stdout.
- Do not add global quiet/verbosity machinery until at least two commands need
  it. A small shared convention is preferable to an output framework.

### 10.4 Output budgets

Use these as review targets rather than byte-level protocol guarantees:

| Operation | Default human-output target |
|---|---|
| `init` | discovered routing summary, one completion line, at most two next actions |
| `install` | one completion line; at most three next actions |
| `config check` | one line on success; concise field errors on failure |
| `key create` | ACL confirmation, raw key once, one apply instruction |
| key mutation | one completion line and one apply instruction |
| `serve` startup | one structured summary plus actionable warnings only |
| inspection commands | enough detail to fulfill the explicit inspection request |

Tests should pin essential content and absence of known noise rather than
entire prose blocks, so wording can improve without rewriting broad golden
fixtures.

## 11. Documentation structure

Rewrite operator documentation around tasks rather than subsystems.

Recommended order for `README.md`:

1. What Mellomting is
2. Three-command local quick start
3. Production systemd setup
4. Connect a client
5. Common operations
6. Advanced configuration
7. Security model
8. Development

Documentation requirements:

- The local quick start must be executable by copying its commands.
- The systemd quick start must match installer output exactly.
- README, CLI help, config scaffold, service comments, and tests must not
  contradict one another.
- Remove stale claims that the installer does not create auth material.
- Keep `docs/PLAN.md` as the complete design/security source of truth; the
  README should not become a second exhaustive reference.

## 12. Fixed implementation decisions

Sections 12–18 are the decision-complete execution contract. Implement the
ledger tasks in order; each task is independently reviewable, testable, and
committable. Before each task, read `AGENTS.md` and every named source file and
direct test, preserve unrelated worktree changes, and stop if a security
invariant cannot be preserved.

References such as “implement D15” name a fixed decision in this section.
Ledger identifiers such as task `D1` name implementation steps in section 15;
the two namespaces are intentionally separate.

### D1. Configuration lookup

Every config-dependent CLI command uses one resolver:

1. an explicit `--config PATH`, when supplied;
2. `./config.yaml`, when it exists as a regular non-symlink file; otherwise
3. `/etc/mellomting/config.yaml` only when `./config.yaml` is absent.

If the local path exists as a symlink (including dangling), directory, FIFO,
device, or other non-regular file, fail instead of falling through to `/etc`.

`mellomting init` is different because it creates a file: its default
destination is an absolute path formed from the current working directory and
`config.yaml`. It never falls back to `/etc`.

The generated config contains absolute users-file and pepper-file paths.
Changing the working directory therefore does not change the auth files used.

Document and test `--config` with two dashes. Do not add compatibility logic
for development-era spellings. Do not implement a root-level global flag:
`mellomting --config X serve` remains invalid. The accepted shape is:

```text
mellomting <command> [subcommand] --config PATH
```

### D2. Local sandbox behavior

`init` defaults to `security.landlock.mode: required` with the existing
default minimum ABI.

Before writing, it runs the equivalent of the Landlock capability check:

- supported Linux host meeting the minimum: continue;
- unsupported/too-old host: fail without writing and show the explicit
  `--landlock best-effort` alternative;
- `--landlock best-effort` or `--landlock disabled`: accept only when the
  operator supplied it explicitly and write that choice into config.

There is no platform-dependent silent downgrade.

### D3. Bare initialization

`mellomting init` requires at least one `--server`. Bare `init` is a usage
error and prints one short example. It does not create an incomplete scaffold.
Systemd installation retains its separate commented scaffold behavior.

### D4. Initialization transaction

The first implementation requires config, users, and pepper to share one parent
directory. `--config` may select the config filename; its sibling auth files
have the fixed names `users.yaml` and `auth.pepper`.

Initialization follows this order:

1. resolve absolute paths;
2. validate arguments, platform policy, every destination, and the destination
   directory's ownership and mode;
3. complete all server discovery;
4. render config and users bytes in memory and generate pepper bytes;
5. validate the in-memory config/users/pepper representation;
6. create three mode-0600 temporary files in the destination directory;
7. write and fsync each temporary file;
8. publish pepper and users using an atomic create-only hard link from each
   temporary file to its final name, then unlink their temporary names;
9. fsync the parent directory, establishing both auth names durably;
10. publish the resolved config destination with the same create-only link,
    then unlink its temporary name; and
11. fsync the parent directory again.

The resolved config destination is the completion marker because its directory
entry is not published until both auth entries are durably synchronized.

Do not use `os.Rename` to publish these files: it can replace a destination
created after preflight. A same-directory, directory-FD-relative
`linkat(temp, final)` fails when the final path exists and does not expose
partial contents.

The destination directory must be owned by the effective user and must not be
group- or world-writable. Revalidate the opened directory identity before
publication. Create, link, unlink, and sync relative to that held directory FD
without resolving the parent pathname again; use the narrow `x/sys` primitives
needed for this contract. This explicitly excludes hostile shared directories
and same-UID attackers from the first implementation's threat model while
closing parent-path replacement races by other users.

On every ordinary error, remove temporary names and remove only a final file
whose device/inode matches the file this invocation created. On process crash,
pepper or users may remain without config. A later `init` refuses to overwrite
them and reports their exact paths for manual inspection and cleanup; it does
not emit a shell command containing untrusted path text or guess ownership.

This is fail-closed, not fully transactional across crashes. The limitation is
documented and tested.

### D5. File modes

Local `init` writes config, users, and pepper as `0600`.

Systemd provisioning retains `0640 root:mellomting` for those files.

### D6. Discovery scope and limits

Discovery queries only explicitly supplied server URLs. Server URL hosts must
be literal IPv4 or IPv6 addresses with explicit ports; an empty host is invalid
and DNS names are not resolved by the first implementation.

Fixed initial limits:

| Limit | Value |
|---|---:|
| Servers per init | 16 |
| Models per server | 256 |
| Unique public models | 1,024 |
| Response bytes per server | 1 MiB |
| Model ID bytes | 256 |
| Connect timeout | 3 seconds |
| Response-header timeout | 10 seconds |
| Total request timeout | 15 seconds |

These are constants, not initial CLI flags.

Discovery uses `GET <base_url>/v1/models` with:

- MPTCP disabled through the existing backend dialer mechanism;
- the derived backend-network policy enforced on every connection;
- HTTP proxying disabled regardless of `HTTP_PROXY`, `HTTPS_PROXY`,
  `ALL_PROXY`, or lowercase equivalents;
- redirects disabled;
- response compression disabled;
- no retries;
- no client Authorization, Proxy-Authorization, X-Api-Key, forwarding, or
  proxy-identity headers;
- `Accept: application/json`; and
- no request body.

Accept only HTTP 200. Parse a present `Content-Type` with
`mime.ParseMediaType`; accept only `application/json`, with parameters allowed.
Also accept a truly absent header. Reject malformed and other media types.
Never include a raw response body in errors or logs.

Derive the policy without another CLI flag:

- when every server is loopback, use `loopback-only`;
- otherwise use `allowed-cidrs` containing every unique server IP, including
  loopback members of a mixed server set, as a `/32` IPv4 or `/128` IPv6
  prefix.

Write that same policy into the generated config. An explicit `--server` is
the operator's authorization for that exact destination; it does not authorize
other addresses.

### D7. Discovery response

Decode this bounded subset:

```json
{
  "object": "list",
  "data": [
    {"id": "qwen3.8-27b", "object": "model"}
  ]
}
```

Unknown fields pass through the decoder and are ignored. Require:

- one JSON document and EOF after trailing whitespace;
- top-level `object == "list"`;
- non-null `data`;
- each entry has `object == "model"`;
- each `id` is valid UTF-8, non-empty, at most 256 bytes, has no Unicode
  control or format character, no Unicode line/paragraph separator, and no
  leading/trailing Unicode whitespace;
- no duplicate ID within one server; and
- the configured model/server bounds.

Do not infer capabilities from other response fields.

### D8. Model type

The OpenAI models list does not provide a portable generation-versus-embedding
capability signal. Discovery defaults every model to `type: generation`.
Embedding types are selected by editing the generated config. No CLI flag,
filename heuristic, model-name heuristic, or inference request is used.

### D9. Server arguments and names

Accepted server syntax:

```text
--server URL
--server NAME=URL
```

Rules:

- a sole unnamed server is named `local`;
- multiple unnamed servers are named `local-1`, `local-2`, in argument
  order;
- if any explicit name is used, all servers must have explicit names;
- explicit names match `^[a-z][a-z0-9-]{0,62}$`;
- names are unique;
- URLs pass the existing backend base-URL validation, including a literal IP,
  explicit port, and no userinfo, path, query, or fragment; and
- canonical `(scheme, IP, port)` destinations are unique after normalizing IP
  spellings and unmapping IPv4-mapped IPv6 addresses;
- argument order is preserved in preview output and normalization.

The first implementation discovers only servers whose `/v1/models` endpoint is
available without authentication. Backend credentials remain supported in
hand-authored config but are not accepted by `init`.

### D10. Discovery failure and preview

If any server fails, `init` fails before writing. There is no
`--allow-partial` option.

`init` is non-interactive by default. It prints the bounded discovered-routing
summary and immediately writes the files. Failure to write that pre-publication
summary aborts before filesystem mutation.

`--dry-run` performs all validation and discovery, writes the exact config YAML
and nothing else to stdout, writes bounded discovery context to stderr, and
writes no files. It never prints `Writing`, completion, or next-action claims.
Do not add a confirmation prompt or `--yes`.

### D11. Source configuration schema

Keep configuration `version: 1`. The server-oriented form is the sole accepted
source schema; the development-era `backends:` form is removed rather than
migrated or accepted in parallel.

Source form:

```yaml
version: 1

server:
  listen:
    network: tcp
    address: 127.0.0.1:8080

auth:
  users_file: /absolute/path/users.yaml
  pepper_file: /absolute/path/auth.pepper

servers:
  local-a:
    url: http://127.0.0.1:8000
  local-b:
    url: http://127.0.0.1:8010

models:
  qwen3.8-27b:
    servers: [local-a, local-b]
  qwen-coder:
    servers: [local-b]
```

Server fields:

```yaml
servers:
  NAME:
    url: URL
    api_key_file: PATH       # optional
```

Model fields:

```yaml
models:
  PUBLIC_NAME:
    type: generation         # optional, default generation
    servers: [SERVER_NAME]
    upstream_model: ID       # optional, default public map key
    strategy: least-inflight # optional; single when one server,
                             # least-inflight when several
    policy:                  # optional
      max_output_tokens: 32768
```

Reject `backends:` and `qualifiers:` as unknown source fields. Qualifiers remain
shelved. `config show-effective` prints a fully defaulted server-oriented
version-1 document that can be fed back into `config check`; synthetic internal
backend names never appear in operator-facing configuration.

The config package uses one parsing pipeline that produces both a fully
defaulted source projection and the normalized runtime `Config`. Ordinary
`Load` returns only the runtime value; `config show-effective` serializes the
source projection from that same invocation. It must not reverse-engineer
servers or names from synthetic normalized backends.

Static TLS source configuration has no mode field:

```yaml
server:
  listen:
    network: tcp
    address: :8443
  tls:
    cert_file: /etc/mellomting/tls/cert.pem
    key_file: /etc/mellomting/tls/key.pem
```

When either file field is present, require both and normalize to the existing
internal static-TLS state. An empty TLS block enables nothing. Reject `mode`,
`acme`, and other unknown TLS source fields. Native ACME remains shelved.

### D12. Deterministic normalization

Normalize source form before applying defaults and validation.

For each compact public model, in UTF-8 bytewise sorted public-model order, and
each referenced server in YAML order:

1. verify the server exists and is not repeated;
2. compute `sha256(publicModel + "\\x00" + serverName)`;
3. create the internal backend name
   `"auto-" + serverName + "-" + first 12 lowercase hex characters of the
   digest`;
4. reject the impossible-but-checked generated-name collision;
5. copy server URL and API-key path into the backend;
6. set `upstream_model` to the discovered/public model ID;
7. create a backend reference with weight 1.

The normalized model copies `type`, `strategy`, and `policy`. Empty type
defaults to `generation`. Empty strategy defaults to `single` for one
server, otherwise `least-inflight`.

The resulting runtime `Config.Backends` and `Config.Models` use the existing
types and contain no source-only state. The separate effective source
projection preserves operator server names and replica order only for
validation and `config show-effective`.

Discovery-generated public names equal upstream IDs. Aliasing is done by
editing the compact model map key and adding:

```yaml
upstream_model: ORIGINAL_ID
```

to that compact model. When absent it defaults to the public name.
Normalization uses `upstream_model` for the internal backend and the map key
for the public model.

### D13. Config path behavior

Source paths use the semantics defined in `docs/PLAN.md`. `init` writes
absolute paths, so generated files introduce no working-directory dependency.

### D14. Key creation

When `--models` is absent:

- exactly one configured public model: infer it;
- zero models: fail;
- two or more models: fail and list only the model names, sorted, with a
  request to pass `--models`; show at most 20 names plus the omitted count and
  direct the operator to `config show-effective` for the complete set;
- never infer `*`.

On success, stdout contains the raw key and a trailing newline only. All human
context goes to stderr:

```text
Created API key "local" for qwen3.8-27b.
Restart Mellomting to apply it.
```

### D15. Installation command

Use `mellomting install [--systemd]` as the sole installation command. Do not
implement a top-level `--install` alias.

For an existing systemd config:

- do not require it to validate, because a previously installed scaffold is
  deliberately invalid until edited;
- if the regular, non-symlink config explicitly names a fixed default auth
  path, create that artifact only when it is missing;
- otherwise never create custom auth files, parent directories, or unrelated
  default auth files;
- report auth setup generically as remaining operator work; and
- leave all existing auth files untouched.

For a newly created default scaffold, continue generating the fixed default
pepper and empty users file.

Determine referenced default auth paths with a bounded, no-side-effect YAML
source parse that does not require the deliberately incomplete scaffold to pass
full config validation. A parse failure creates no auth files.

### D16. Human output

Default success output follows section 10 of this plan. In particular:

- install: one completion line and at most three next actions;
- config check: one line;
- key mutation: one completion line plus one restart/reload action;
- no default artifact/permission inventory;
- no design rationale in routine output.

Do not add `--verbose` in the first implementation. Existing inspection
commands provide detail. Add it later only if a concrete diagnostic cannot fit
an inspection command.

### D17. Listener syntax

`init --listen` defaults to `127.0.0.1:8080` and accepts:

```text
127.0.0.1:8080
[::1]:8080
192.168.1.20:8080
0.0.0.0:8080
[::]:8080
:8080
/absolute/path/mellomting.sock
```

Parsing is deterministic:

1. a value beginning with `/` is a Unix socket and must be an absolute,
   cleaned path;
2. every other value is parsed with `net.SplitHostPort`;
3. the host is empty or a literal IP address;
4. the port is decimal in 1..65535.

This deliberately changes the current config validation: an empty TCP host is
accepted as a wildcard bind, subject to the same TLS-or-explicit-plaintext
policy as `0.0.0.0` and `[::]`.

Reject relative paths, hostnames, URL schemes, bare ports, zone-scoped IPv6,
and unbracketed IPv6.

For Unix, generate `network: unix`, the path, and the existing default socket
mode. For TCP, generate `network: tcp` and the address. If the TCP host is
empty, unspecified, or non-loopback and static TLS is absent, also generate
`allow_plaintext_nonloopback: true`. The address itself is the explicit
operator decision; there is no second confirmation flag.

`init` has no TLS flags in the first implementation. TLS is enabled by editing
the generated config with the two file fields in D11.

### D18. API-key format

Every generated and accepted client API key has exactly this form:

```text
sk-<username>-<keyid>-<secret>
```

The grammar is deliberately strict:

- `username` matches `^[a-z][a-z0-9]{0,31}$`;
- `keyid` is 8 random bytes encoded as exactly 16 lowercase hexadecimal
  characters;
- `secret` is 32 random bytes (256 bits) encoded as exactly 64 lowercase
  hexadecimal characters; and
- no segment is empty and no additional separator or suffix is accepted.

Examples:

```text
sk-alice-4f92c16a0b7de831-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
sk-alice2-b8170d34ac290fe6-fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
```

Multiple keys may use the same username because `keyid` distinguishes them.
Operators who want a human suffix may append lowercase letters or digits within
the username grammar, such as `alice2`; hyphens, underscores, dots, uppercase,
whitespace, and Unicode are rejected. The username segment equals `key create
--name` and is stored as the key's human-visible name.

Generate the ID and secret independently with `crypto/rand`. Key creation
checks ID uniqueness against the loaded users file and retries a collision at
most eight times before failing without mutation. Authentication extracts the
fixed-format ID, performs the existing dummy-HMAC path for unknown IDs, and
verifies the HMAC of the complete raw key in constant time. For a matching
hash, it also requires the parsed username to equal the stored key name. Raw
keys are bounded to 117 bytes by the grammar and oversized inputs are rejected
before parsing individual segments.

Only this grammar is implemented. Update all repository fixtures,
documentation, and examples in the task that changes the generator.

## 13. Security invariants

These requirements are release blockers:

- Discovery never connects outside the policy derived from explicit servers.
- Every discovery connection has MPTCP disabled.
- Redirects cannot escape the validated destination.
- No client credential or proxy identity header reaches a server.
- Init accepts no backend credentials and never invents authentication policy.
- Raw discovery error bodies are never logged or returned.
- All discovery inputs and outputs are bounded.
- Discovery completes before filesystem mutation.
- Init never overwrites an existing final path.
- Init never follows a final-component symlink.
- New local config, users, and pepper files are mode 0600.
- Pepper comes from `crypto/rand` and has 64 random bytes before base64.
- The empty users file authenticates nobody.
- Server-oriented source config is normalized before the strict internal
  validation boundary.
- Development-era `backends:` source config is rejected.
- Runtime never discovers, adds, removes, or remaps models.
- Wildcard key authorization is never inferred.
- Raw API keys and peppers are never logged.
- A username parsed from a presented credential is never logged.
- API-key usernames and both hexadecimal segments satisfy D18 exactly.
- Only the D18 key format is accepted.

## 14. Package and file map

New files:

```text
internal/discovery/discovery.go
internal/discovery/discovery_test.go
internal/config/source.go
internal/config/source_test.go
cmd/mellomting/config_path.go
cmd/mellomting/config_path_test.go
cmd/mellomting/init.go
cmd/mellomting/init_test.go
integration/ux_journey_test.go
```

Expected modified files:

```text
internal/config/config.go
internal/config/load.go
internal/config/validate.go
internal/config/config_test.go
internal/backend/backend.go          # only if a narrow dial/policy helper
                                      # must be exposed internally
internal/systemd/systemd.go
internal/systemd/systemd_test.go
cmd/mellomting/main.go
cmd/mellomting/cli_unit_test.go
cmd/mellomting/key_cli_test.go
cmd/mellomting/install.go
cmd/mellomting/install_test.go
README.md
docs/PLAN.md
deploy/mellomting-config.yaml.example
internal/systemd/mellomting-config.yaml.example
```

Do not add a third-party dependency. Use the standard library plus existing
internal packages.

## 15. Atomic implementation tasks

### Execution ledger

Complete one row at a time. Do not start a later row while the current row has
uncommitted implementation changes or a failing required check.

| Task | Deliverable | Status |
|---|---|---|
| A0 | promote fixed UX contracts into `docs/PLAN.md` | complete |
| A1 | truthful systemd provisioning and concise install output | complete |
| A2 | shared CLI config-path resolver | complete |
| A3 | static TLS source fields without a mode selector | complete |
| B1 | server-oriented source config and normalization | complete |
| B2 | canonical server-oriented effective config | complete |
| B3 | bounded pure `/v1/models` parser | complete |
| B4 | policy-enforced discovery HTTP client | complete |
| B5 | deterministic multi-server aggregation | complete |
| B6 | init flags, listener parsing, and preflight | complete |
| B7 | in-memory config/auth rendering and validation | complete |
| B8 | create-only init filesystem commit | pending |
| B9 | end-to-end init command and documentation | pending |
| C1 | safe sole-model key inference | pending |
| C2 | strict username-bearing API-key format | pending |
| C3 | script-safe one-time key output | pending |
| D1 | conventional install command | pending |
| D2 | concise output across remaining commands | pending |
| E1 | final design, schema, and scaffold docs | pending |
| E2 | full local and Linux validation | pending |

For each task:

1. record the starting commit and confirm unrelated worktree files;
2. read every file named by the task and its direct tests;
3. implement only the stated deliverable;
4. run `gofmt` on changed Go files;
5. run the task-specific verification command;
6. run `go test ./...` before committing;
7. inspect `git diff --check` and the complete task diff;
8. commit with the stated message only after all required checks pass; and
9. change the ledger row to `complete` in the same commit or in the next
   planning-document commit.

If a check fails for a demonstrably pre-existing reason, record the exact
command and evidence in the task handoff. Do not silently waive it.

### A0. Update the authoritative design specification

Modify:

- `docs/PLAN.md`

Promote the fixed behavior in D1–D18 and the applicable security invariants
from section 13 into the RFC 2119 design specification before changing code.
Remove the development-era source schema, API-key grammar, and install command
shape instead of documenting migrations or aliases. Keep this UX plan as the
execution ledger, but make `docs/PLAN.md` authoritative for every subsequent
implementation task as required by `AGENTS.md`.

Verify:

```sh
rg -n 'sk-<username>-<keyid>-<secret>|server-oriented|mellomting install' docs/PLAN.md
```

Confirm each required contract appears in its normative section. Review all
operator-facing examples. The finished specification must describe one source
schema, key grammar, TLS shape, and install command, without a transition or
dual-parser design.

Commit:

```text
specify the simplified operator interface
```

### A1. Align current documentation and installer correctness

Goal: establish a truthful baseline before adding new UX.

Modify:

- `README.md`
- `internal/systemd/systemd.go`
- `internal/systemd/systemd_test.go`
- `cmd/mellomting/install.go`
- `cmd/mellomting/install_test.go`

Implement:

- correct stale claims about installer-generated auth material;
- apply D15 to existing configs;
- replace verbose default install output with D16 output;
- retain detailed errors when provisioning fails.

Tests:

- fresh install creates the scaffold and both fixed auth artifacts;
- retry after a partial fresh install creates only a missing fixed auth
  artifact when the config explicitly names its fixed default path;
- copied and older scaffold revisions follow the same path-based rule;
- a customized or unrelated existing config creates no auth artifact;
- symlink, non-regular, and destination-race cases create nothing;
- existing config and auth bytes are never rewritten;
- success output stays within the D16 budget; and
- failures retain an actionable, sanitized error.

Must not:

- create files outside fixed default paths;
- rewrite an existing config or auth file;
- start the service; or
- weaken preflight.

Verify:

```sh
go test ./internal/systemd ./cmd/mellomting
```

Commit:

```text
fix install provisioning and simplify its output
```

### A2. Introduce the shared config-path resolver

Modify/create:

- `cmd/mellomting/config_path.go`
- `cmd/mellomting/config_path_test.go`
- all config-dependent command flag setup in `cmd/mellomting`

Implement D1. Use `os.Lstat` for `./config.yaml`; accept it only when it is
a regular non-symlink file. An existing symlink is an error, not a reason to
fall through to `/etc`.

Apply the resolver to `serve`, `config check`, `config show-effective`, every
`key` mutation/list operation, `usage report`, and optional-config
`sandbox check`. Preserve sandbox check's ability to run without a config when
no local or system config exists.

Tests:

- explicit path wins;
- cwd regular file wins;
- absent cwd file falls back to `/etc`;
- cwd symlink, dangling symlink, and every non-regular path fail;
- `--help` performs no lookup for every command above;
- `--config` works at the documented post-command or post-subcommand position
  for every command above;
- root-level placement is rejected; and
- an explicitly named missing path reports that path and never falls back.

Verify:

```sh
go test ./cmd/mellomting
```

Commit:

```text
use consistent config discovery across CLI commands
```

### A3. Simplify static TLS source configuration

Modify:

- `internal/config/validate.go`
- `internal/config/config_test.go`
- `cmd/mellomting/serve_test.go`

Implement the TLS portion of D11:

- both cert/key fields normalize to the internal static-TLS state;
- one field without the other fails validation;
- `mode` and ACME-shaped source fields are rejected as unknown;
- TLS on Unix remains invalid;
- effective config remains valid when parsed again.

Also update TCP listener validation for D17: `:PORT` is a valid wildcard bind.
It is accepted only when TLS is configured or the normalized config contains
`allow_plaintext_nonloopback: true`, exactly like the explicit wildcard IPs.
Add source-config table tests covering all three wildcard spellings and the
secure/plaintext-policy combinations.

Do not change certificate loading, permissions, TLS minimum version, or restart
semantics.

Verify:

```sh
go test ./internal/config ./internal/tlsconfig ./cmd/mellomting
```

Commit:

```text
simplify static TLS configuration
```

### B1. Add the server-oriented source schema

Modify/create:

- `internal/config/config.go`
- `internal/config/source.go`
- `internal/config/source_test.go`

Add source-only types:

```go
type sourceServer struct {
    URL        string `yaml:"url"`
    APIKeyFile string `yaml:"api_key_file"`
}

type sourceModel struct {
    Type          string      `yaml:"type"`
    Strategy      string      `yaml:"strategy"`
    Policy        ModelPolicy `yaml:"policy"`
    Servers       []string    `yaml:"servers"`
    UpstreamModel string      `yaml:"upstream_model"`
}
```

Do not add source-only fields to the normalized public `Config` value returned
by `Parse`. Decode through an unexported source struct, normalize, then apply
defaults and strict internal validation. Remove acceptance of the
development-era `backends:` source form and update all repository fixtures in
this task.

Tests:

- source-schema happy path;
- `backends:` and qualifiers rejected;
- unknown fields rejected;
- missing/duplicate server references rejected;
- default type/strategy;
- upstream alias;
- deterministic generated backend names;
- generated-name collision path unit-tested through an injectable helper;
- fully defaulted source output parses again to the same normalized config.

Verify:

```sh
go test ./internal/config ./internal/routing
```

Commit:

```text
adopt deterministic server-oriented configuration
```

### B2. Make effective config emit canonical source form

Modify:

- `cmd/mellomting/main.go`
- `cmd/mellomting/cli_unit_test.go`

Ensure `config show-effective` emits fully defaulted server-oriented YAML,
never synthetic internal backend names, and that its output passes
`config.Parse` to the same normalized configuration.

Use the effective source projection produced by B1's parsing pipeline. Do not
reconstruct source servers or replica names from normalized backends.

Tests must verify credential paths may appear but credential contents do not.

Verify:

```sh
go test ./cmd/mellomting ./internal/config
```

Commit:

```text
render canonical server-oriented effective config
```

### B3. Add pure discovery response parsing

Create:

- `internal/discovery/discovery.go`
- `internal/discovery/discovery_test.go`

First implement a network-free function:

```go
func ParseModels(r io.Reader, maxBytes int64, maxModels int) ([]string, error)
```

It implements D7 and returns IDs sorted by UTF-8 bytes. Errors identify the
server at the caller layer, not in this pure parser.

Add a fuzz target seeded with valid, truncated, duplicate, multi-document, and
oversized inputs. Fuzzing arbitrary bytes must never panic or allocate beyond
the surrounding byte/model limits.

Table tests additionally cover bidi overrides, zero-width format characters,
Unicode line/paragraph separators, combining characters, and invalid UTF-8.

Verify:

```sh
go test ./internal/discovery
go test ./internal/discovery -run=Fuzz -fuzz=FuzzParseModels -fuzztime=10s
```

Commit:

```text
parse bounded OpenAI model discovery responses
```

### B4. Add the bounded discovery client

Modify/create:

- `internal/discovery/discovery.go`
- `internal/discovery/discovery_test.go`
- a narrow existing backend policy/dial helper only if required

API:

```go
type Server struct {
    Name    string
    BaseURL string
}

type Options struct {
    Network config.BackendNetwork
}

func Fetch(ctx context.Context, server Server, opts Options) ([]string, error)
```

Implement D6. Reuse the production network policy and MPTCP-off dialer; do not
copy security logic into a weaker discovery-only implementation. If reuse
requires refactoring, expose the smallest internal constructor possible.
Add a pure helper that derives `loopback-only` or exact-host
`allowed-cidrs` policy from the ordered server list.

Tests use `httptest` plus injectable dial/policy seams and cover:

- exact method/path/headers;
- bounds and each timeout class;
- redirect refusal;
- sanitized non-200 errors;
- absent, parameterized JSON, malformed, and non-JSON media types;
- auth/proxy header absence;
- proxy environment variables are ignored and the policy-controlled direct
  dialer remains the only connection path;
- loopback-only and derived exact-host allowed-CIDR behavior;
- mixed loopback/non-loopback and IPv4/IPv6 policy derivation, verifying that
  every destination is included and duplicate CIDRs are removed;
- MPTCP-off constructor use; and
- cancellation.

Verify:

```sh
go test ./internal/discovery ./internal/backend
```

Commit:

```text
discover models through the bounded backend client
```

### B5. Add deterministic multi-server aggregation

Create in `internal/discovery`:

```go
type Result struct {
    Models map[string][]string
}

func Aggregate(ordered []Server, discovered map[string][]string) (Result, error)
```

Preserve server argument order within each model's replicas; sort public model
names for rendering. Apply global bounds. Do not use goroutines in the first
implementation: query servers sequentially for deterministic errors and a
small resource footprint.

Tests cover overlapping/disjoint sets, stable ordering, empty server results,
global model bound, and missing result entries.

Verify:

```sh
go test ./internal/discovery
```

Commit:

```text
aggregate discovered models deterministically
```

### B6. Add init argument parsing and preflight

Create/modify:

- `cmd/mellomting/init.go`
- `cmd/mellomting/init_test.go`
- `cmd/mellomting/main.go`

Parse D2, D3, D6, D8, D9, and D17. Separate parsing/preflight from network
and filesystem effects.

Use an injectable Landlock capability seam so supported, too-old, and
unsupported-host behavior is deterministic on every CI platform.

Required flags:

```text
--server URL|NAME=URL       repeatable, at least one
```

Optional flags:

```text
--config PATH
--listen ADDRESS
--landlock required|best-effort|disabled
--dry-run
```

The config, users file, and pepper use fixed filenames in the config
destination directory. This enforces D4 without exposing filesystem layout as
CLI surface.

`--listen` implements D17, including Unix paths, loopback, one-interface,
wildcard IPv4/IPv6, and empty-host wildcard binds. It has no companion
plaintext-confirmation flag.

Tests cover every syntax rule, maximum count, duplicate/conflicting flags and
canonical duplicate destinations,
rejection of removed/unknown convenience flags, explicit Landlock choices, and
side-effect-free help/preflight failure. Listener table tests cover every D17
example plus bare port, relative path, hostname, URL, invalid port, unbracketed
IPv6, and zone-scoped IPv6 rejection.

Verify:

```sh
go test ./cmd/mellomting
```

Commit:

```text
add fail-closed init command preflight
```

### B7. Render and validate init artifacts in memory

Modify:

- `cmd/mellomting/init.go`
- `cmd/mellomting/init_test.go`

Given parsed arguments and aggregate discovery:

- build server-oriented config with the selected listener, absolute auth paths,
  derived exact-destination backend-network policy, selected Landlock policy,
  servers, and generation-default models;
- set `allow_plaintext_nonloopback: true` exactly for plaintext TCP listeners
  whose host is empty, unspecified, or non-loopback;
- generate 64 pepper bytes using `crypto/rand`, base64 plus newline;
- use the existing valid empty users representation;
- parse/normalize the config;
- parse users through an extracted byte parser or a temporary-free equivalent;
- validate pepper length without writing.

Rendering is stable: YAML keys are sorted and server replica order follows
arguments.

Tests compare exact config fixtures, except random pepper bytes, and prove the
rendered config's normalized graph.

Verify:

```sh
go test ./cmd/mellomting ./internal/config ./internal/auth
```

Commit:

```text
render validated init configuration and auth state
```

### B8. Commit init artifacts safely

Modify:

- `cmd/mellomting/init.go`
- `cmd/mellomting/init_test.go`

Implement D4 and D5 in a small helper with injectable filesystem failure
points. Do not reuse an overwrite-capable atomic writer without adding an
exclusive create-only contract.

Tests:

- success and exact modes;
- parent ownership and group/world-write rejection;
- opened-parent identity revalidation;
- every destination already exists;
- final symlink/FIFO/directory;
- temp create/write/fsync/close/link/unlink/directory-sync failures;
- separate failure injection before and after the auth-entry directory sync;
- atomic link publication catches a destination race without replacement;
- temporary-source replacement, parent rename, and ancestor-symlink races;
- rollback removes only matching created inodes;
- config published last;
- crash-residue diagnostic;
- dry-run writes nothing.

Verify:

```sh
go test ./cmd/mellomting
```

Commit:

```text
write init artifacts with create-only semantics
```

### B9. Wire end-to-end init and concise output

Modify:

- `cmd/mellomting/init.go`
- `cmd/mellomting/init_test.go`
- `README.md`

Wire preflight -> discovery -> aggregation -> rendering -> commit. Default
output shows at most 20 model rows plus an omitted count, one completion line,
and two next commands. It never prints pepper or credential contents.

Tests capture stdout and stderr separately. A dry run emits parseable config
YAML only on stdout, bounded discovery context on stderr, no completion/write
claim, and leaves the filesystem unchanged. A broken output stream before
publication also leaves the filesystem unchanged.

Add an end-to-end test with two local fake servers and overlapping models.

Verify:

```sh
go test ./cmd/mellomting ./internal/discovery
```

Commit:

```text
initialize Mellomting from discovered servers
```

### C1. Infer a sole key model

Modify:

- `cmd/mellomting/main.go`
- `cmd/mellomting/key_cli_test.go`

Implement D14 model inference without changing explicit `--models` behavior.
Refactor only enough to keep flag validation independent from config-dependent
inference.

Verify:

```sh
go test ./cmd/mellomting ./internal/auth
```

Commit:

```text
infer sole model when creating API keys
```

### C2. Implement the strict API-key format

Modify:

- `internal/auth/key.go`
- `internal/auth/auth_test.go`
- `internal/auth/fuzz_test.go`
- `internal/auth/store.go`
- `cmd/mellomting/main.go`
- `cmd/mellomting/key_cli_test.go`
- all repository fixtures and documentation containing API keys

Implement D18 as the only generated and accepted key grammar. Change key
generation to accept the validated username, generate independent 8-byte ID
and 32-byte secret values, lowercase-hex encode both, and return the complete
key plus ID. Validate `key create --name` against the D18 username grammar
before entropy use or filesystem mutation.

Tests cover exact length and separators, lowercase-only ID/secret, username
boundaries and every rejected character class, multiple keys for one username,
bounded collision retries, entropy failures, parse/generate round trips,
malformed and oversized input, unknown-ID dummy HMAC, constant-time full-key
hash comparison, and use of the D18 grammar in every repository fixture.

Verify:

```sh
go test ./internal/auth ./cmd/mellomting
go test ./internal/auth -run=Fuzz -fuzz=FuzzParseKey -fuzztime=10s
```

Commit:

```text
adopt strict username-bearing API keys
```

### C3. Make secret output script-safe

Modify:

- key create output and tests in `cmd/mellomting`

Implement D14 stdout/stderr exactly using the D18 key format. Tests capture
both streams and assert the raw key appears once overall and never in logs.

Verify:

```sh
go test ./cmd/mellomting
```

Commit:

```text
make API key creation output script-safe
```

### D1. Use the conventional install command

Modify:

- `cmd/mellomting/main.go`
- `cmd/mellomting/install.go`
- tests and documentation

Implement only `mellomting install [--systemd]`. Reject the development-era
top-level `--install` form as an unknown option. Help is side-effect free.

Verify:

```sh
go test ./cmd/mellomting
```

Commit:

```text
use conventional install command
```

### D2. Normalize remaining human output

Audit:

- install;
- config check/show-effective;
- key create/list/enable/disable/revoke;
- sandbox check;
- usage report;
- serve startup warnings.

Apply D16. Do not reduce explicit inspection output merely to meet a line
count. Do not suppress security-degradation warnings.

Tests should assert required facts and banned noise fragments, not full prose
except where stdout is a machine contract.

Verify:

```sh
go test ./cmd/mellomting
```

Commit:

```text
simplify operator-facing command output
```

### E1. Update operator documents and generated assets

Modify:

- `README.md`
- generated systemd scaffold and its embedded copy

Ensure README, help text, and generated assets match the design already added
to `docs/PLAN.md` by A0: the sole server-oriented schema, init-time discovery,
config lookup, listener syntax, derived backend policy, static TLS, D18 keys,
and the sole install command. Keep embedded/deploy assets byte-identical.

Verify:

```sh
go test ./internal/systemd ./cmd/mellomting
```

Commit:

```text
document simplified initialization and configuration
```

### E2. Full validation

Verify:

```sh
make check
go test -tags=integration -v ./integration -run TestUXJourneys
```

`make check` is the repository authority: it runs `staticcheck` and
`govulncheck` when installed, while primary Linux CI installs and requires
both. Do not blanket-ignore findings beyond the documented Darwin Landlock
SA4023 exception.

`TestUXJourneys` runs on Linux, skips with an explicit reason elsewhere, uses
only temporary directories and local fake model servers, and covers:

1. local init with one server;
2. local init with two overlapping servers;
3. non-Linux behavior through the injected Landlock capability seam;
4. dry-run stdout/stderr and zero-mutation behavior;
5. systemd fresh-install and existing-config fixtures through injected paths
   and service operations;
6. authenticated request to `/v1/models`; and
7. static TLS after init plus a cert/key config edit, proving `mode` is
   unnecessary, plaintext is not served, and partial cert/key config fails
   before bind.

The integration package must not modify the host's real `/etc`, users,
systemd state, or firewall. Race and unsupported-destination cases remain
deterministic unit tests in B6/B8 rather than unsafe live-host exercises.

Record any upstream compatibility adjustment in `docs/COMPATIBILITY.md`.

### Phase checkpoints

After A3:

```sh
gofmt -l .
go test ./...
go vet ./...
```

Confirm both old and simplified TLS configs start the same static TLS listener,
and a pre-existing systemd scaffold can be re-provisioned without validation.

After B9:

```sh
gofmt -l .
go build ./...
go test ./...
go test -race ./internal/config ./internal/discovery ./cmd/mellomting
go vet ./...
```

Fixture assertions cover generated config for one server, two disjoint
servers, and two servers sharing a model. Confirm `--dry-run` leaves the
directory unchanged and its stdout parses as server-oriented config.

After C3:

```sh
go test ./cmd/mellomting ./internal/auth
go test -race ./cmd/mellomting ./internal/auth
```

Capture stdout and stderr separately. Confirm the raw key occurs exactly once
and wildcard access is never inferred.

After D2:

```sh
go test ./cmd/mellomting
```

Review actual output for every top-level command. Confirm routine success is
short and every degraded-security warning is still present.

E2 is the final repository-wide release gate and may not be skipped because an
earlier checkpoint passed.

## 16. Required test fixtures

Keep test fixtures local and deterministic:

- valid one-model response;
- valid multi-model response;
- overlapping two-server responses;
- empty list;
- duplicate ID;
- missing/wrong `object`;
- null/missing `data`;
- overlong/control/whitespace ID;
- trailing JSON document;
- oversized body;
- delayed headers;
- delayed body;
- redirect;
- sanitized backend failure.

Do not add recorded production traffic or real model-server dependencies.

## 17. Review checklist for every task

- Is the change limited to the task?
- Are all new inputs bounded?
- Are URL and path errors sanitized?
- Can a symlink or rename race redirect a privileged write?
- Can a backend response change runtime policy after init?
- Can credentials enter argv, stdout, stderr, logs, config contents, or tests?
- Does failure happen before mutation when possible?
- Does failure retain the prior valid state?
- Is output shorter without hiding degraded security?
- Are development-era source forms rejected rather than silently interpreted?
- Do security-sensitive branches have tests?

## 18. Completion definition

The UX simplification is complete only when:

- the documented three-command local flow works from a clean directory;
- one or more explicit servers populate the config through bounded discovery;
- the saved server-oriented config is small, strict, and deterministic;
- effective config exposes the normalized routing graph;
- `serve` performs no model discovery;
- a sole model is inferred for key creation without inferring wildcard access;
- install and routine operations use concise human output;
- only the documented source schema, API-key format, and command forms are
  accepted;
- all security invariants in sections 2 and 13 have automated coverage; and
- all quality gates pass.
