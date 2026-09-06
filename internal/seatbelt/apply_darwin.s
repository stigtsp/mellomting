//go:build darwin

#include "textflag.h"

// Trampolines for the two public libSystem symbols this package links.
// The Seatbelt SPI itself is reached through the pointers dlsym returns,
// which syscall.syscall can call directly, so it needs no trampoline of
// its own.

TEXT libc_dlopen_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_dlopen(SB)
GLOBL ·libc_dlopen_trampoline_addr(SB), RODATA, $8
DATA ·libc_dlopen_trampoline_addr(SB)/8, $libc_dlopen_trampoline<>(SB)

TEXT libc_dlsym_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_dlsym(SB)
GLOBL ·libc_dlsym_trampoline_addr(SB), RODATA, $8
DATA ·libc_dlsym_trampoline_addr(SB)/8, $libc_dlsym_trampoline<>(SB)
