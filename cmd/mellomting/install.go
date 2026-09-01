package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

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

// installCmd runs `mellomting --install [--prefix DIR] [--systemd]`: it
// copies the running binary into <prefix>/bin/ (default prefix:
// /usr/local), so a built or downloaded binary can be installed without a
// package manager. With --systemd it additionally provisions the daemon
// as a systemd service (Linux, root): service user, config/log/state/run
// dirs, the hardened unit and logrotate, and a daemon-reload.
//
// Exit codes: 0 ok (including the already-installed no-op), 1 install
// failure, 2 usage error.
func installCmd(args []string) int {
	flags := flag.NewFlagSet("mellomting --install", flag.ContinueOnError)
	prefix := flags.String("prefix", defaultInstallPrefix, "destination prefix: the binary is installed at <prefix>/bin/"+version.Name)
	systemdInstall := flags.Bool("systemd", false, "also provision as a systemd service (Linux root): service user, config/log/state/run dirs, scaffold config.yaml if absent, unit, logrotate")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "mellomting: --install: unexpected arguments %q\n", flags.Args())
		return 2
	}
	if *prefix == "" {
		fmt.Fprintln(os.Stderr, "mellomting: --install: --prefix must not be empty")
		return 2
	}
	src, err := currentExecutable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: --install: %v\n", err)
		return 1
	}

	// Preflight the systemd half before writing anything, so a run that
	// cannot provision (e.g. a non-Linux host) fails closed without
	// leaving a freshly installed binary behind.
	if *systemdInstall {
		p := &systemd.Provision{}
		if err := p.CheckHost(); err != nil {
			fmt.Fprintf(os.Stderr, "mellomting: --install --systemd: %v\n", err)
			return 1
		}
	}

	dest, code := performInstall(src, *prefix)
	if code != 0 {
		return code
	}
	if *systemdInstall {
		p := &systemd.Provision{BinaryPath: dest}
		configCreated, err := p.Run()
		if err != nil {
			fmt.Fprintf(os.Stderr, "mellomting: --install --systemd: %v\n", err)
			return 1
		}
		printSystemdNextSteps(dest, configCreated)
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
			fmt.Fprintf(os.Stdout, "mellomting: already installed at %s\n", dest)
			return dest, 0
		}
		fmt.Fprintf(os.Stderr, "mellomting: --install: %v\n", err)
		if os.IsPermission(err) {
			fmt.Fprintln(os.Stderr, "mellomting: the destination is not writable by this user; run with write access (e.g. root) or choose another --prefix")
		}
		return dest, 1
	}
	fmt.Fprintf(os.Stdout, "mellomting: installed %s at %s\n", version.Version, dest)
	printInstallNote()
	return dest, 0
}

// printInstallNote reminds the operator that the daemon creates neither
// its configuration directory nor its log/runtime directories, so the
// destination host must provide them. systemd's deploy/mellomting.service
// auto-creates them (RuntimeDirectory/StateDirectory/LogsDirectory); the
// install command's --systemd flag provisions them and the service on
// Linux, and writes a commented scaffold into the config directory when
// no config file exists. Plain --install creates nothing beyond the
// binary and ships no secrets (they are operator-authored).
func printInstallNote() {
	fmt.Fprintln(os.Stdout, "mellomting: note: the daemon creates neither its config nor its log/runtime directories")
	fmt.Fprintln(os.Stdout, "             config /etc/mellomting/ (config.yaml, users.yaml, auth.pepper),")
	fmt.Fprintln(os.Stdout, "             log /var/log/mellomting/, socket /run/mellomting/.")
	fmt.Fprintln(os.Stdout, "             deploy/mellomting.service (systemd) creates them automatically;")
	fmt.Fprintln(os.Stdout, "             run `mellomting --install --systemd` (Linux root) to provision them")
	fmt.Fprintln(os.Stdout, "             and write a commented scaffold config (secrets remain operator-authored).")
}

