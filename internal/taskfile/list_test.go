package taskfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListTaskFiles(t *testing.T) {
	tests := []struct {
		name     string
		files    map[string]bool
		expected []string
	}{
		{
			name:     "empty directory",
			files:    map[string]bool{},
			expected: nil,
		},
		{
			name: "only md files",
			files: map[string]bool{
				"alpha.md": false,
				"beta.md":  false,
			},
			expected: []string{"alpha.md", "beta.md"},
		},
		{
			name: "skips directories",
			files: map[string]bool{
				"alpha.md": false,
				"subdir":   true,
			},
			expected: []string{"alpha.md"},
		},
		{
			name: "skips non-md files",
			files: map[string]bool{
				"alpha.md":   false,
				"readme.txt": false,
				".queue":     false,
				"beta.md":    false,
			},
			expected: []string{"alpha.md", "beta.md"},
		},
		{
			name: "sorted by name",
			files: map[string]bool{
				"charlie.md": false,
				"alpha.md":   false,
				"beta.md":    false,
			},
			expected: []string{"alpha.md", "beta.md", "charlie.md"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, isDir := range tt.files {
				path := filepath.Join(dir, name)
				if isDir {
					if err := os.Mkdir(path, 0o755); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.WriteFile(path, []byte("test"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}

			got, err := ListTaskFiles(dir)
			if err != nil {
				t.Fatalf("ListTaskFiles() error = %v", err)
			}
			if len(got) != len(tt.expected) {
				t.Fatalf("ListTaskFiles() = %v, want %v", got, tt.expected)
			}
			for i, name := range got {
				if name != tt.expected[i] {
					t.Errorf("ListTaskFiles()[%d] = %q, want %q", i, name, tt.expected[i])
				}
			}
		})
	}
}

func TestListTaskFiles_MissingDir(t *testing.T) {
	_, err := ListTaskFiles("/nonexistent/path")
	if err == nil {
		t.Fatal("expected error for missing directory")
	}
}

func TestListTaskFiles_SkipsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(target, []byte("# Secret\n"), 0o644); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real.md"), []byte("# Real\n"), 0o644); err != nil {
		t.Fatalf("write real task: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "linked.md")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	got, err := ListTaskFiles(dir)
	if err != nil {
		t.Fatalf("ListTaskFiles() error = %v", err)
	}
	if len(got) != 1 || got[0] != "real.md" {
		t.Fatalf("ListTaskFiles() = %v, want [real.md]", got)
	}
}
