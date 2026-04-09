package ui

import (
	"errors"
	"strings"
)

type hintCarrier interface {
	RemediationHint() string
}

type hintedError struct {
	err  error
	hint string
}

func (e *hintedError) Error() string {
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *hintedError) Unwrap() error {
	return e.err
}

func (e *hintedError) RemediationHint() string {
	return e.hint
}

// WithHint wraps err with a remediation hint that callers can render
// separately from the main error line.
func WithHint(err error, hint string) error {
	if err == nil {
		return nil
	}
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return err
	}
	return &hintedError{err: err, hint: hint}
}

// ErrorHint returns the first remediation hint attached anywhere in err's wrap
// chain.
func ErrorHint(err error) (string, bool) {
	var carrier hintCarrier
	if !errors.As(err, &carrier) {
		return "", false
	}
	hint := strings.TrimSpace(carrier.RemediationHint())
	if hint == "" {
		return "", false
	}
	return hint, true
}
