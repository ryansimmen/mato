package doctor

import (
	"github.com/ryansimmen/mato/internal/runner"
)

// Function variable hook for test injection.
var inspectHostToolsFn = runner.InspectHostTools

// ExportInspectHostToolsFn returns the current inspectHostToolsFn for saving
// and restoring in integration tests.
func ExportInspectHostToolsFn() func() runner.ToolReport {
	return inspectHostToolsFn
}

// SetInspectHostToolsFn overrides inspectHostToolsFn for testing.
func SetInspectHostToolsFn(fn func() runner.ToolReport) {
	inspectHostToolsFn = fn
}

// ---------- B. Host Tools ----------

func checkTools(cc *checkContext) CheckReport {
	cr := CheckReport{Name: "tools", Status: CheckRan, Findings: []Finding{}}

	report := inspectHostToolsFn()
	for _, tf := range report.Findings {
		var sev Severity
		if !tf.Found {
			if tf.Required {
				sev = SeverityError
			} else {
				sev = SeverityWarning
			}
		} else {
			sev = SeverityInfo
		}

		code := "tools.found"
		if !tf.Found {
			if tf.Required {
				if tf.Name == ".copilot" {
					code = "tools.missing_copilot_dir"
				} else {
					code = "tools.missing_required"
				}
			} else {
				code = "tools.missing_optional"
			}
		}

		cr.Findings = append(cr.Findings, Finding{
			Code:     code,
			Severity: sev,
			Message:  tf.Message,
			Path:     tf.Path,
		})
	}

	return cr
}
