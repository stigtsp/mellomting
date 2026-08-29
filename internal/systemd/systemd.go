package systemd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
)

// Host paths the daemon and its provisioning use (PLAN §26-27, §64, §76).
// These are Linux systemd paths; provisioning refuses to run on any other
// host (see Provision.preflight).
const (
	ConfigDir     = "/etc/mellomting"
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

// Run performs the systemd provisioning:
//  1. fail-closed preflight (Linux, root, systemd active, valid binary);
//  2. ensure the unprivileged service account exists;
//  3. create the operational directories with strict owner/mode;
//  4. install the unit and logrotate atomically;
//  5. systemctl daemon-reload so the unit is picked up.
func (p *Provision) Run() error {
	if err := p.preflight(preflightEnv{
		goos:          runtime.GOOS,
		euid:          os.Geteuid(),
		systemdActive: systemdActive(),
	}); err != nil {
		return err
	}
	if p.User == "" {
		p.User = DefaultServiceUser
	}
	u, err := ensureAccount(p.User)
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("service user uid %q is not numeric: %w", u.Uid, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return fmt.Errorf("service user gid %q is not numeric: %w", u.Gid, err)
	}

	dirs := []dirSpec{
		{path: ConfigDir, mode: 0o750, uid: 0, gid: gid}, // root:mellomting, group-readable (pepper 0640, HARDENING)
		{path: LogDir, mode: 0o750, uid: uid, gid: gid},
		{path: StateDir, mode: 0o750, uid: uid, gid: gid},
		{path: RunDir, mode: 0o750, uid: uid, gid: gid},
	}
	for _, d := range dirs {
		if err := ensureDir(d.path, d.mode, d.uid, d.gid); err != nil {
			return err
		}
	}

	if err := writeFileAtomic(RenderService(p.BinaryPath), UnitPath, 0o644); err != nil {
		return err
	}
	if err := writeFileAtomic(string(Logrotate()), LogrotatePath, 0o644); err != nil {
		return err
	}

	if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	return nil
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
		"--home-dir", "/run/mellomting",
		"--shell", "/usr/sbin/nologin",
		name,
	}
	if out, err := exec.Command("useradd", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("create system user %q: %w: %s", name, err, out)
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("look up created user %q: %w", name, err)
	}
	return u, nil
}

// ensureDir creates an operational directory with a strict mode and owner.
// It refuses to use an existing path that is not a real directory; a
// symlink (even one resolving to a directory) is treated as hostile, in
// line with the project's symlink stance.
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
// destination directory followed by rename) with a fixed mode. It refuses
// to replace an existing symlink or non-regular file, and never leaves a
// partial destination on failure.
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
