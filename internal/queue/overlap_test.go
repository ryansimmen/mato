package queue

import (
	"reflect"
	"testing"
)

func TestAffectsMatch(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"exact match", "foo.go", "foo.go", true},
		{"no match", "foo.go", "bar.go", false},
		{"prefix matches file", "pkg/client/", "pkg/client/http.go", true},
		{"file matches prefix", "pkg/client/http.go", "pkg/client/", true},
		{"prefix matches nested file", "pkg/", "pkg/client/http.go", true},
		{"nested file matches prefix", "pkg/client/http.go", "pkg/", true},
		{"prefix matches prefix contained", "pkg/", "pkg/client/", true},
		{"prefix matches prefix reverse", "pkg/client/", "pkg/", true},
		{"same prefix", "pkg/client/", "pkg/client/", true},
		{"non-overlapping prefixes", "pkg/client/", "pkg/server/", false},
		{"prefix no match exact file", "pkg/client/", "pkg/server/main.go", false},
		{"trailing slash required", "pkg/client", "pkg/client/http.go", false},
		{"empty a", "", "foo.go", false},
		{"empty b", "foo.go", "", false},
		{"both empty", "", "", true},
		// Glob vs concrete path cases
		{"glob matches file", "internal/runner/*.go", "internal/runner/task.go", true},
		{"glob no match file", "internal/runner/*.go", "internal/queue/queue.go", false},
		{"doublestar matches nested", "**/*.go", "internal/runner/task.go", true},
		{"doublestar matches top-level", "**/*.go", "main.go", true},
		{"glob no match extension", "internal/runner/*.go", "internal/runner/task.txt", false},
		{"glob matches reverse", "internal/runner/task.go", "internal/runner/*.go", true},
		// Glob vs directory prefix cases
		{"glob vs dir prefix overlapping", "internal/runner/*.go", "internal/runner/", true},
		{"glob vs dir prefix parent", "internal/runner/*.go", "internal/", true},
		{"glob vs dir prefix disjoint", "internal/runner/*.go", "pkg/", false},
		{"dir prefix vs glob overlapping", "internal/runner/", "internal/runner/*.go", true},
		{"doublestar vs any dir prefix", "**/*.go", "internal/", true},
		{"dir prefix vs doublestar", "internal/", "**/*.go", true},
		// Glob vs glob cases
		{"glob vs glob overlapping prefix", "internal/runner/*.go", "internal/runner/*_test.go", true},
		{"glob vs glob disjoint prefix", "internal/runner/*.go", "pkg/client/*.go", false},
		{"glob vs glob one doublestar", "**/*.go", "internal/runner/*.go", true},
		{"glob vs glob both doublestar", "**/*.go", "**/*_test.go", true},
		// Malformed glob conservatively conflicts via static prefix comparison.
		{"malformed glob same prefix", "internal/[bad", "internal/runner/task.go", true},
		{"malformed glob disjoint prefix", "internal/[bad", "pkg/client/http.go", false},
		{"malformed glob empty prefix", "[bad", "anything.go", true},
		// Glob with trailing slash is an invalid-glob entry.
		{"glob trailing slash vs file under", "internal/*/", "internal/runner/task.go", true},
		{"glob trailing slash vs disjoint", "internal/*/", "pkg/client.go", false},
		// Invalid glob vs valid glob: valid side also reduced to static prefix.
		{"malformed glob vs doublestar", "internal/[bad", "**/*.go", true},
		{"malformed glob vs overlapping glob", "internal/[bad", "internal/runner/*.go", true},
		{"malformed glob vs disjoint glob", "internal/[bad", "pkg/client/*.go", false},
		{"malformed glob vs brace glob", "internal/[bad", "{internal,pkg}/**/*.go", true},
		{"glob slash vs doublestar", "internal/*/", "**/*.go", true},
		{"glob slash vs overlapping glob", "internal/*/", "internal/runner/*.go", true},
		{"glob slash vs disjoint glob", "internal/*/", "pkg/client/*.go", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := affectsMatch(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("affectsMatch(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestAffectsMatch_Symmetry(t *testing.T) {
	pairs := [][2]string{
		{"foo.go", "bar.go"},
		{"pkg/client/", "pkg/client/http.go"},
		{"internal/runner/*.go", "internal/runner/task.go"},
		{"**/*.go", "internal/runner/task.go"},
		{"internal/runner/*.go", "internal/runner/"},
		{"**/*.go", "internal/"},
		{"internal/runner/*.go", "pkg/client/*.go"},
		{"internal/runner/*.go", "internal/runner/*_test.go"},
		{"**/*.go", "**/*_test.go"},
		{"internal/*.go", "internal/r*.go"},
		// Invalid glob entries
		{"internal/[bad", "internal/runner/task.go"},
		{"internal/[bad", "pkg/client.go"},
		{"[bad", "anything.go"},
		{"internal/*/", "internal/runner/task.go"},
		{"internal/*/", "pkg/client.go"},
		// Invalid glob vs valid glob
		{"internal/[bad", "**/*.go"},
		{"internal/[bad", "internal/runner/*.go"},
		{"internal/[bad", "pkg/client/*.go"},
		{"internal/*/", "**/*.go"},
		{"internal/*/", "internal/runner/*.go"},
		{"internal/*/", "pkg/client/*.go"},
	}
	for _, p := range pairs {
		ab := affectsMatch(p[0], p[1])
		ba := affectsMatch(p[1], p[0])
		if ab != ba {
			t.Errorf("asymmetric: affectsMatch(%q, %q)=%v but affectsMatch(%q, %q)=%v",
				p[0], p[1], ab, p[1], p[0], ba)
		}
	}
}

func TestStaticPrefix(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		want    string
	}{
		{"file glob", "internal/runner/*.go", "internal/runner/"},
		{"doublestar", "**/*.go", ""},
		{"no glob", "foo.go", "foo.go"},
		{"brace expansion", "internal/{a,b}/*.go", "internal/"},
		{"glob at root", "*.go", ""},
		{"nested doublestar", "internal/**/*_test.go", "internal/"},
		{"question mark", "internal/runner/task?.go", "internal/runner/"},
		{"char class", "data[1].csv", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := staticPrefix(tt.pattern); got != tt.want {
				t.Errorf("staticPrefix(%q) = %q, want %q", tt.pattern, got, tt.want)
			}
		})
	}
}

