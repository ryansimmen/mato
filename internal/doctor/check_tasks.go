package doctor

import (
	"fmt"
	"path/filepath"
	"strings"

	"mato/internal/queue"
)

// ---------- E. Task Parsing ----------

func checkTaskParsing(cc *checkContext) CheckReport {
	cr := CheckReport{Name: "tasks", Status: CheckRan, Findings: []Finding{}}

	idx := cc.ensureIndex()

	// Parse failures.
	for _, pf := range idx.ParseFailures() {
		cr.Findings = append(cr.Findings, Finding{
			Code:     "tasks.parse_error",
			Severity: SeverityError,
			Message:  fmt.Sprintf("%s/%s: %v", pf.State, pf.Filename, pf.Err),
			Path:     pf.Path,
		})
	}

	// Invalid glob and unsafe affects errors from BuildWarnings — error
	// severity to match the runtime, which quarantines affected tasks into
	// failed/.
	for _, bw := range idx.BuildWarnings() {
		errMsg := bw.Err.Error()
		if strings.Contains(errMsg, "glob") || strings.Contains(errMsg, "affects") {
			code := "tasks.invalid_glob"
			if strings.Contains(errMsg, "unsafe affects") {
				code = "tasks.unsafe_affects"
			}
			cr.Findings = append(cr.Findings, Finding{
				Code:     code,
				Severity: SeverityError,
				Message:  fmt.Sprintf("%s: %v", filepath.Base(bw.Path), bw.Err),
				Path:     bw.Path,
			})
		}
	}

	// Total parsed count.
	total := 0
	for _, dir := range queue.AllDirs {
		total += len(idx.TasksByState(dir))
	}
	cr.Findings = append(cr.Findings, Finding{
		Code:     "tasks.total_count",
		Severity: SeverityInfo,
		Message:  fmt.Sprintf("total parsed tasks: %d", total),
	})

	return cr
}
