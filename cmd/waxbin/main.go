// Command waxbin is the scriptable CLI for the WaxBin audio library engine.
//
// Every data command supports --json and returns a stable exit code mapped from
// the error's waxerr.Code (see `waxbin exit-codes`). The library never prints;
// the CLI owns all human/JSON output, and logs go to stderr.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

func main() {
	g := &globals{}
	err := newRootCmd(g).Execute()
	// Close out any maintenance-mode hand-off before exiting, so a server this
	// command borrowed the lock from is returned to service (runs after the
	// command's deferred Close released the lock).
	g.cleanup()
	if err != nil {
		// Cobra already prints the error; map it to a stable exit code.
		fmt.Fprintln(os.Stderr, "waxbin: "+err.Error())
		if hint := respelledPIDHint(err, os.Args[1:]); hint != "" {
			fmt.Fprintln(os.Stderr, "waxbin: "+hint)
		}
		os.Exit(exitCodeFor(err))
	}
}

// respelledPIDHint says how a pid among args should be spelled when a command found
// nothing. A pid is matched as text in upper case, so one typed in lower case reads as
// no such item, and nothing else tells the user why.
func respelledPIDHint(err error, args []string) string {
	if !waxerr.Is(err, waxerr.CodeNotFound) {
		return ""
	}
	for _, a := range args {
		if flag, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(flag, "-") {
			a = v
		}
		if up := strings.ToUpper(a); up != a && model.PID(up).Valid() {
			return "a pid is written in upper case: " + up
		}
	}
	return ""
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
