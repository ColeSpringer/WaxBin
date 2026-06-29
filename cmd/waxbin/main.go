// Command waxbin is the scriptable CLI for the WaxBin audio library engine.
//
// Every data command supports --json and returns a stable exit code mapped from
// the error's waxerr.Code (see `waxbin exit-codes`). The library never prints;
// the CLI owns all human/JSON output, and logs go to stderr.
package main

import (
	"fmt"
	"os"

	"github.com/colespringer/waxbin/waxerr"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		// Cobra already prints the error; map it to a stable exit code.
		fmt.Fprintln(os.Stderr, "waxbin: "+err.Error())
		os.Exit(exitCodeFor(err))
	}
}

// Exit codes are part of the public contract (see the exit-codes command).
const (
	exitOK          = 0
	exitError       = 1 // generic/internal
	exitUsage       = 2 // invalid arguments / config
	exitNotFound    = 3
	exitConflict    = 4 // write ownership / lease conflict
	exitLocked      = 5
	exitIO          = 6
	exitUnsupported = 7
	exitCanceled    = 8 // context cancellation / deadline
)

func exitCodeFor(err error) int {
	switch waxerr.CodeOf(err) {
	case waxerr.CodeInvalid:
		return exitUsage
	case waxerr.CodeNotFound:
		return exitNotFound
	case waxerr.CodeConflict:
		return exitConflict
	case waxerr.CodeLocked:
		return exitLocked
	case waxerr.CodeIO:
		return exitIO
	case waxerr.CodeUnsupported:
		return exitUnsupported
	case waxerr.CodeCanceled:
		return exitCanceled
	default:
		return exitError
	}
}
