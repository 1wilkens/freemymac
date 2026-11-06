//go:build darwin

#include "textflag.h"

// Trampoline into libc's getattrlistbulk, same pattern as golang.org/x/sys/unix.
TEXT libc_getattrlistbulk_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_getattrlistbulk(SB)
GLOBL	·libc_getattrlistbulk_trampoline_addr(SB), RODATA, $8
DATA	·libc_getattrlistbulk_trampoline_addr(SB)/8, $libc_getattrlistbulk_trampoline<>(SB)
