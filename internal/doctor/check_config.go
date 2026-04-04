package doctor

import (
	"fmt"
	"strings"

	"mato/internal/configresolve"
)

func checkConfig(cc *checkContext) CheckReport {
	cr := CheckReport{Name: "config", Status: CheckRan, Findings: []Finding{}}

	if !cc.hasRepo() {
		if cc.checkSelected("git") {
			return cr
		}
		msg := "cannot validate config: no valid git repository"
		if cc.repoErr != nil {
			msg = fmt.Sprintf("cannot validate config (%s)", cc.repoErrDetail())
		}
		cr.Findings = append(cr.Findings, Finding{
			Code:     "config.no_repo",
			Severity: SeverityError,
			Message:  msg,
		})
		return cr
	}

	result, err := configresolve.ValidateRepoDefaults(cc.repoRoot)
	if err != nil {
		cc.configValidationFatal = true
		cr.Findings = append(cr.Findings, Finding{
			Code:     "config.parse_error",
			Severity: SeverityError,
			Message:  err.Error(),
			Path:     configPathFromError(err),
		})
		return cr
	}

	cc.configValidationResult = result
	if result.Resolved.DockerImage.Value != "" {
		cc.resolvedDockerImage = result.Resolved.DockerImage.Value
		cc.dockerImageResolved = true
	}

	for _, issue := range result.Issues {
		cr.Findings = append(cr.Findings, Finding{
			Code:     issue.Code,
			Severity: SeverityError,
			Message:  issue.Message,
			Path:     issue.ConfigPath,
		})
	}

	return cr
}

func configPathFromError(err error) string {
	msg := err.Error()
	const prefix = "parse config file "
	if strings.HasPrefix(msg, prefix) {
		rest := strings.TrimPrefix(msg, prefix)
		if i := strings.Index(rest, ":"); i > 0 {
			return rest[:i]
		}
	}
	return ""
}
