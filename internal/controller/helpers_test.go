package controller

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devenvv1 "github.com/tunnel4/tunnel4-operator/api/v1"
)

func TestBuildNamespace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		developer string
		branch    string
		want      string
	}{
		{
			name:      "simple branch",
			developer: "alice",
			branch:    "main",
			want:      "alice-main",
		},
		{
			name:      "branch with forward slash",
			developer: "alice",
			branch:    "feature/stripe",
			want:      "alice-feature-stripe",
		},
		{
			name:      "branch with underscore",
			developer: "bob",
			branch:    "fix_bug",
			want:      "bob-fix-bug",
		},
		{
			name:      "branch with dot",
			developer: "carol",
			branch:    "release.1.0",
			want:      "carol-release-1-0",
		},
		{
			name:      "branch uppercase is lowercased",
			developer: "dave",
			branch:    "Feature/My-Branch",
			want:      "dave-feature-my-branch",
		},
		{
			name:      "long name is truncated to 63 chars",
			developer: "x",
			branch:    strings.Repeat("y", 66), // "x-" + 66 y's = 68 chars
			want:      "x-" + strings.Repeat("y", 61),
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildNamespace(tt.developer, tt.branch)
			if got != tt.want {
				t.Errorf("buildNamespace(%q, %q) = %q, want %q", tt.developer, tt.branch, got, tt.want)
			}
			if len(got) > 63 {
				t.Errorf("buildNamespace(%q, %q) = %q (len=%d), exceeds 63-char Kubernetes limit",
					tt.developer, tt.branch, got, len(got))
			}
		})
	}
}

func TestSanitizeLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no special chars", "main", "main"},
		{"forward slash", "feature/stripe", "feature-stripe"},
		{"underscore", "fix_bug", "fix-bug"},
		{"uppercase", "Feature/Stripe", "feature-stripe"},
		{"mixed separators", "Feature/My_Branch", "feature-my-branch"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sanitizeLabel(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeLabel(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestContainsString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		slice []string
		s     string
		want  bool
	}{
		{"found in middle", []string{"a", "b", "c"}, "b", true},
		{"found at start", []string{"a", "b", "c"}, "a", true},
		{"found at end", []string{"a", "b", "c"}, "c", true},
		{"not found", []string{"a", "b", "c"}, "d", false},
		{"empty slice", []string{}, "a", false},
		{"nil slice", nil, "a", false},
		{"finalizer found", []string{"tunnel4.dev/finalizer"}, "tunnel4.dev/finalizer", true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := containsString(tt.slice, tt.s)
			if got != tt.want {
				t.Errorf("containsString(%v, %q) = %v, want %v", tt.slice, tt.s, got, tt.want)
			}
		})
	}
}

func TestRemoveString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		slice []string
		s     string
		want  []string
	}{
		{
			name:  "remove from middle",
			slice: []string{"a", "b", "c"},
			s:     "b",
			want:  []string{"a", "c"},
		},
		{
			name:  "remove first element",
			slice: []string{"a", "b", "c"},
			s:     "a",
			want:  []string{"b", "c"},
		},
		{
			name:  "remove last element",
			slice: []string{"a", "b", "c"},
			s:     "c",
			want:  []string{"a", "b"},
		},
		{
			name:  "element not present",
			slice: []string{"a", "b", "c"},
			s:     "d",
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "remove only element",
			slice: []string{"a"},
			s:     "a",
			want:  []string{},
		},
		{
			name:  "empty slice",
			slice: []string{},
			s:     "a",
			want:  []string{},
		},
		{
			name:  "remove finalizer from multi-finalizer slice",
			slice: []string{"other/finalizer", "tunnel4.dev/finalizer"},
			s:     "tunnel4.dev/finalizer",
			want:  []string{"other/finalizer"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := removeString(tt.slice, tt.s)
			if len(got) != len(tt.want) {
				t.Errorf("removeString(%v, %q) = %v (len=%d), want %v (len=%d)",
					tt.slice, tt.s, got, len(got), tt.want, len(tt.want))
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("removeString(%v, %q)[%d] = %q, want %q",
						tt.slice, tt.s, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestIsIdle(t *testing.T) {
	t.Parallel()
	r := &DevEnvironmentReconciler{}

	tests := []struct {
		name   string
		devEnv *devenvv1.DevEnvironment
		want   bool
	}{
		{
			name: "zero heartbeat is not idle",
			devEnv: &devenvv1.DevEnvironment{
				Status: devenvv1.DevEnvironmentStatus{
					LastHeartbeat: metav1.Time{},
				},
			},
			want: false,
		},
		{
			name: "heartbeat 1 hour ago is idle",
			devEnv: &devenvv1.DevEnvironment{
				Status: devenvv1.DevEnvironmentStatus{
					LastHeartbeat: metav1.Time{Time: time.Now().Add(-1 * time.Hour)},
				},
			},
			want: true,
		},
		{
			name: "heartbeat 1 minute ago is not idle",
			devEnv: &devenvv1.DevEnvironment{
				Status: devenvv1.DevEnvironmentStatus{
					LastHeartbeat: metav1.Time{Time: time.Now().Add(-1 * time.Minute)},
				},
			},
			want: false,
		},
		{
			name: "heartbeat 31 minutes ago exceeds idle timeout",
			devEnv: &devenvv1.DevEnvironment{
				Status: devenvv1.DevEnvironmentStatus{
					LastHeartbeat: metav1.Time{Time: time.Now().Add(-31 * time.Minute)},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := r.isIdle(tt.devEnv)
			if got != tt.want {
				t.Errorf("isIdle() = %v, want %v", got, tt.want)
			}
		})
	}
}
