//go:build darwin

package seatbelt

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	_ "unsafe" // for go:linkname

	"mellomting/internal/sandbox"
)

// The Seatbelt entry points are Apple SPI: they are not in sandbox.h and
// carry no compatibility promise. They are therefore resolved at run
// time rather than linked, so a macOS release that drops them leaves a
// daemon that reports an unavailable sandbox — the same fail-closed path
// as an old kernel on Linux — instead of a binary that cannot launch at
// all. Only dlopen and dlsym are linked, and both are public libSystem.
//
//go:cgo_import_dynamic libc_dlopen dlopen "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_dlsym dlsym "/usr/lib/libSystem.B.dylib"

//go:linkname syscall_syscall syscall.syscall
func syscall_syscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)

var (
	libc_dlopen_trampoline_addr uintptr
	libc_dlsym_trampoline_addr  uintptr
)

// sandboxLib is where the profile compiler lives. libsystem_sandbox
// re-exports sandbox_init_with_parameters but not the compiler, and
// compiling separately is what lets a profile be validated without
// being applied.
const sandboxLib = "/usr/lib/libsandbox.1.dylib"

// rtldNow resolves every symbol at load time; the library is small and
// a lazy failure would surface at the worst moment.
const rtldNow = 0x2

// symbols is the resolved SPI, looked up once.
type symbols struct {
	compileString uintptr
	apply         uintptr
	freeProfile   uintptr
	freeError     uintptr
	check         uintptr
	err           error
}

var loadSymbols = sync.OnceValue(func() symbols {
	h, err := dlopen(sandboxLib)
	if err != nil {
		return symbols{err: err}
	}
	var s symbols
	for _, sym := range []struct {
		name string
		into *uintptr
	}{
		{"sandbox_compile_string", &s.compileString},
		{"sandbox_apply", &s.apply},
		{"sandbox_free_profile", &s.freeProfile},
		{"sandbox_free_error", &s.freeError},
		{"sandbox_check", &s.check},
	} {
		addr, err := dlsym(h, sym.name)
		if err != nil {
			return symbols{err: err}
		}
		*sym.into = addr
	}
	return s
})

func dlopen(path string) (uintptr, error) {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return 0, err
	}
	h, _, _ := syscall_syscall(libc_dlopen_trampoline_addr, uintptr(unsafe.Pointer(p)), rtldNow, 0)
	runtime.KeepAlive(p)
	if h == 0 {
		return 0, fmt.Errorf("%s is not available on this system", path)
	}
	return h, nil
}

func dlsym(handle uintptr, name string) (uintptr, error) {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return 0, err
	}
	addr, _, _ := syscall_syscall(libc_dlsym_trampoline_addr, handle, uintptr(unsafe.Pointer(p)), 0)
	runtime.KeepAlive(p)
	if addr == 0 {
		return 0, fmt.Errorf("this system's sandbox library does not export %s", name)
	}
	return addr, nil
}

// compiled is a compiled profile that has not been applied yet.
type compiled struct {
	handle uintptr
	syms   symbols
}

// compile translates a profile into the kernel's representation without
// applying anything. A profile the system rejects — an operation this
// macOS does not know, a malformed rule — is reported here, so the
// daemon never applies a policy it could not fully express.
func compile(profile string) (*compiled, error) {
	syms := loadSymbols()
	if syms.err != nil {
		return nil, syms.err
	}
	p, err := syscall.BytePtrFromString(profile)
	if err != nil {
		return nil, err
	}
	var errbuf *byte
	handle, _, _ := syscall_syscall(syms.compileString,
		uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&errbuf)))
	runtime.KeepAlive(p)
	if handle == 0 {
		return nil, fmt.Errorf("sandbox profile was rejected: %s", takeError(syms, errbuf))
	}
	return &compiled{handle: handle, syms: syms}, nil
}

// free releases a compiled profile that was not applied.
func (c *compiled) free() {
	syscall_syscall(c.syms.freeProfile, c.handle, 0, 0)
}

// apply enforces the compiled profile on this process. Seatbelt confines
// the whole process, so unlike Landlock there is no per-thread step: no
// thread the Go runtime spawns afterwards escapes it. It cannot be
// undone.
func (c *compiled) apply() error {
	defer c.free()
	r1, _, errno := syscall_syscall(c.syms.apply, c.handle, 0, 0)
	if int32(r1) == 0 {
		return nil
	}
	if errno == 0 {
		return errors.New("sandbox_apply failed")
	}
	// A process that is already confined may be refused a second
	// profile, depending on what the outer one permits. Saying so turns
	// a bare EPERM into something the operator can act on.
	if errors.Is(errno, syscall.EPERM) && alreadyConfined(c.syms) {
		return fmt.Errorf("sandbox_apply: %w (this process is already confined by another sandbox, which may not permit a second profile)", errno)
	}
	return fmt.Errorf("sandbox_apply: %w", errno)
}

// alreadyConfined reports whether some sandbox already applies to this
// process. It is used only to explain a refusal: nesting does not
// always prevent a second profile, so it is not a capability answer.
func alreadyConfined(syms symbols) bool {
	r1, _, _ := syscall_syscall(syms.check, uintptr(os.Getpid()), 0, 0)
	return int32(r1) == 1
}

// takeError renders and frees a libsandbox error string. The buffer is
// libsandbox's, so freeing it is this side's job.
func takeError(syms symbols, errbuf *byte) string {
	if errbuf == nil {
		return "no reason reported"
	}
	// The conversion sits inside the deferred call rather than in the
	// defer statement, so it is part of the call expression the unsafe
	// rules are written about.
	defer func() { syscall_syscall(syms.freeError, uintptr(unsafe.Pointer(errbuf)), 0, 0) }()
	var n int
	for p := unsafe.Pointer(errbuf); *(*byte)(p) != 0; n++ {
		p = unsafe.Add(p, 1)
	}
	// libsandbox appends a multi-line backtrace of its own parser; the
	// first line is the diagnosis and the rest is noise in a log.
	msg := string(unsafe.Slice(errbuf, n))
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	return msg
}

// Check probes for Seatbelt without applying any restriction: it
// resolves the SPI and compiles a trivial profile, which exercises the
// same compiler the real policy goes through.
func Check() sandbox.Report {
	r := sandbox.Report{Platform: runtime.GOOS, Backend: Backend}
	// The empty policy still carries the profile's skeleton, imports
	// included: a macOS that no longer ships the baseline profile has
	// to be reported here rather than discovered at Apply.
	c, err := compile(Profile(sandbox.Policy{}))
	if err != nil {
		r.Reason = err.Error()
		return r
	}
	c.free()
	r.Supported = true
	return r
}

// Apply compiles pol and enforces it on this process (PLAN §56, §57).
// It is strictly fail-closed: a profile that does not compile, or an
// enforcement the system refuses, is an error and never a silent
// downgrade (PLAN §55 step 5). The serve mode gate decides whether to
// enforce or to abort.
func Apply(pol sandbox.Policy) error {
	c, err := compile(Profile(pol))
	if err != nil {
		return err
	}
	return c.apply()
}
