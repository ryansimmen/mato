package process

import (
	"os"
	"testing"
)

func TestIsProcessActive_CurrentPID(t *testing.T) {
	if !isProcessActive(os.Getpid()) {
		t.Fatal("current process should be active")
	}
}

func TestIsProcessActive_DeadPID(t *testing.T) {
	if isProcessActive(2147483647) {
		t.Fatal("non-existent PID should not be active")
	}
}

func TestIsProcessActive_InvalidPID(t *testing.T) {
	if isProcessActive(0) {
		t.Fatal("PID 0 should not be active")
	}
	if isProcessActive(-1) {
		t.Fatal("negative PID should not be active")
	}
}

func TestIsProcessActive_EPERMTreatedAsAlive(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root user (PID 1 returns EPERM only for non-root)")
	}
	// PID 1 (init/systemd) belongs to root; Signal(0) returns EPERM for non-root callers.
	if !isProcessActive(1) {
		t.Fatal("PID 1 should be considered active (EPERM means process exists)")
	}
}
