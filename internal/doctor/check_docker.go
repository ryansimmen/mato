package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ryansimmen/mato/internal/config"
)

// Function variable hooks for test injection.

var dockerLookPathFn = func() error {
	_, err := exec.LookPath("docker")
	return err
}

var dockerProbe = func(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "info").Run()
}

// dockerImageInspectFn checks whether a Docker image is available locally.
var dockerImageInspectFn = func(ctx context.Context, image string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "image", "inspect", image).Run()
}

// dockerImagePullFn pulls a Docker image. Used by --fix mode.
var dockerImagePullFn = func(ctx context.Context, image string) error {
	cmd := exec.CommandContext(ctx, "docker", "pull", image)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ExportDockerLookPathFn returns the current dockerLookPathFn for saving
// and restoring in integration tests.
func ExportDockerLookPathFn() func() error {
	return dockerLookPathFn
}

// SetDockerLookPathFn overrides dockerLookPathFn for testing.
func SetDockerLookPathFn(fn func() error) {
	dockerLookPathFn = fn
}

// ExportDockerProbe returns the current dockerProbe for saving and restoring
// in integration tests.
func ExportDockerProbe() func(context.Context) error {
	return dockerProbe
}

// SetDockerProbe overrides dockerProbe for testing.
func SetDockerProbe(fn func(context.Context) error) {
	dockerProbe = fn
}

// ExportDockerImageInspectFn returns the current dockerImageInspectFn for
// saving and restoring in integration tests.
func ExportDockerImageInspectFn() func(context.Context, string) error {
	return dockerImageInspectFn
}

// SetDockerImageInspectFn overrides dockerImageInspectFn for testing.
func SetDockerImageInspectFn(fn func(context.Context, string) error) {
	dockerImageInspectFn = fn
}

// resolveDockerImage returns the configured Docker image name from the
// environment, falling back to config.DefaultDockerImage.
func resolveDockerImage() string {
	if img := strings.TrimSpace(os.Getenv("MATO_DOCKER_IMAGE")); img != "" {
		return img
	}
	return config.DefaultDockerImage
}

// ---------- C. Docker ----------

func checkDocker(cc *checkContext) CheckReport {
	cr := CheckReport{Name: "docker", Status: CheckRan, Findings: []Finding{}}

	// Check if docker CLI exists.
	if err := dockerLookPathFn(); err != nil {
		cr.Findings = append(cr.Findings, Finding{
			Code:     "docker.cli_missing",
			Severity: SeverityError,
			Message:  "docker CLI not found in PATH",
		})
		return cr
	}

	if err := dockerProbe(cc.ctx); err != nil {
		cr.Findings = append(cr.Findings, Finding{
			Code:     "docker.daemon_unreachable",
			Severity: SeverityError,
			Message:  fmt.Sprintf("docker daemon unreachable: %v", err),
		})
		return cr
	}

	cr.Findings = append(cr.Findings, Finding{
		Code:     "docker.reachable",
		Severity: SeverityInfo,
		Message:  "docker daemon reachable",
	})

	if cc.configValidationFatal && cc.checkSelected("config") {
		return cr
	}

	// Check if the configured Docker image is available locally.
	image := cc.resolvedDockerImage
	if !cc.dockerImageResolved {
		image = cc.opts.DockerImage
	}
	if image == "" {
		image = resolveDockerImage()
	}
	if err := dockerImageInspectFn(cc.ctx, image); err != nil {
		f := Finding{
			Code:     "docker.image_missing",
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("Docker image %s not found locally", image),
			Fixable:  true,
		}

		if cc.opts.Fix {
			if pullErr := dockerImagePullFn(cc.ctx, image); pullErr != nil {
				f.Message = fmt.Sprintf("Docker image %s not found locally; pull failed: %v", image, pullErr)
				f.Severity = SeverityError
				f.Fixable = false
			} else {
				f.Fixed = true
				f.Fixable = false
				f.Message = fmt.Sprintf("Docker image %s pulled successfully", image)
			}
		}

		cr.Findings = append(cr.Findings, f)
	} else {
		cr.Findings = append(cr.Findings, Finding{
			Code:     "docker.image_available",
			Severity: SeverityInfo,
			Message:  fmt.Sprintf("Docker image %s available locally", image),
		})
	}

	return cr
}
