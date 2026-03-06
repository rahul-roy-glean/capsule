package github

import (
	"fmt"
	"strings"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

// ParseGitHubRepo extracts owner and repo name from various GitHub URL formats.
func ParseGitHubRepo(repoURL string) (owner, repoName string, err error) {
	s := repoURL

	// Strip schemes
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "git@")

	// Strip github.com prefix
	s = strings.TrimPrefix(s, "github.com/")
	s = strings.TrimPrefix(s, "github.com:")

	// Strip .git suffix
	s = strings.TrimSuffix(s, ".git")

	parts := strings.SplitN(s, "/", 3)
	if len(parts) < 2 {
		return "", "", fmt.Errorf("cannot parse owner/repo from: %s", repoURL)
	}

	return parts[0], parts[1], nil
}

// ExtractGitCloneArgs returns the repo URL and branch from a shell command that
// runs git clone, or empty strings if no such command is found.
// It scans shell-type commands for "git clone" invocations and extracts the URL
// (https:// or git@ prefix) and branch (-b flag) from the command string.
func ExtractGitCloneArgs(commands []snapshot.SnapshotCommand) (repoURL, branch string) {
	for _, cmd := range commands {
		if cmd.Type != "shell" {
			continue
		}
		// Look for "git clone" in any arg (typically the bash -c argument).
		for _, arg := range cmd.Args {
			if !strings.Contains(arg, "git clone") {
				continue
			}
			tokens := strings.Fields(arg)
			for i, tok := range tokens {
				// Extract URL
				if repoURL == "" && (strings.HasPrefix(tok, "https://") || strings.HasPrefix(tok, "git@")) {
					repoURL = tok
				}
				// Extract branch from -b flag
				if tok == "-b" && i+1 < len(tokens) {
					branch = tokens[i+1]
				}
			}
			if repoURL != "" {
				if branch == "" {
					branch = "main"
				}
				return repoURL, branch
			}
		}
	}
	return "", ""
}

// RepoFromCommands extracts the CI repository identifier (owner/name) from
// snapshot init commands. Returns "" if no repository can be determined.
func RepoFromCommands(commands []snapshot.SnapshotCommand) string {
	repoURL, _ := ExtractGitCloneArgs(commands)
	if repoURL == "" {
		return ""
	}
	owner, repoName, err := ParseGitHubRepo(repoURL)
	if err != nil {
		return ""
	}
	return owner + "/" + repoName
}
