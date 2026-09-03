package systemd

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Host paths the daemon and its provisioning use (PLAN §26-27, §64, §76).
// These are Linux systemd paths; provisioning refuses to run on any other
// host (see Provision.preflight).
const (
	ConfigDir     = "/etc/mellomting"
	ConfigPath    = "/etc/mellomting/config.yaml"
	UsersPath     = "/etc/mellomting/users.yaml"
	PepperPath    = "/etc/mellomting/auth.pepper"
	LogDir        = "/var/log/mellomting"
	StateDir      = "/var/lib/mellomting"
	RunDir        = "/run/mellomting"
	UnitPath      = "/etc/systemd/system/mellomting.service"
	LogrotatePath = "/etc/logrotate.d/mellomting"
)

// UnitName is the systemd unit basename (mellomting.service without the
// suffix), i.e. the name passed to `systemctl enable --now`. It is derived
// from UnitPath so it cannot diverge from the installed unit. It is
// distinct from DefaultServiceUser (the account the daemon runs under) —
// the two happen to share the string "mellomting" today but must not be
// conflated.
const UnitName = "mellomting"

// pepperBytes is the HMAC pepper length the installer generates: 64
// crypto-random bytes, base64-encoded on one line (the in-process
// equivalent of the `base64(head -c 64 /dev/urandom)` format). Any pepper
// of at least 16 bytes loads (auth.LoadPepper); a pre-existing file of any
// format is left in place and loads unchanged.
const pepperBytes = 64

// configMaxBytes bounds the size of an existing config read by the D15
// auth-path source parse (referencedAuthPaths). It mirrors the config
// loader's own size cap so a hostile or accidental huge file cannot make
// provisioning allocate unboundedly.
const configMaxBytes = 1 << 20

// usersStub is the empty key store the installer writes when no users file
// exists. It is the minimal valid on-disk record (version 1, no keys) — an
// empty store is explicitly valid (T-M10) and the daemon fails closed (401
// for every request) until an operator creates a key — with a note that
// the key subcommands own the file. A test asserts the stub round-trips
// auth.LoadUsers.
const usersStub = `# Mellomting API keys. Managed by ` + "`" + `mellomting key
# create|list|enable|disable|revoke` + "`" + ` — do not edit by hand.
version: 1
keys: []
`

// dirSpec describes an operational directory the daemon needs but does not
// create; provisioning creates it with a strict mode and owner.
type dirSpec struct {
	path string
	mode os.FileMode
	uid  int
	gid  int
}

// Provision carries the parameters for `mellomting --install --systemd`.
// It is fail-closed: any precondition that does not hold aborts before the
// host is mutated.
type Provision struct {
	// BinaryPath is the resolved absolute path of the installed binary; it
	// is substituted into the unit's ExecStart.
	BinaryPath string
	// User is the service account to ensure. Defaults to DefaultServiceUser.
	User string
}

// Report records which provisioning artifacts Run created so the
// installer can say what was set up and what pre-existed and was left in
// place.
type Report struct {
	ConfigCreated bool
	PepperCreated bool
	UsersCreated  bool
}

