package doctor

import (
	"fmt"
	"os/exec"
	"strings"
)

// ---------- A. Git Repository ----------

func checkGit(cc *checkContext) CheckReport {
	cr := CheckReport{Name: "git", Status: CheckRan, Findings: []Finding{}}

	// resolveRepo was already called before the check loop, so
	// cc.repoRoot is populated on success and cc.repoErr on failure.
	if cc.repoRoot == "" {
		code := "git.resolve_failed"
		detail := cc.repoErrDetail()
		msg := fmt.Sprintf("failed to resolve git repository: %s", detail)

		// Only classify as "not a repo" when git itself says so.
		if strings.Contains(detail, "not a git repository") {
			code = "git.not_a_repo"
			msg = fmt.Sprintf("not a git repository: %s", cc.repoInput)
		}

		cr.Findings = append(cr.Findings, Finding{
			Code:     code,
			Severity: SeverityError,
			Message:  msg,
			Path:     cc.repoInput,
		})
		return cr
	}

	repoRoot := cc.repoRoot

	cr.Findings = append(cr.Findings, Finding{
		Code:     "git.repo_root",
		Severity: SeverityInfo,
		Message:  fmt.Sprintf("repo root: %s", repoRoot),
	})

	// Current branch.
	branchOut, err := exec.CommandContext(cc.ctx, "git", "-C", repoRoot, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err == nil {
		branch := strings.TrimSpace(string(branchOut))
		cr.Findings = append(cr.Findings, Finding{
			Code:     "git.branch",
			Severity: SeverityInfo,
			Message:  fmt.Sprintf("current branch: %s", branch),
		})
	}

	return cr
}
