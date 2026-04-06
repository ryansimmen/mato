package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"mato/internal/ui"
)

var colors = ui.NewColorSet()

// colorIndicator wraps a plain-text category indicator with ANSI color.
func colorIndicator(indicator string) string {
	switch indicator {
	case "[OK]":
		return colors.Green(indicator)
	case "[ERROR]":
		return colors.Red(indicator)
	case "[WARN]":
		return colors.Yellow(indicator)
	case "[SKIP]":
		return colors.Dim(indicator)
	default:
		return indicator
	}
}

// RenderText writes a human-readable text report to w.
func RenderText(w io.Writer, r Report) error {
	if _, err := fmt.Fprintln(w, formatSummaryLine(r.Summary)); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

	for _, cr := range r.Checks {
		indicator := colorIndicator(categoryIndicator(cr))
		fixed := fixedCount(cr)

		if cr.Status == CheckSkipped {
			if _, err := fmt.Fprintf(w, "%s %s\n", indicator, cr.Name); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
			continue
		}

		if fixed > 0 {
			if _, err := fmt.Fprintf(w, "%s %s (%s)\n", indicator, cr.Name, pluralize(fixed, "fixed", "fixed")); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintf(w, "%s %s\n", indicator, cr.Name); err != nil {
				return err
			}
		}

		for _, f := range cr.Findings {
			suffix := severitySuffix(f)
			if f.Path != "" {
				if _, err := fmt.Fprintf(w, "  - %s: %s%s\n", f.Path, f.Message, suffix); err != nil {
					return err
				}
			} else {
				if _, err := fmt.Fprintf(w, "  - %s%s\n", f.Message, suffix); err != nil {
					return err
				}
			}
		}

		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}

	return nil
}

// RenderJSON writes a JSON report to w.
func RenderJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// severitySuffix returns the inline annotation for a finding in text mode.
func severitySuffix(f Finding) string {
	if f.Fixed {
		return " (fixed)"
	}
	var parts []string
	switch f.Severity {
	case SeverityWarning:
		parts = append(parts, "warning")
	case SeverityError:
		parts = append(parts, "error")
	}
	if f.Fixable {
		parts = append(parts, "fixable with --fix")
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}
