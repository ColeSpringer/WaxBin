package main

import (
	"errors"
	"fmt"

	"github.com/colespringer/waxbin"
	"github.com/spf13/cobra"
)

// surfaceWriteBack reports a *WriteBackError as per-file warnings and returns nil (the
// catalog edit committed; only the on-disk sync did not fully apply), or returns any
// other error unchanged. It is the shared CLI handling for the opt-in write-back
// commands (edit, credit, art set, entity edit) so a partial on-disk sync reads as a
// warning, not a failed command.
func surfaceWriteBack(cmd *cobra.Command, err error) error {
	var wbErr *waxbin.WriteBackError
	if errors.As(err, &wbErr) {
		for _, f := range wbErr.Failures {
			switch {
			case f.Lost && f.Key != "":
				fmt.Fprintf(errOut(cmd), "warning: %s was written but cannot hold %s as given\n", f.Path, f.Key)
			case f.Lost:
				fmt.Fprintf(errOut(cmd), "warning: %s was written but cannot hold every value as given\n", f.Path)
			case f.Path != "" && f.Key != "":
				fmt.Fprintf(errOut(cmd), "warning: on-disk write-back of %s skipped for %s: %s\n", f.Key, f.Path, f.Reason)
			case f.Path != "":
				fmt.Fprintf(errOut(cmd), "warning: on-disk write-back skipped for %s: %s\n", f.Path, f.Reason)
			default:
				fmt.Fprintf(errOut(cmd), "warning: on-disk write-back skipped: %s\n", f.Reason)
			}
		}
		return nil
	}
	return err
}
