package main

import (
	"errors"
	"fmt"
	"io"
	"mellomting/internal/securefile"
	"os"
	"path/filepath"
	"strings"

	"mellomting/internal/systemd"
	"mellomting/internal/version"
)

// defaultInstallPrefix is the conventional FHS prefix for locally
// installed software; the binary is installed at <prefix>/bin/mellomting
// (matching deploy/mellomting.service, which execs
// /usr/local/bin/mellomting).
const defaultInstallPrefix = "/usr/local"

// errAlreadyInstalled is returned by installBinary when source and
// destination are the same resolved path (a re-install on top of itself
// is a no-op, not an error).
var errAlreadyInstalled = errors.New("already installed")

// installCmd runs `mellomting install [--prefix DIR] [--systemd]`: it
// copies the running binary into <prefix>/bin/ (default prefix:
// /usr/local), so a built or downloaded binary can be installed without a
// package manager. With --systemd it additionally provisions the daemon
// as a systemd service (Linux, root): service user, config/log/state/run
// dirs, the hardened unit and logrotate, and a daemon-reload.
//
// Exit codes: 0 ok (including the already-installed no-op), 1 install
// failure, 2 usage error.
func installCmd(args []string) int {
	flags := commandFlags("install", "Install this binary. With --systemd, also prepare the service and configuration.\nExisting configuration and auth files are preserved; the service is not started.")
	prefix := flags.String("prefix", defaultInstallPrefix, "install the binary in `DIR`/bin")
	systemdInstall := flags.Bool("systemd", false, "install as a systemd service (Linux, root)")
	if err := parseCommandFlags(flags, args); err != nil {
		return flagExitCode(err)
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "mellomting: install: unexpected arguments %q\n", flags.Args())
		return 2
	}
	if *prefix == "" {
		fmt.Fprintln(os.Stderr, "mellomting: install: --prefix must not be empty")
		return 2
	}
	src, err := currentExecutable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: install: %v\n", err)
		return 1
	}

	// Preflight the systemd half before writing anything, so a run that
	// cannot provision (e.g. a non-Linux host) fails closed without
	// leaving a freshly installed binary behind.
	if *systemdInstall {
		p := &systemd.Provision{}
		if err := p.CheckHost(); err != nil {
			fmt.Fprintf(os.Stderr, "mellomting: install --systemd: %v\n", err)
			return 1
		}
	}

	dest, code := performInstall(src, *prefix)
	if code != 0 {
		return code
	}
	if *systemdInstall {
		p := &systemd.Provision{BinaryPath: dest}
		report, err := p.Run()
		if err != nil {
			fmt.Fprintf(os.Stderr, "mellomting: install --systemd: %v\n", err)
			return 1
		}
		printSystemdNextSteps(dest, report)
	}
	return 0
}

// performInstall installs the binary at src into <prefix>/bin/ and
// reports the result on stdout/stderr. On success it returns the resolved
// destination path so the caller can pass it to systemd provisioning.
func performInstall(src, prefix string) (dest string, code int) {
	dest = filepath.Join(prefix, "bin", version.Name)
	if err := installBinary(src, dest); err != nil {
		if errors.Is(err, errAlreadyInstalled) {
			fmt.Fprintf(os.Stdout, "already installed at %s\n", dest)
			return dest, 0
		}
		fmt.Fprintf(os.Stderr, "mellomting: install: %v\n", err)
		if os.IsPermission(err) {
			fmt.Fprintln(os.Stderr, "mellomting: the destination is not writable by this user; run with write access (e.g. root) or choose another --prefix")
		}
		return dest, 1
	}
	fmt.Fprintf(os.Stdout, "installed %s at %s\n", version.Version, dest)
	return dest, 0
}

// systemdNextSteps renders the concise post-install text (D16): one
// completion line and the three remaining operator actions in execution
// order. It deliberately omits the artifact/permission inventory — those
// details live in documentation, not the ordinary success path.
func systemdNextSteps(binaryPath string, r systemd.Report) string {
	var b strings.Builder
	b.WriteString("Mellomting installed.\n\n")
	b.WriteString("Next:\n")
	fmt.Fprintf(&b, "  1. Run: sudo editor %s\n", systemd.ConfigPath)
	fmt.Fprintf(&b, "  2. Run: sudo %s key create --name production\n", binaryPath)
	fmt.Fprintf(&b, "  3. Run: sudo systemctl enable --now %s\n", systemd.UnitName)
	return b.String()
}

// printSystemdNextSteps prints the post-install text (systemdNextSteps):
// what provisioning did and the operator's remaining steps.
func printSystemdNextSteps(binaryPath string, r systemd.Report) {
	fmt.Fprint(os.Stdout, systemdNextSteps(binaryPath, r))
}

// installBinary copies src to dest atomically: the copy is written to a
// temporary file in the destination directory, fsynced, and renamed over
// dest, so concurrent readers never observe a partial binary and the
// replace is a single syscall. The installed file is always mode 0755,
// independent of umask and of the source file's mode. installBinary
// refuses to replace a symlink (the same stance as safeUnixListen, PLAN
// §8.3) and returns errAlreadyInstalled when src and dest address the
// same file, including through symlinked path components (macOS's /var
// is a symlink to /private/var).
func installBinary(src, dest string) error {
	if sameResolvedPath(src, dest) {
		return errAlreadyInstalled
	}
	if st, err := os.Lstat(dest); err == nil && st.IsDir() {
		return fmt.Errorf("refusing to replace %q: it is a directory", dest)
	}
	destDir := filepath.Dir(dest)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create %q: %w", destDir, err)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %q: %w", src, err)
	}
	defer in.Close()
	srcInfo, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat %q: %w", src, err)
	}
	// securefile.Replace refuses a symlink or non-regular destination,
	// fsyncs the copy and the parent directory, and sets the mode through
	// the descriptor rather than by path.
	return securefile.Replace(dest, 0o755, func(w io.Writer) error {
		n, err := io.Copy(w, in)
		if err != nil {
			return fmt.Errorf("copy %q to %q: %w", src, destDir, err)
		}
		if n != srcInfo.Size() {
			return fmt.Errorf("verify %q: copied %d bytes, source is %d", dest, n, srcInfo.Size())
		}
		return nil
	})
}

// sameResolvedPath reports whether a and b address the same file after
// resolving symlinked path components (macOS's /var is /private/var, so
// a literal-path comparison would miss a no-op re-install). b may not
// exist yet; in that case the install is not a no-op.
func sameResolvedPath(a, b string) bool {
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// currentExecutable resolves the path of the running binary.
// os.Executable already resolves symlinks on supported platforms
// (/proc/self/exe on Linux, proc_pidpath on Darwin); EvalSymlinks is a
// defensive re-resolve so the no-op re-install check below is exact.
func currentExecutable() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	if p, err = filepath.EvalSymlinks(p); err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	return p, nil
}
