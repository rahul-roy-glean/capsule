package repo

import "testing"

func TestSlug(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"github.com/askscio/scio", "askscio-scio"},
		{"https://github.com/org/repo.git", "org-repo"},
		{"org/repo", "org-repo"},
		{"https://github.com/my-org/my-repo", "my-org-my-repo"},
		{"git@github.com:company/project.git", "company-project"},
		{"https://gitlab.com/group/subgroup/project", "group-subgroup-project"},
		{"simple-repo", "simple-repo"},
		{"UPPERCASE/Repo", "uppercase-repo"},
		{"", "unknown"},
		{"https://github.com/a/b.git", "a-b"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := Slug(tt.input)
			if got != tt.want {
				t.Errorf("Slug(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