func TestOverlappingAffects(t *testing.T) {
	tests := []struct {
		name string
		a, b []string
		want []string
	}{
		{
			name: "exact match",
			a:    []string{"foo.go", "bar.go"},
			b:    []string{"bar.go", "baz.go"},
			want: []string{"bar.go"},
		},
		{
			name: "no overlap",
			a:    []string{"foo.go"},
			b:    []string{"bar.go"},
			want: nil,
		},
		{
			name: "nil a",
			a:    nil,
			b:    []string{"foo.go"},
			want: nil,
		},
		{
			name: "nil b",
			a:    []string{"foo.go"},
			b:    nil,
			want: nil,
		},
		{
			name: "both nil",
			a:    nil,
			b:    nil,
			want: nil,
		},
		{
			name: "empty strings filtered",
			a:    []string{"", "foo.go"},
			b:    []string{"foo.go", ""},
			want: []string{"foo.go"},
		},
		{
			name: "prefix in a matches file in b",
			a:    []string{"pkg/client/"},
			b:    []string{"pkg/client/http.go"},
			want: []string{"pkg/client/", "pkg/client/http.go"},
		},
		{
			name: "file in a matched by prefix in b",
			a:    []string{"pkg/client/http.go"},
			b:    []string{"pkg/client/"},
			want: []string{"pkg/client/", "pkg/client/http.go"},
		},
		{
			name: "prefix vs prefix nested",
			a:    []string{"pkg/"},
			b:    []string{"pkg/client/"},
			want: []string{"pkg/", "pkg/client/"},
		},
		{
			name: "non-overlapping prefixes",
			a:    []string{"pkg/client/"},
			b:    []string{"pkg/server/"},
			want: nil,
		},
		{
			name: "mixed exact and prefix",
			a:    []string{"README.md", "pkg/client/"},
			b:    []string{"pkg/client/http.go", "README.md"},
			want: []string{"README.md", "pkg/client/", "pkg/client/http.go"},
		},
		{
			name: "duplicate in b",
			a:    []string{"foo.go"},
			b:    []string{"foo.go", "foo.go"},
			want: []string{"foo.go"},
		},
		{
			name: "all exact no prefix fast path",
			a:    []string{"a.go", "b.go", "c.go"},
			b:    []string{"b.go", "c.go", "d.go"},
			want: []string{"b.go", "c.go"},
		},
		{
			name: "broad prefix matches multiple",
			a:    []string{"internal/"},
			b:    []string{"internal/queue/queue.go", "internal/merge/merge.go", "docs/readme.md"},
			want: []string{"internal/", "internal/merge/merge.go", "internal/queue/queue.go"},
		},
		{
			name: "glob matches exact file",
			a:    []string{"internal/runner/*.go"},
			b:    []string{"internal/runner/task.go"},
			want: []string{"internal/runner/*.go", "internal/runner/task.go"},
		},
		{
			name: "glob no match",
			a:    []string{"internal/runner/*.go"},
			b:    []string{"pkg/client/http.go"},
			want: nil,
		},
		{
			name: "glob vs prefix",
			a:    []string{"internal/runner/*.go"},
			b:    []string{"internal/runner/"},
			want: []string{"internal/runner/", "internal/runner/*.go"},
		},
		{
			name: "glob vs glob overlapping",
			a:    []string{"internal/runner/*.go"},
			b:    []string{"internal/runner/*_test.go"},
			want: []string{"internal/runner/*.go", "internal/runner/*_test.go"},
		},
		{
			name: "glob vs glob disjoint",
			a:    []string{"internal/runner/*.go"},
			b:    []string{"pkg/client/*.go"},
			want: nil,
		},
		{
			name: "doublestar matches everything",
			a:    []string{"**/*.go"},
			b:    []string{"internal/runner/task.go"},
			want: []string{"**/*.go", "internal/runner/task.go"},
		},
		{
			name: "mixed exact glob and prefix",
			a:    []string{"README.md", "internal/runner/*.go"},
			b:    []string{"internal/runner/task.go", "README.md"},
			want: []string{"README.md", "internal/runner/*.go", "internal/runner/task.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := overlappingAffects(tt.a, tt.b)
			if len(got) == 0 {
				got = nil
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("overlappingAffects(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestOverlappingAffects_SymmetricWhenPrefixesPresent(t *testing.T) {
	a := []string{"pkg/client/", "README.md"}
	b := []string{"pkg/client/http.go", "README.md"}

	gotAB := overlappingAffects(a, b)
	gotBA := overlappingAffects(b, a)
	if !reflect.DeepEqual(gotAB, gotBA) {
		t.Fatalf("overlappingAffects should be symmetric, got %v vs %v", gotAB, gotBA)
	}
}