// Run performs the systemd provisioning:
//  1. fail-closed preflight (Linux, root, systemd active, valid binary);
//  2. ensure the unprivileged service account exists;
//  3. create the operational directories with strict owner/mode;
//  4. write the commented scaffold into the config directory when no
//     config file exists (never touching one that does);
//  5. create auth artifacts per D15: a freshly written scaffold gets the
//     fixed default pepper and empty users file; a pre-existing config is
//     operator-managed and only a missing artifact it explicitly names at
//     its exact default path is created;
//  6. install the unit and logrotate atomically;
//  7. systemctl daemon-reload so the unit is picked up.
//
// Every step is create-only-if-absent. It reports what was created so the
// installer can say what was set up and what was already present.
func (p *Provision) Run() (r Report, err error) {
	if err := p.preflight(preflightEnv{
		goos:          runtime.GOOS,
		euid:          os.Geteuid(),
		systemdActive: systemdActive(),
	}); err != nil {
		return Report{}, err
	}
	if p.User == "" {
		p.User = DefaultServiceUser
	}
	u, err := ensureAccount(p.User)
	if err != nil {
		return Report{}, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return Report{}, fmt.Errorf("service user uid %q is not numeric: %w", u.Uid, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return Report{}, fmt.Errorf("service user gid %q is not numeric: %w", u.Gid, err)
	}

	dirs := []dirSpec{
		{path: ConfigDir, mode: 0o750, uid: 0, gid: gid}, // root:mellomting, group-readable (pepper 0640, HARDENING)
		{path: LogDir, mode: 0o750, uid: uid, gid: gid},
		{path: StateDir, mode: 0o750, uid: uid, gid: gid},
		{path: RunDir, mode: 0o750, uid: uid, gid: gid},
	}
	for _, d := range dirs {
		if err := ensureDir(d.path, d.mode, d.uid, d.gid); err != nil {
			return Report{}, err
		}
	}

	// Config file before the secrets and the unit: once the unit is
	// enabled a half-configured config is the first thing the daemon will
	// read, so the scaffold (invalid until filled in) makes the remaining
	// work explicit instead of leaving an empty config directory.
	r.ConfigCreated, err = ensureConfig(ConfigPath, 0o640, 0, gid)
	if err != nil {
		return Report{}, err
	}
	// Auth artifacts follow D15: a freshly written scaffold gets the
	// fixed default pepper and empty users file; a pre-existing config is
	// operator-managed and we create a missing fixed default artifact
	// only when that config explicitly names its exact default path.
	r.PepperCreated, r.UsersCreated, err = ensureAuthArtifacts(r.ConfigCreated, ConfigPath, UsersPath, PepperPath, 0, gid)
	if err != nil {
		return Report{}, err
	}

	if err := writeFileAtomic(RenderService(p.BinaryPath), UnitPath, 0o644); err != nil {
		return Report{}, err
	}
	if err := writeFileAtomic(string(Logrotate()), LogrotatePath, 0o644); err != nil {
		return Report{}, err
	}

	systemctlPath, err := resolveBinary("systemctl", "/usr/bin/systemctl", "/bin/systemctl")
	if err != nil {
		return Report{}, err
	}
	if err := exec.Command(systemctlPath, "daemon-reload").Run(); err != nil {
		return Report{}, fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	return r, nil
}

// ensureConfig guarantees the config file the daemon will read (path):
// when no file is present it writes the scaffold — deliberately invalid
// until the operator fills in backends and models — with the given mode
// and owner, so the service user can read it. An existing config is the
// operator's work (secrets, limits, backend choices) and is never
// rewritten; a non-regular file at the path (a symlink among them) is
// refused, in line with the project's symlink stance. It reports whether
// it created the file.
func ensureConfig(path string, mode os.FileMode, uid, gid int) (bool, error) {
	st, err := os.Lstat(path)
	if err == nil {
		if !st.Mode().IsRegular() {
			return false, fmt.Errorf("refusing to use %q: it is not a regular file", path)
		}
		return false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("stat %q: %w", path, err)
	}
	if err := writeFileAtomic(string(ConfigTemplate()), path, mode); err != nil {
		return false, err
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return false, fmt.Errorf("chown %q: %w", path, err)
	}
	return true, nil
}

// ensurePepper generates the HMAC pepper (PLAN §27) when no file is at
// path: pepperBytes crypto-random bytes, standard base64, one line. A
// pre-existing file is the operator's secret and is never rewritten (it
// loads in whatever format it already has); a non-regular file (a symlink
// among them) is refused, in line with the project's symlink stance. It
// reports whether it created the file.
func ensurePepper(path string, uid, gid int) (bool, error) {
	st, err := os.Lstat(path)
	if err == nil {
		if !st.Mode().IsRegular() {
			return false, fmt.Errorf("refusing to use %q: it is not a regular file", path)
		}
		return false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("stat %q: %w", path, err)
	}
	b := make([]byte, pepperBytes)
	if _, err := rand.Read(b); err != nil {
		return false, fmt.Errorf("generate pepper for %q: %w", path, err)
	}
	// One base64 line + newline: LoadPepper trims the trailing newline.
	if err := writeFileAtomic(base64.StdEncoding.EncodeToString(b)+"\n", path, 0o640); err != nil {
		return false, err
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return false, fmt.Errorf("chown %q: %w", path, err)
	}
	return true, nil
}

// ensureUsers writes the empty key-store stub (usersStub) when no users
// file is at path, with the same create-only-if-absent and symlink-refusal
// shape as ensurePepper. A pre-existing users file holds the operator's
// keys and is never rewritten. It reports whether it created the file.
func ensureUsers(path string, uid, gid int) (bool, error) {
	st, err := os.Lstat(path)
	if err == nil {
		if !st.Mode().IsRegular() {
			return false, fmt.Errorf("refusing to use %q: it is not a regular file", path)
		}
		return false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("stat %q: %w", path, err)
	}
	if err := writeFileAtomic(usersStub, path, 0o640); err != nil {
		return false, err
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return false, fmt.Errorf("chown %q: %w", path, err)
	}
	return true, nil
}

// ensureAuthArtifacts applies the D15 auth-artifact contract. A freshly
// written scaffold (configCreated) gets the fixed default pepper and empty
// users file, both 0640 root:mellomting, so `key create` works out of the
// box. A pre-existing config is operator-managed: we create a missing
// fixed default artifact only when that config explicitly names its exact
// default path; a parse failure creates no auth files. It reports which of
// the two artifacts it created. Paths are parameters so the contract is
// testable without touching the real /etc/mellomting.
func ensureAuthArtifacts(configCreated bool, configPath, usersPath, pepperPath string, uid, gid int) (pepperCreated, usersCreated bool, err error) {
	if configCreated {
		pepperCreated, err = ensurePepper(pepperPath, uid, gid)
		if err != nil {
			return false, false, err
		}
		usersCreated, err = ensureUsers(usersPath, uid, gid)
		if err != nil {
			return false, false, err
		}
		return pepperCreated, usersCreated, nil
	}

	usersRef, pepperRef, err := referencedAuthPaths(configPath)
	if err != nil {
		// D15: a parse failure creates no auth files.
		return false, false, nil
	}
	if usersRef == usersPath {
		usersCreated, err = ensureUsers(usersPath, uid, gid)
		if err != nil {
			return false, false, err
		}
	}
	if pepperRef == pepperPath {
		pepperCreated, err = ensurePepper(pepperPath, uid, gid)
		if err != nil {
			return false, false, err
		}
	}
	return pepperCreated, usersCreated, nil
}

// referencedAuthPaths determines the auth files an existing config names
// with a bounded, no-side-effect YAML source parse that does not require
// the config to pass full validation (D15). The deliberately incomplete
// scaffold and partially edited configs must not prevent the installer from
// recognizing the fixed default paths they name. On any read or decode
// failure it returns an error; the caller creates no auth files.
func referencedAuthPaths(path string) (usersFile, pepperFile string, err error) {
	st, err := os.Lstat(path)
	if err != nil {
		return "", "", err
	}
	if !st.Mode().IsRegular() {
		return "", "", fmt.Errorf("%q is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, configMaxBytes+1))
	if err != nil {
		return "", "", err
	}
	if len(data) > configMaxBytes {
		return "", "", fmt.Errorf("%q exceeds maximum size of %d bytes", path, configMaxBytes)
	}
	var doc struct {
		Auth struct {
			UsersFile  string `yaml:"users_file"`
			PepperFile string `yaml:"pepper_file"`
		} `yaml:"auth"`
	}
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&doc); err != nil {
		return "", "", err
	}
	return doc.Auth.UsersFile, doc.Auth.PepperFile, nil
}

// resolveBinary returns the first existing regular file among a fixed
// list of well-known locations for the named system binary (useradd,
// systemctl). It deliberately resolves nothing through $PATH: the
// commands it produces run as root, and a caller-supplied PATH could put
// a forged binary first. When no location yields a regular file it
// returns an error naming the binary, never a partial fallback.
func resolveBinary(bin string, locations ...string) (string, error) {
	for _, loc := range locations {
		if st, err := os.Stat(loc); err == nil && st.Mode().IsRegular() {
			return loc, nil
		}
	}
	return "", fmt.Errorf("%s not found in %s; refusing to resolve it through $PATH", bin, strings.Join(locations, " "))
}

// preflightEnv carries the environment-derived inputs to preflight so the
// check is deterministic and testable on any host.
type preflightEnv struct {
	goos          string
	euid          int
	systemdActive bool
}

// CheckHost verifies the host-level preconditions (Linux, root, systemd
// active) without mutating the host and without requiring the binary to
// exist yet. installCmd runs it before writing the binary so a doomed run
// (e.g. on a non-Linux host) fails closed before any file is written.
func (p *Provision) CheckHost() error {
	return p.checkHost(preflightEnv{
		goos:          runtime.GOOS,
		euid:          os.Geteuid(),
		systemdActive: systemdActive(),
	})
}

// preflight verifies every precondition that must hold before provisioning
// mutates the host. It fails closed: nothing runs on a partial match.
func (p *Provision) preflight(env preflightEnv) error {
	if err := p.checkHost(env); err != nil {
		return err
	}
	// BinaryPath is embedded verbatim in the unit's ExecStart; a control
	// character would inject arbitrary directives into the generated unit.
	if strings.ContainsFunc(p.BinaryPath, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	}) {
		return fmt.Errorf("binary path %q must not contain control characters", p.BinaryPath)
	}
	if !filepath.IsAbs(p.BinaryPath) {
		return fmt.Errorf("binary path %q is not absolute", p.BinaryPath)
	}
	if st, err := os.Stat(p.BinaryPath); err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("binary %q not found or not a regular file", p.BinaryPath)
	}
	return nil
}

// checkHost validates the host-level preconditions only.
func (p *Provision) checkHost(env preflightEnv) error {
	if env.goos != "linux" {
		return fmt.Errorf("--install --systemd requires a Linux host (got %q)", env.goos)
	}
	if env.euid != 0 {
		return errors.New("--install --systemd must run as root (use sudo)")
	}
	if !env.systemdActive {
		return errors.New("systemd is not the active init (no /run/systemd/system); refusing to provision")
	}
	return nil
}

// systemdActive reports whether systemd is the running init.
func systemdActive() bool {
	st, err := os.Stat("/run/systemd/system")
	return err == nil && st.IsDir()
}

// ensureAccount ensures the unprivileged service account (and same-named
// group) exists, creating a system account when missing. It fails closed:
// a failed creation or a missing post-creation entry is an error, never a
// silent fallback.
func ensureAccount(name string) (*user.User, error) {
	if u, err := user.Lookup(name); err == nil {
		return u, nil
	}
	args := []string{
		"--system",
		"--no-create-home",
		"--user-group",
		"--home-dir", RunDir,
		"--shell", "/usr/sbin/nologin",
		name,
	}
	useraddPath, err := resolveBinary("useradd", "/usr/sbin/useradd", "/sbin/useradd")
	if err != nil {
		return nil, err
	}
	if out, err := exec.Command(useraddPath, args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("create system user %q: %w: %s", name, err, out)
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("look up created user %q: %w", name, err)
	}
	return u, nil
}

// ensureDir creates an operational directory with a strict mode and owner,
// or adopts an existing one, re-asserting the mode and re-owning it to the
// service account. An existing directory of foreign ownership is adopted
// rather than refused: the installer runs as root by design and is
// re-provisioning the same host, so a directory it created (or an operator
// chown-ed) on an earlier run must not make re-running --install
// --systemd fail. It refuses to use an existing path that is not a real
// directory; a symlink (even one resolving to a directory) is treated as
// hostile, in line with the project's symlink stance.
func ensureDir(path string, mode os.FileMode, uid, gid int) error {
	st, err := os.Lstat(path)
	switch {
	case err == nil:
		if !st.IsDir() {
			return fmt.Errorf("refusing to use %q: it is not a directory", path)
		}
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(path, mode); err != nil {
			return fmt.Errorf("create %q: %w", path, err)
		}
	default:
		return fmt.Errorf("stat %q: %w", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("chmod %q: %w", path, err)
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("chown %q: %w", path, err)
	}
	return nil
}

// writeFileAtomic writes content to path atomically (a temp file in the
// destination directory, fsynced, then renamed over path) with a fixed
// mode. It refuses to replace an existing symlink or non-regular file,
// and never leaves a partial destination on failure.
func writeFileAtomic(content, path string, mode os.FileMode) error {
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to replace %q: it is a symlink", path)
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("refusing to replace %q: it is not a regular file", path)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat %q: %w", path, err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".mellomting-"+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	abort := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.WriteString(content); err != nil {
		abort()
		return fmt.Errorf("write %q: %w", tmpName, err)
	}
	// Fsync the data before the rename: rename(2) orders the directory
	// entry, not the file's blocks, so an unsynced write can leave a
	// truncated file at the destination after a crash.
	if err := tmp.Sync(); err != nil {
		abort()
		return fmt.Errorf("sync %q: %w", tmpName, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		abort()
		return fmt.Errorf("chmod %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace %q: %w", path, err)
	}
	return nil
}
