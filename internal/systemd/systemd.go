package systemd

import (
	"bytes"
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
	"syscall"

	"gopkg.in/yaml.v3"

	"mellomting/internal/auth"
	"mellomting/internal/securefile"
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
const usersStub = auth.EmptyUsers

// dirSpec describes an operational directory the daemon needs but does not
// create; provisioning creates it with a strict mode and owner.
type dirSpec struct {
	path string
	mode os.FileMode
	uid  int
	gid  int
}

// Provision carries the parameters for `mellomting install --systemd`.
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
	gid, err := serviceGroupID(p.User, u)
	if err != nil {
		return Report{}, err
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

// ensureFile creates path only when it does not already exist, with the
// create-only contract the installer relies on: the content of an
// existing regular file is never rewritten, and anything that is not a
// regular file — a symlink above all — is refused rather than written
// through. content is called only when the file will actually be
// created, so ensurePepper does not draw entropy for a file that already
// exists. Ownership and mode are re-asserted either way, so a
// provisioning run always leaves the service account able to read what
// the unit will hand it.
func ensureFile(path string, mode os.FileMode, uid, gid int, content func() (string, error)) (bool, error) {
	st, err := os.Lstat(path)
	if err == nil {
		if !st.Mode().IsRegular() {
			return false, fmt.Errorf("refusing to use %q: it is not a regular file", path)
		}
		// The content is the operator's, but the ownership is the
		// installer's business: a file written earlier by `init` (or by
		// an older installer) is root:root 0600, which the service
		// account cannot read, and the unit then fails to start with a
		// permission error that names no cause. Re-assert owner and mode
		// exactly as ensureDir does for the directories.
		if err := reassertOwnership(path, st, mode, uid, gid); err != nil {
			return false, err
		}
		return false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("stat %q: %w", path, err)
	}
	data, err := content()
	if err != nil {
		return false, err
	}
	if err := writeFileAtomic(data, path, mode); err != nil {
		return false, err
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return false, fmt.Errorf("chown %q: %w", path, err)
	}
	return true, nil
}

// reassertOwnership brings an existing managed artifact to the owner and
// mode the unit needs, without touching its content — the same stance
// ensureDir already takes for the directories, and for the same reason:
// the installer runs as root by design and is provisioning this host, so
// an artifact left by an earlier `init` must not make the service
// unstartable. Only the three fixed default paths reach here; a config
// naming its own auth files elsewhere is the operator's to own.
//
// The chown and chmod go through an opened descriptor, never the path,
// so they cannot land on a file swapped in underneath.
func reassertOwnership(path string, st os.FileInfo, mode os.FileMode, uid, gid int) error {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if ok && int(sys.Uid) == uid && int(sys.Gid) == gid && st.Mode().Perm() == mode {
		return nil
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()
	if err := f.Chown(uid, gid); err != nil {
		return fmt.Errorf("chown %q: %w", path, err)
	}
	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("chmod %q: %w", path, err)
	}
	return nil
}

func ensureConfig(path string, mode os.FileMode, uid, gid int) (bool, error) {
	return ensureFile(path, mode, uid, gid, func() (string, error) {
		return string(ConfigTemplate()), nil
	})
}

func ensurePepper(path string, uid, gid int) (bool, error) {
	return ensureFile(path, 0o640, uid, gid, func() (string, error) {
		b, err := auth.GeneratePepper(nil)
		if err != nil {
			return "", fmt.Errorf("generate pepper for %q: %w", path, err)
		}
		return auth.EncodePepper(b), nil
	})
}

func ensureUsers(path string, uid, gid int) (bool, error) {
	return ensureFile(path, 0o640, uid, gid, func() (string, error) {
		return usersStub, nil
	})
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
	return "", fmt.Errorf("%s not found in %s", bin, strings.Join(locations, " "))
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
		return fmt.Errorf("install --systemd requires a Linux host (got %q)", env.goos)
	}
	if env.euid != 0 {
		return errors.New("install --systemd must run as root (use sudo)")
	}
	if !env.systemdActive {
		return errors.New("systemd is not running (missing /run/systemd/system)")
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

// serviceGroupID resolves the group the unit will actually run as. The
// unit names it (Group=), so that is the group the files it reads must
// belong to — not whatever primary group the account happens to carry.
// The two agree for an account this installer created with
// --user-group, and diverge for one that already existed with a
// different primary group (a package manager's, or `useradd` without
// --user-group), where owning the files to the account's primary group
// leaves every one of them unreadable by the running service.
//
// An account whose primary group IS the named one still resolves
// through the account, so a host whose group database only answers for
// the user is not made worse off.
func serviceGroupID(name string, u *user.User) (int, error) {
	if g, err := user.LookupGroup(name); err == nil {
		gid, err := strconv.Atoi(g.Gid)
		if err != nil {
			return 0, fmt.Errorf("service group %q gid %q is not numeric: %w", name, g.Gid, err)
		}
		return gid, nil
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, fmt.Errorf("service user gid %q is not numeric: %w", u.Gid, err)
	}
	if g, err := user.LookupGroupId(u.Gid); err != nil || g.Name != name {
		return 0, fmt.Errorf("service group %q is missing; run groupadd --system %s and add user %s to it", name, name, name)
	}
	return gid, nil
}

// ensureDir creates an operational directory with a strict mode and owner,
// or adopts an existing one, re-asserting the mode and re-owning it to the
// service account. An existing directory of foreign ownership is adopted
// rather than refused: the installer runs as root by design and is
// re-provisioning the same host, so a directory it created (or an operator
// chown-ed) on an earlier run must not make re-running install
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
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %q: %w", dir, err)
	}
	return securefile.Replace(path, mode, func(w io.Writer) error {
		if _, err := io.WriteString(w, content); err != nil {
			return fmt.Errorf("write %q: %w", path, err)
		}
		return nil
	})
}
