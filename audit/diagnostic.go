package audit

import (
	"context"
	"strconv"

	"github.com/colespringer/waxbin/model"
)

// diagSeverities are the bands a diagnostic finding is capped within, most severe
// first. One capped bucket per band keeps each "N more" summary meaningful, since a
// flood of info rows would otherwise consume the budget an error row needs.
var diagSeverities = []model.AuditSeverity{model.SeverityError, model.SeverityWarn, model.SeverityInfo}

// reportFileDiagnostics emits a finding per persisted diagnostic, leaving out
// corrupt_audio.
//
// That exclusion keeps one concept to one --check name. model.CheckCorruptAudio
// already exists, so routing corrupt_audio through here as well would leave
// `--check corrupt_audio` and `--check file_diagnostic` as two flags for the same
// thing. CheckCorruptAudio owns the code and dedups its own two halves.
func (a *Auditor) reportFileDiagnostics(ds []model.FileDiagnostic, sample int, add func(model.AuditFinding)) {
	caps := make(map[model.AuditSeverity]*capped, len(diagSeverities))
	for _, sev := range diagSeverities {
		caps[sev] = &capped{limit: sample, check: model.CheckFileDiagnostic, sev: sev, add: add}
	}
	for _, d := range ds {
		if d.Code == model.DiagCorruptAudio {
			continue
		}
		c, ok := caps[d.Severity]
		if !ok {
			// An unknown severity is reported rather than dropped; warn is the neutral band.
			c = caps[model.SeverityWarn]
		}
		c.emit(model.AuditFinding{
			Check:    model.CheckFileDiagnostic,
			Severity: d.Severity,
			Message:  diagMessage(d),
			Path:     d.DisplayPath,
		})
	}
	for _, sev := range diagSeverities {
		caps[sev].summary("file diagnostics (" + string(sev) + ")")
	}
}

// reportCorruptDiagnostics emits corrupt_audio findings from the diagnostics the
// scan already derived. It is the cheap half of CheckCorruptAudio, needing no file
// I/O, so it runs by default. It returns the display paths it reported so the probe
// half can skip them, leaving a file both halves flag with one finding rather than
// two.
func (a *Auditor) reportCorruptDiagnostics(ds []model.FileDiagnostic, sample int, add func(model.AuditFinding)) map[string]bool {
	seen := map[string]bool{}
	caps := make(map[model.AuditSeverity]*capped, len(diagSeverities))
	for _, sev := range diagSeverities {
		caps[sev] = &capped{limit: sample, check: model.CheckCorruptAudio, sev: sev, add: add}
	}
	for _, d := range ds {
		if d.Code != model.DiagCorruptAudio {
			continue
		}
		seen[d.DisplayPath] = true
		c, ok := caps[d.Severity]
		if !ok {
			c = caps[model.SeverityWarn]
		}
		c.emit(model.AuditFinding{
			Check:    model.CheckCorruptAudio,
			Severity: d.Severity,
			Message:  diagMessage(d),
			Path:     d.DisplayPath,
		})
	}
	for _, sev := range diagSeverities {
		caps[sev].summary("files with corrupt audio (" + string(sev) + ")")
	}
	return seen
}

// reportDiagnosticCoverage emits an info finding naming how many files have not had
// diagnostics derived under the current rule set.
//
// It resolves what no rows would otherwise mean, which is clean, not yet derived, or
// derived under an older rule set, differently for each file. Force is set
// opportunistically by the watcher's periodic rescan, any cover change, and any
// sidecar edit, so the derived set would fill in unevenly and the counts would drift
// between runs for reasons that have nothing to do with the library.
//
// The finding costs one count and lets the user choose the expensive pass. A version
// mismatch re-derives nothing on its own: the scan fast-path's escape skips content
// hashing and a per-file write transaction along with the tag re-read, so forcing
// here would amount to `scan --force`, unannounced, on every library that upgrades.
func (a *Auditor) reportDiagnosticCoverage(ctx context.Context, add func(model.AuditFinding)) error {
	stale, total, err := a.store.DiagnosticCoverage(ctx)
	if err != nil {
		return err
	}
	if stale == 0 {
		return nil
	}
	add(model.AuditFinding{
		Check:    model.CheckFileDiagnostic,
		Severity: model.SeverityInfo,
		Message: "diagnostics not derived for " + strconv.Itoa(stale) + " of " + strconv.Itoa(total) +
			" files; run scan --force",
	})
	return nil
}

// diagMessage renders a diagnostic as a finding message: the code word, the path,
// and the detail the writer recorded (already length-capped at the meta seam).
func diagMessage(d model.FileDiagnostic) string {
	m := string(d.Code) + ": " + d.DisplayPath
	if d.TagKey != "" {
		m += " [" + d.TagKey + "]"
	}
	if d.Detail != "" {
		m += ": " + d.Detail
	}
	return m
}
