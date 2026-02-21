package repo

import (
	"net/url"
	"regexp"
	"strings"
)

var nonAlphanumDash = regexp.MustCompile(`[^a-z0-9-]+`)
var multiDash = regexp.MustCompile(`-{2,}`)

// Slug converts a repository URL or full name into a deterministic, URL-safe,
// lowercase slug suitable for use as a directory name and database key.
//
// Examples:
//
//	"github.com/askscio/scio"          → "askscio-scio"
//	"https://github.com/org/repo.git"  → "org-repo"
//	"org/repo"                         → "org-repo"
func Slug(repoURL string) string {
	s := repoURL

	// Strip common URL schemes
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "git@")

	// If it still looks like a URL, parse it
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil {
			s = u.Host + u.Path
		}
	}

	// Strip known hosting prefixes
	s = strings.TrimPrefix(s, "github.com/")
	s = strings.TrimPrefix(s, "github.com:")
	s = strings.TrimPrefix(s, "gitlab.com/")
	s = strings.TrimPrefix(s, "bitbucket.org/")

	// Strip .git suffix
	s = strings.TrimSuffix(s, ".git")

	// Lowercase
	s = strings.ToLower(s)

	// Replace path separators and other non-alphanumeric chars with dashes
	s = strings.ReplaceAll(s, "/", "-")
	s = nonAlphanumDash.ReplaceAllString(s, "-")

	// Collapse consecutive dashes
	s = multiDash.ReplaceAllString(s, "-")

	// Trim leading/trailing dashes
	s = strings.Trim(s, "-")

	if s == "" {
		s = "unknown"
	}

	return s
}
