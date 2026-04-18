package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/ryansimmen/mato/internal/ui"
)

const resumeDetectionBufferLimit = 8192

type resumeDetectionBuffer struct {
	matched bool
	buf     bytes.Buffer
}

func (b *resumeDetectionBuffer) Write(p []byte) (int, error) {
	b.buf.Write(p)
	if !b.matched && resumeRejectedBytes(b.buf.Bytes()) {
		b.matched = true
	}
	if b.buf.Len() > resumeDetectionBufferLimit {
		data := b.buf.Bytes()
		tail := data[len(data)-resumeDetectionBufferLimit:]
		b.buf.Reset()
		b.buf.Write(tail)
	}
	return len(p), nil
}

func (b *resumeDetectionBuffer) Matched() bool {
	return b.matched || resumeRejectedBytes(b.buf.Bytes())
}

// execCommandContext creates an *exec.Cmd bound to a context. It is a
// variable so tests can inject a stub without spawning real processes.
//
// NOTE: This is a package-level mutable variable used as a test seam.
// It prevents t.Parallel() within this package. Struct-based dependency
// injection would be needed for true parallel test safety.
var execCommandContext = exec.CommandContext

func runCopilotCommand(ctx context.Context, env envConfig, run runContext, extraEnvs []string, extraVolumes []string, label string, resetResumeSession func() string) error {
	runAttempt := func(current runContext) (bool, error) {
		args := buildDockerArgs(env, current, extraEnvs, extraVolumes)
		timeoutCtx, timeoutCancel := context.WithTimeout(ctx, current.timeout)
		defer timeoutCancel()

		cmd := execCommandContext(timeoutCtx, "docker", args...)
		cmd.Env = append(os.Environ(), env.authEnv...)
		cmd.Cancel = func() error {
			return cmd.Process.Signal(syscall.SIGTERM)
		}
		cmd.WaitDelay = gracefulShutdownDelay
		cmd.Stdin = os.Stdin

		var stdoutDetect resumeDetectionBuffer
		var stderrDetect resumeDetectionBuffer
		cmd.Stdout = io.MultiWriter(os.Stdout, &stdoutDetect)
		cmd.Stderr = io.MultiWriter(os.Stderr, &stderrDetect)

		err := cmd.Run()
		if timeoutCtx.Err() == context.DeadlineExceeded {
			fmt.Fprintf(os.Stderr, "error: %s timed out after %v\n", label, current.timeout)
		} else if ctx.Err() != nil {
			fmt.Fprintf(os.Stderr, "%s interrupted by signal\n", label)
		}
		return stdoutDetect.Matched() || stderrDetect.Matched(), err
	}

	rejected, err := runAttempt(run)
	if err == nil || strings.TrimSpace(run.resumeSessionID) == "" || resetResumeSession == nil || !rejected {
		return err
	}

	ui.Warnf("warning: Copilot resume rejected; retrying with a fresh session\n")
	if freshSessionID := strings.TrimSpace(resetResumeSession()); freshSessionID != "" && freshSessionID != run.resumeSessionID {
		run.resumeSessionID = freshSessionID
		_, freshErr := runAttempt(run)
		return freshErr
	}
	return err
}

func resumeRejected(output string) bool {
	for _, rawLine := range strings.Split(output, "\n") {
		if resumeRejectedLine([]byte(rawLine)) {
			return true
		}
	}
	return false
}

func resumeRejectedBytes(output []byte) bool {
	for len(output) > 0 {
		line := output
		if i := bytes.IndexByte(output, '\n'); i >= 0 {
			line = output[:i]
			output = output[i+1:]
		} else {
			output = nil
		}
		if resumeRejectedLine(line) {
			return true
		}
	}
	return false
}

func resumeRejectedLine(rawLine []byte) bool {
	line := bytes.TrimSpace(rawLine)
	if len(line) == 0 {
		return false
	}
	lower := bytes.ToLower(line)
	if !bytes.Contains(lower, []byte("resume")) && !bytes.Contains(lower, []byte("session")) {
		return false
	}

	// Phrases that are unambiguous stale-session indicators on their own,
	// even without an accompanying "error"/"failed"/"invalid" keyword.
	for _, phrase := range [][]byte{
		[]byte("unknown session"),
		[]byte("session not found"),
		[]byte("session expired"),
	} {
		if bytes.Contains(lower, phrase) {
			return true
		}
	}

	// For all other markers, require an error-class keyword to avoid
	// false positives on unrelated output that mentions "session".
	if !bytes.Contains(lower, []byte("error")) && !bytes.Contains(lower, []byte("failed")) && !bytes.Contains(lower, []byte("invalid")) {
		return false
	}
	for _, marker := range [][]byte{
		[]byte("resume session"),
		[]byte("--resume"),
		[]byte("cannot resume"),
		[]byte("failed to resume"),
		[]byte("invalid session"),
		[]byte("resume rejected"),
		[]byte("invalid value for '--resume'"),
		[]byte("unknown option '--resume'"),
	} {
		if bytes.Contains(lower, marker) {
			return true
		}
	}
	return false
}
