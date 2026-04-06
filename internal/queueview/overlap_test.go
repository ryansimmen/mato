package queueview

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
		{"glob matches file", "internal/runner/*.go", "internal/runner/task.go", true},
		{"glob no match file", "internal/runner/*.go", "internal/queue/queue.go", false},
		{"doublestar matches nested", "**/*.go", "internal/runner/task.go", true},
		{"doublestar matches top-level", "**/*.go", "main.go", true},
		{"glob no match extension", "internal/runner/*.go", "internal/runner/task.txt", false},
		{"glob matches reverse", "internal/runner/task.go", "internal/runner/*.go", true},
		{"glob vs dir prefix overlapping", "internal/runner/*.go", "internal/runner/", true},
		{"glob vs dir prefix parent", "internal/runner/*.go", "internal/", true},
		{"glob vs dir prefix disjoint", "internal/runner/*.go", "pkg/", false},
		{"dir prefix vs glob overlapping", "internal/runner/", "internal/runner/*.go", true},
		{"doublestar vs any dir prefix", "**/*.go", "internal/", true},
		{"dir prefix vs doublestar", "internal/", "**/*.go", true},
		{"glob vs glob overlapping prefix", "internal/runner/*.go", "internal/runner/*_test.go", true},
		{"glob vs glob disjoint prefix", "internal/runner/*.go", "pkg/client/*.go", false},
		{"glob vs glob one doublestar", "**/*.go", "internal/runner/*.go", true},
		{"glob vs glob both doublestar", "**/*.go", "**/*_test.go", true},
		{"malformed glob same prefix", "internal/[bad", "internal/runner/task.go", true},
		{"malformed glob disjoint prefix", "internal/[bad", "pkg/client/http.go", false},
		{"malformed glob empty prefix", "[bad", "anything.go", true},
		{"glob trailing slash vs file under", "internal/*/", "internal/runner/task.go", true},
		{"glob trailing slash vs disjoint", "internal/*/", "pkg/client.go", false},
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
	pairs := [][2]string{{"foo.go", "bar.go"}, {"pkg/client/", "pkg/client/http.go"}, {"internal/runner/*.go", "internal/runner/task.go"}, {"**/*.go", "internal/runner/task.go"}, {"internal/runner/*.go", "internal/runner/"}, {"**/*.go", "internal/"}, {"internal/runner/*.go", "pkg/client/*.go"}, {"internal/runner/*.go", "internal/runner/*_test.go"}, {"**/*.go", "**/*_test.go"}, {"internal/*.go", "internal/r*.go"}, {"internal/[bad", "internal/runner/task.go"}, {"internal/[bad", "pkg/client.go"}, {"[bad", "anything.go"}, {"internal/*/", "internal/runner/task.go"}, {"internal/*/", "pkg/client.go"}, {"internal/[bad", "**/*.go"}, {"internal/[bad", "internal/runner/*.go"}, {"internal/[bad", "pkg/client/*.go"}, {"internal/*/", "**/*.go"}, {"internal/*/", "internal/runner/*.go"}, {"internal/*/", "pkg/client/*.go"}}
	for _, p := range pairs {
		ab := affectsMatch(p[0], p[1])
		ba := affectsMatch(p[1], p[0])
		if ab != ba {
			t.Errorf("asymmetric: affectsMatch(%q, %q)=%v but affectsMatch(%q, %q)=%v", p[0], p[1], ab, p[1], p[0], ba)
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
		{"exact match", []string{"foo.go", "bar.go"}, []string{"bar.go", "baz.go"}, []string{"bar.go"}},
		{"no overlap", []string{"foo.go"}, []string{"bar.go"}, nil},
		{"nil a", nil, []string{"foo.go"}, nil},
		{"nil b", []string{"foo.go"}, nil, nil},
		{"both nil", nil, nil, nil},
		{"empty strings filtered", []string{"", "foo.go"}, []string{"foo.go", ""}, []string{"foo.go"}},
		{"prefix in a matches file in b", []string{"pkg/client/"}, []string{"pkg/client/http.go"}, []string{"pkg/client/", "pkg/client/http.go"}},
		{"file in a matched by prefix in b", []string{"pkg/client/http.go"}, []string{"pkg/client/"}, []string{"pkg/client/", "pkg/client/http.go"}},
		{"prefix vs prefix nested", []string{"pkg/"}, []string{"pkg/client/"}, []string{"pkg/", "pkg/client/"}},
		{"non-overlapping prefixes", []string{"pkg/client/"}, []string{"pkg/server/"}, nil},
		{"mixed exact and prefix", []string{"README.md", "pkg/client/"}, []string{"pkg/client/http.go", "README.md"}, []string{"README.md", "pkg/client/", "pkg/client/http.go"}},
		{"duplicate in b", []string{"foo.go"}, []string{"foo.go", "foo.go"}, []string{"foo.go"}},
		{"all exact no prefix fast path", []string{"a.go", "b.go", "c.go"}, []string{"b.go", "c.go", "d.go"}, []string{"b.go", "c.go"}},
		{"broad prefix matches multiple", []string{"internal/"}, []string{"internal/queue/queue.go", "internal/merge/merge.go", "docs/readme.md"}, []string{"internal/", "internal/merge/merge.go", "internal/queue/queue.go"}},
		{"glob matches exact file", []string{"internal/runner/*.go"}, []string{"internal/runner/task.go"}, []string{"internal/runner/*.go", "internal/runner/task.go"}},
		{"glob no match", []string{"internal/runner/*.go"}, []string{"pkg/client/http.go"}, nil},
		{"glob vs prefix", []string{"internal/runner/*.go"}, []string{"internal/runner/"}, []string{"internal/runner/", "internal/runner/*.go"}},
		{"glob vs glob overlapping", []string{"internal/runner/*.go"}, []string{"internal/runner/*_test.go"}, []string{"internal/runner/*.go", "internal/runner/*_test.go"}},
		{"glob vs glob disjoint", []string{"internal/runner/*.go"}, []string{"pkg/client/*.go"}, nil},
		{"doublestar matches everything", []string{"**/*.go"}, []string{"internal/runner/task.go"}, []string{"**/*.go", "internal/runner/task.go"}},
		{"mixed exact glob and prefix", []string{"README.md", "internal/runner/*.go"}, []string{"internal/runner/task.go", "README.md"}, []string{"README.md", "internal/runner/*.go", "internal/runner/task.go"}},
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
