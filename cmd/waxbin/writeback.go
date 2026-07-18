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
			if f.Path != "" {
				fmt.Fprintf(errOut(cmd), "warning: on-disk write-back skipped for %s: %s\n", f.Path, f.Reason)
			} else {
				fmt.Fprintf(errOut(cmd), "warning: on-disk write-back skipped: %s\n", f.Reason)
			}
		}
		return nil
	}
	return err
}
