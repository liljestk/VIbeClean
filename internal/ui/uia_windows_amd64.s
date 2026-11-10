#include "textflag.h"

// Go's Windows callback trampoline accepts integer and pointer arguments.
// Put UIA's two double coordinates into the matching x64 integer registers.
TEXT ·uiaPointStub(SB), NOSPLIT|NOFRAME, $0
	MOVQ X1, DX
	MOVQ X2, R8
	MOVQ ·uiaPointCallback(SB), AX
	JMP AX

TEXT ·uiaPointStubAddress(SB), NOSPLIT, $0-8
	LEAQ ·uiaPointStub(SB), AX
	MOVQ AX, ret+0(FP)
	RET
