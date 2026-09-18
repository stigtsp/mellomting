# Differential Security Review

## Executive Summary

| Severity | Count |
|---|---:|
| Critical | 0 |
| High | 0 |
| Medium | 1 |
| Low | 0 |

**Overall risk:** Medium  
**Recommendation:** Conditional approval

> **Status: resolved.** The single Medium finding was fixed in `b3d5bd1` — an existing
> config's auth paths are now honoured rather than overwritten, per the `D15`
> auth-artifact contract in `internal/systemd/systemd.go`. The conditions attached to
> the approval above have been met; this report is retained as history.

The generated pepper and empty users store are cryptographically and
permission-wise sound for the default scaffold. The blocking correctness gap
is that re-provisioning preserves an existing config but ignores the auth paths
that config selects.

**Key metrics:**

- Review range: `origin/main..HEAD` (`63afcb1`, `c8fa854`, `cfa0fc9`)
- Files analyzed: 12/12 changed files
- Changed lines: +496/-342
- High-risk changed functions: 3 (`Provision.Run`, `ensurePepper`, `ensureUsers`)
- Security regressions: 0
- Test coverage gaps tied to the finding: 1 integration case

## What Changed

| Area | Change | Risk |
|---|---|---|
| systemd provisioning | Generate a 64-byte random pepper and valid empty users store | High |
| systemd provisioning | Report created/pre-existing artifacts | Medium |
| config scaffold | Reduce operator comments while preserving an invalid-by-default scaffold | Low |
| CLI/errors | Remove repository-local PLAN references from operator-visible text | Low |

The three commits were authored on 2026-09-02. The high-risk behavior is
concentrated in `internal/systemd`; other edits are documentation, help/error
text, and tests.

## Findings

### MEDIUM: Provisioning ignores auth paths in a preserved existing config

**File:** `internal/systemd/systemd.go:143`, `internal/systemd/systemd.go:153`,
`internal/systemd/systemd.go:157`  
**Commit:** `63afcb1`  
**Blast radius:** one production caller (`installCmd` -> `Provision.Run`)  
**Test coverage:** Partial; helper tests cover fixed paths in isolation, but no
test covers an existing config with custom `auth.users_file` or
`auth.pepper_file`.

`Provision.Run` deliberately preserves an existing `/etc/mellomting/config.yaml`
but then unconditionally provisions `/etc/mellomting/auth.pepper` and
`/etc/mellomting/users.yaml`. Both paths are configurable. The post-install
report nevertheless says the fixed files are the installed auth artifacts,
while the recommended `key create -config /etc/mellomting/config.yaml` follows
the custom paths from the preserved config.

**Concrete failure scenario:**

1. An operator has an existing config with `pepper_file:
   /srv/mellomting/auth.pepper` and `users_file:
   /srv/mellomting/users.yaml`.
2. Root runs `mellomting --install --systemd` to install or upgrade the service.
3. The installer leaves the config untouched, creates secrets at the two fixed
   `/etc/mellomting` paths, and reports provisioning complete.
4. The printed `key create` command loads the preserved config and attempts to
   use `/srv/mellomting/auth.pepper`; if it is absent, key creation fails. If the
   custom files already exist, the newly generated default files are unused
   secret material left on disk and the report is misleading.

**Impact:** The installer does not fulfill its stated end-to-end provisioning
contract for a supported configuration. This is primarily an availability and
operator-safety failure, not an authentication bypass: daemon/config loading
continues to fail closed.

**Recommendation:** Couple auth-file provisioning to the effective config.
When the scaffold was newly created, provisioning the known default paths is
correct. When a config already exists, either:

1. strictly load it and provision its configured auth paths after validating
   that their parent directories and ownership policy are safe; or
2. do not create default auth files, report that auth artifacts were not
   managed, and instruct the operator using the configured paths.

Add an integration-level test for a pre-existing config whose two auth paths
are non-default. Assert that the report never claims unrelated default files
are the active credentials.

## High-Risk Function Context

### `(*Provision).Run` (`internal/systemd/systemd.go:103`)

**Purpose:** This is the privileged orchestration entry point for systemd
installation. It crosses from CLI-controlled installation state into root-owned
accounts, directories, secrets, unit files, and a systemd reload.

**Inputs & assumptions:** `BinaryPath` is caller-provided but preflighted as an
absolute regular file; `User` defaults to `mellomting`; the process is root on a
Linux systemd host; fixed filesystem paths have their documented meanings; an
existing config is operator-owned state; resolved UID/GID values are numeric;
external `useradd` and `systemctl` binaries at allow-listed paths are trusted.

**Outputs & effects:** Returns a creation report or an error; may create a
service account; creates/adopts and changes ownership/mode of four directories;
may create three config/auth files; atomically replaces the unit and logrotate
files; executes `systemctl daemon-reload`.

**Block analysis:**

- Lines 104-125 validate host/binary state and resolve the service identity
  before filesystem writes. This ordering is necessary because privileged
  writes must not occur on a doomed host; it assumes account lookup faithfully
  describes the identity systemd will use. First principle: establish the
  principal and target before granting either access.
