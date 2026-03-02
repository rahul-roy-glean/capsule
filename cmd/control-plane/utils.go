package main

import (
	"fmt"
	"strings"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

// parseGitHubRepo extracts owner and repo name from various GitHub URL formats.
func parseGitHubRepo(repoURL string) (owner, repoName string, err error) {
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

// extractGitCloneArgs returns the repo URL and branch from a git-clone command,
// or empty strings if no git-clone command is found.
func extractGitCloneArgs(commands []snapshot.SnapshotCommand) (repoURL, branch string) {
	for _, cmd := range commands {
		if cmd.Type == "git-clone" {
			// Args convention: ["<repo-url>", "<branch>"] or just ["<repo-url>"]
			if len(cmd.Args) >= 1 {
				repoURL = cmd.Args[0]
			}
			if len(cmd.Args) >= 2 {
				branch = cmd.Args[1]
			}
			if branch == "" {
				branch = "main"
			}
			return repoURL, branch
		}
	}
	return "", ""
}