// printSystemdNextSteps reports what provisioning created (configCreated
// says whether the scaffold config was written or an existing config was
// left in place) and the manual steps that remain. Secrets are
// operator-authored: the installer never writes users.yaml or the pepper
// file, and it never rewrites an existing config.yaml. The config dir is
// root:mellomting 0750 (only root can write it), so these steps run as
// root; the files they create are then made group-readable (0640
// root:mellomting) so the daemon, which runs as the service user, can
// read them — including users.yaml, which `key create` writes as 0600
// root:root.
func printSystemdNextSteps(binaryPath string, configCreated bool) {
	fmt.Fprintln(os.Stdout, "mellomting: systemd provisioning complete")
	fmt.Fprintf(os.Stdout, "  service account:  %s (created if absent)\n", systemd.DefaultServiceUser)
	fmt.Fprintln(os.Stdout, "  log/state dirs:   "+systemd.LogDir+", "+systemd.StateDir+" (mellomting, 0750); the runtime dir "+systemd.RunDir+" is recreated by systemd on each start")
	if configCreated {
		fmt.Fprintln(os.Stdout, "  config:           "+systemd.ConfigPath+" (scaffold created, 0640 root:mellomting — fill it in)")
	} else {
		fmt.Fprintln(os.Stdout, "  config:           "+systemd.ConfigPath+" (already present — left untouched)")
	}
	fmt.Fprintln(os.Stdout, "  unit:             "+systemd.UnitPath)
	fmt.Fprintln(os.Stdout, "  logrotate:        "+systemd.LogrotatePath)
	fmt.Fprintln(os.Stdout, "next steps (run as root, as you did for --install --systemd; secrets are operator-authored):")
	if configCreated {
		fmt.Fprintln(os.Stdout, "  editor "+systemd.ConfigPath+"  # fill in the backends: and models: sections")
	} else {
		fmt.Fprintln(os.Stdout, "  editor "+systemd.ConfigPath)
	}
	fmt.Fprintln(os.Stdout, "  umask 077 && head -c 64 /dev/urandom | base64 > "+systemd.PepperPath)
	fmt.Fprintln(os.Stdout, "  chown root:"+systemd.DefaultServiceUser+" "+systemd.PepperPath+" && chmod 640 "+systemd.PepperPath)
	fmt.Fprintln(os.Stdout, "  "+binaryPath+" key create --name <key-name> --models <model> -config "+systemd.ConfigPath)
	fmt.Fprintln(os.Stdout, "  chown root:"+systemd.DefaultServiceUser+" "+systemd.UsersPath+" && chmod 640 "+systemd.UsersPath)
	fmt.Fprintln(os.Stdout, "  "+binaryPath+" config check -config "+systemd.ConfigPath)
	fmt.Fprintln(os.Stdout, "  sudo systemctl enable --now "+systemd.UnitName)
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
	if st, err := os.Lstat(dest); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to replace %q: it is a symlink", dest)
		}
		if st.IsDir() {
			return fmt.Errorf("refusing to replace %q: it is a directory", dest)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %q: %w", dest, err)
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
	tmp, err := os.CreateTemp(destDir, ".mellomting-install-")
	if err != nil {
		return fmt.Errorf("create temporary file in %q: %w", destDir, err)
	}
	tmpName := tmp.Name()
	discard := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := io.Copy(tmp, in); err != nil {
		discard()
		return fmt.Errorf("copy %q to %q: %w", src, destDir, err)
	}
	st, err := tmp.Stat()
	if err != nil {
		discard()
		return fmt.Errorf("verify %q: %w", tmpName, err)
	}
	if st.Size() != srcInfo.Size() {
		discard()
		return fmt.Errorf("verify %q: copied %d bytes, source is %d", tmpName, st.Size(), srcInfo.Size())
	}
	// Fsync the data before the rename: rename(2) orders the directory
	// entry, not the file's blocks, so an unsynced copy can leave a
	// truncated binary at the destination after a crash.
	if err := tmp.Sync(); err != nil {
		discard()
		return fmt.Errorf("sync %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close %q: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("chmod %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace %q: %w", dest, err)
	}
	return nil
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