- Lines 127-137 establish strict directory ownership/modes. This must precede
  secret creation so temporary and final files live under a root-controlled
  parent. Why: without a protected parent, final-component symlink checks alone
  do not prevent namespace races.
- Lines 139-160 preserve/create config and create auth material. Why: key
  creation and daemon startup require all three. The missing dependency is that
  existing config contents select the actual auth paths; fixed-path creation
  therefore does not establish the claimed postcondition.
- Lines 162-176 replace public service assets and reload systemd last. Why:
  systemd should only observe assets after prerequisites are present; external
  command failure is propagated rather than silently accepted.

**Invariants:** no mutation before host preflight; auth material is never
world-accessible when newly created; existing auth files are never overwritten;
daemon startup remains fail-closed; unit reload happens only after file writes.

**Dependencies and risks:** Called only by `installCmd`; calls account,
directory, config, auth-file, atomic-write, and binary-resolution helpers;
shares fixed paths with the embedded scaffold and CLI output. External risks
are account-database behavior, filesystem mutation under root, and systemd
reload behavior. The principal invariant coupling is between config-selected
paths and the files later consumed by `auth.LoadUsers`/`auth.LoadPepper`.

### `ensurePepper` and `ensureUsers` (`internal/systemd/systemd.go:213`, `:242`)

**Purpose:** These helpers create the two authentication inputs only when
absent, with restrictive modes and requested ownership. One creates random
HMAC keying material; the other creates a valid fail-closed empty key store.

**Inputs & assumptions:** Each receives a trusted target path and numeric
UID/GID; its parent directory already exists and is protected; `crypto/rand`
is available; `writeFileAtomic` preserves the requested mode; `os.Chown`
succeeds under root; a pre-existing regular file is intentionally authoritative.

**Outputs & effects:** Return whether a file was created; call `Lstat`; write
and rename a temporary file when absent; change final ownership; never emit
secret contents; reject symlinks and other non-regular targets.

**Block analysis:**

- The initial `Lstat` branch preserves regular files and rejects other file
  types. Why: credentials must not be silently rotated and symlinks must not
  redirect privileged writes. Assumption: only trusted root can mutate the
  protected parent during the subsequent operation.
- Pepper generation obtains 64 random bytes and stores one base64 line. Why:
  this matches the established operator format and exceeds the loader's
  minimum. First principle: unpredictable key material must originate from the
  OS CSPRNG and must never be derived from public installation state.
- Atomic writing precedes final `Chown`. Why: readers see complete content and
  never a partial secret/store; a chown failure is returned fail-closed.
- The users stub contains version 1 and an empty key list. Why: it parses while
  authenticating nobody, allowing subsequent locked key updates without
  weakening the daemon's default denial.

**Invariants:** generated pepper entropy is 512 bits before encoding; new files
have mode 0640; existing regular files are byte-preserved; non-regular targets
are refused; empty users state authenticates no client.

**Dependencies and risks:** Both are called only by `Provision.Run` and depend
on `writeFileAtomic`; the outputs are consumed by `auth.LoadPepper`,
`auth.LoadUsers`, and later `auth.Update`. Risks considered are weak randomness,
symlink replacement, permissive modes, partial writes, accidental rotation,
and ownership loss during key-store updates. Tests cover generation, parsing,
mode, non-rewrite, and symlink refusal; `auth.Update` preserves/clamps existing
ownership and mode.

## Test and Tool Results

- `go test ./...`: pass
- `go vet ./...`: pass
- `gofmt -l .`: pass (no output)
- `govulncheck ./...`: pass (`No vulnerabilities found.`)
- `staticcheck ./...`: only the documented Darwin-only SA4023 at
  `cmd/mellomting/serve.go:308`
- `go test -race ./...`: one failure in unchanged
  `TestNonStreamWriteDeadlineResets/slow_but_steady_completes`; the isolated
  test then passed three consecutive race runs. Treat this as a flaky quality
  gate, not evidence of a regression in this diff.

## Historical and Blast-Radius Analysis

The installer originated in `1e62102`; subsequent commits hardened admission
and installation behavior. No removed validation in this range traces to a
security-fix commit. The new secret helpers each have one production caller,
and the `Report` reaches only operator output. The config/error-text changes do
not alter validation branches or accepted values.

## Recommendations

### Before merge

- Resolve the fixed-path versus preserved-config mismatch and add the custom
  auth-path integration test.

### Follow-up

- Stabilize `TestNonStreamWriteDeadlineResets` under `-race` so the required
  quality gate is deterministic.

## Methodology and Limitations

**Strategy:** Focused differential review of a medium Go codebase (89 Go files),
with deep analysis of the privileged/authentication changes and surface review
of low-risk text/scaffold changes.

Techniques included baseline/current comparison, commit history inspection,
caller search, trust-boundary and invariant reconstruction, adversarial misuse
analysis, test-gap analysis, and all locally available quality gates. The
review did not execute `Provision.Run` on a real Linux systemd host as root;
host mutations were assessed from implementation and unit tests. Confidence is
high for the reviewed diff and medium for host-specific integration behavior.
