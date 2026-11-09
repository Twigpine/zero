//go:build darwin

#include "textflag.h"

TEXT libc_getattrlist_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_getattrlist(SB)
GLOBL ·libc_getattrlist_trampoline_addr(SB), RODATA, $8
DATA ·libc_getattrlist_trampoline_addr(SB)/8, $libc_getattrlist_trampoline<>(SB)

TEXT libc_open_extended_trampoline<>(SB),NOSPLIT,$0-0
	JMP libc_open_extended(SB)
GLOBL ·libc_open_extended_trampoline_addr(SB), RODATA, $8
DATA ·libc_open_extended_trampoline_addr(SB)/8, $libc_open_extended_trampoline<>(SB)

TEXT libc_mkdir_extended_trampoline<>(SB),NOSPLIT,$0-0
	JMP libc_mkdir_extended(SB)
GLOBL ·libc_mkdir_extended_trampoline_addr(SB), RODATA, $8
DATA ·libc_mkdir_extended_trampoline_addr(SB)/8, $libc_mkdir_extended_trampoline<>(SB)
