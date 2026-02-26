package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/github"
)

var (
	configFile  = flag.String("config", "", "Path to git-cache config file (YAML/JSON)")
	outputDir   = flag.String("output-dir", "/tmp/git-cache-build", "Output directory for build artifacts")
	gcsBucket   = flag.String("gcs-bucket", "", "GCS bucket for git-cache upload")
	imageSizeGB = flag.Int("image-size-gb", 100, "Size of git-cache.img in GB")
	githubToken = flag.String("github-token", "", "GitHub token for private repo access (or use GITHUB_TOKEN env)")
	logLevel    = flag.String("log-level", "info", "Log level")
	dryRun      = flag.Bool("dry-run", false, "Build locally but don't upload to GCS")
	gcsPrefix   = flag.String("gcs-prefix", "v1", "Top-level prefix for all GCS paths (e.g. 'v1'). Set to empty string to disable.")

	// GitHub App authentication (alternative to --github-token for production)
	githubAppID     = flag.String("github-app-id", "", "GitHub App ID for private repo access")
	githubAppSecret = flag.String("github-app-secret", "", "GCP Secret Manager secret name containing GitHub App private key")
	gcpProject      = flag.String("gcp-project", "", "GCP project for Secret Manager (defaults to metadata project)")

	// Inline repo specification (alternative to config file)
	repos = flag.String("repos", "", "Comma-separated repo specs: github.com/org/repo:dirname,...")
)

// GitCacheConfig defines the repos to include in the git-cache
type GitCacheConfig struct {
	Repos []RepoConfig `json:"repos" yaml:"repos"`
}

// RepoConfig defines a single repo to cache
type RepoConfig struct {
	// URL is the full repo URL (e.g., github.com/askscio/scio)
	URL string `json:"url" yaml:"url"`
	// Name is the directory name inside the cache (e.g., scio)
	Name string `json:"name" yaml:"name"`
	// Branch to checkout (default: default branch)
	Branch string `json:"branch,omitempty" yaml:"branch,omitempty"`
	// Depth for shallow clone (0 = full clone, recommended for reference cloning)
	Depth int `json:"depth,omitempty" yaml:"depth,omitempty"`
}

// GitCacheMetadata records what's in the cache (for freshness checking)
type GitCacheMetadata struct {
	Version   string                `json:"version"`
	BuildTime time.Time             `json:"build_time"`
	Repos     map[string]RepoStatus `json:"repos"`
	ImageSize int64                 `json:"image_size_bytes"`
}

// RepoStatus tracks the state of a cached repo
type RepoStatus struct {
	URL       string    `json:"url"`
	Branch    string    `json:"branch"`
	CommitSHA string    `json:"commit_sha"`
	ClonedAt  time.Time `json:"cloned_at"`
}

func main() {
	flag.Parse()

	// Setup logger
	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	level, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)

	log := logger.WithField("component", "git-cache-builder")
	log.Info("Starting git-cache-builder")

	ctx := context.Background()

	// Get GitHub token (prefer GitHub App, fall back to PAT)
	token := *githubToken
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}

	// If GitHub App credentials are provided, use them instead
	if *githubAppID != "" && *githubAppSecret != "" {
		project := *gcpProject
		if project == "" {
			// Try to get from metadata
			project = os.Getenv("GCP_PROJECT")
		}
		if project == "" {
			log.Fatal("--gcp-project is required when using GitHub App authentication")
		}

		log.WithFields(logrus.Fields{
			"app_id":      *githubAppID,
			"secret":      *githubAppSecret,
			"gcp_project": project,
		}).Info("Using GitHub App for authentication")

		tokenClient, err := github.NewTokenClient(ctx, *githubAppID, *githubAppSecret, project)
		if err != nil {
			log.WithError(err).Fatal("Failed to create GitHub App token client")
		}

		installToken, err := tokenClient.GetInstallationToken(ctx)
		if err != nil {
			log.WithError(err).Fatal("Failed to get GitHub App installation token")
		}

		token = installToken
		log.Info("Successfully obtained GitHub App installation token")
	}

	// Parse config
	config, err := parseConfig(*configFile, *repos)
	if err != nil {
		log.WithError(err).Fatal("Failed to parse config")
	}

	if len(config.Repos) == 0 {
		log.Fatal("No repos configured. Use --config or --repos")
	}

	log.WithField("repo_count", len(config.Repos)).Info("Loaded config")

	// Create output directory
	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		log.WithError(err).Fatal("Failed to create output directory")
	}

	// Clone repos
	cloneDir := filepath.Join(*outputDir, "repos")
	if err := os.MkdirAll(cloneDir, 0755); err != nil {
		log.WithError(err).Fatal("Failed to create clone directory")
	}

	metadata := GitCacheMetadata{
		Version:   fmt.Sprintf("v%s", time.Now().Format("20060102-150405")),
		BuildTime: time.Now(),
		Repos:     make(map[string]RepoStatus),
	}

	for _, repo := range config.Repos {
		repoLog := log.WithFields(logrus.Fields{
			"repo": repo.URL,
			"name": repo.Name,
		})

		repoLog.Info("Cloning repository")

		status, err := cloneRepo(repo, cloneDir, token, repoLog)
		if err != nil {
			repoLog.WithError(err).Error("Failed to clone repo")
			continue
		}

		metadata.Repos[repo.Name] = *status
		repoLog.WithField("commit", status.CommitSHA).Info("Repository cloned")
	}

	if len(metadata.Repos) == 0 {
		log.Fatal("No repos cloned successfully")
	}

	// Create ext4 image
	imagePath := filepath.Join(*outputDir, "git-cache.img")
	log.WithFields(logrus.Fields{
		"path":    imagePath,
		"size_gb": *imageSizeGB,
	}).Info("Creating git-cache image")

	if err := createGitCacheImage(imagePath, cloneDir, *imageSizeGB, log); err != nil {
		log.WithError(err).Fatal("Failed to create git-cache image")
	}

	// Get image size
	if info, err := os.Stat(imagePath); err == nil {
		metadata.ImageSize = info.Size()
	}

	// Write metadata
	metadataPath := filepath.Join(*outputDir, "git-cache-metadata.json")
	metadataBytes, _ := json.MarshalIndent(metadata, "", "  ")
	if err := os.WriteFile(metadataPath, metadataBytes, 0644); err != nil {
		log.WithError(err).Warn("Failed to write metadata file")
	}

	log.WithFields(logrus.Fields{
		"version":    metadata.Version,
		"repos":      len(metadata.Repos),
		"image_size": metadata.ImageSize,
	}).Info("Git-cache image created")

	// Upload to GCS
	if !*dryRun && *gcsBucket != "" {
		log.Info("Uploading to GCS...")
		if err := uploadToGCS(context.Background(), *gcsBucket, *outputDir, metadata.Version, log); err != nil {
			log.WithError(err).Fatal("Failed to upload to GCS")
		}
		log.WithField("gcs_path", fmt.Sprintf("gs://%s/%s/", *gcsBucket, gcsPath(*gcsPrefix, "git-cache"))).Info("Upload complete")
	} else if *dryRun {
		log.Info("Dry-run mode, skipping GCS upload")
	}

	log.Info("Git-cache build complete!")
}

func parseConfig(configFile, reposFlag string) (*GitCacheConfig, error) {
	config := &GitCacheConfig{}

	// Parse from config file if provided
	if configFile != "" {
		data, err := os.ReadFile(configFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}
		if err := json.Unmarshal(data, config); err != nil {
			return nil, fmt.Errorf("failed to parse config: %w", err)
		}
	}

	// Parse from --repos flag (overrides/adds to config file)
	if reposFlag != "" {
		for _, spec := range strings.Split(reposFlag, ",") {
			spec = strings.TrimSpace(spec)
			if spec == "" {
				continue
			}

			parts := strings.SplitN(spec, ":", 2)
			url := strings.TrimSpace(parts[0])
			name := filepath.Base(url)
			if len(parts) == 2 {
				name = strings.TrimSpace(parts[1])
			}

			config.Repos = append(config.Repos, RepoConfig{
				URL:  url,
				Name: name,
			})
		}
	}

	return config, nil
}

func cloneRepo(repo RepoConfig, baseDir, token string, log *logrus.Entry) (*RepoStatus, error) {
	targetDir := filepath.Join(baseDir, repo.Name)

	// Build clone URL with auth
	cloneURL := "https://" + repo.URL
	if token != "" {
		cloneURL = fmt.Sprintf("https://x-access-token:%s@%s", token, repo.URL)
	}

	// Remove existing if present
	os.RemoveAll(targetDir)

	// Build clone command
	args := []string{"clone"}

	// Use shallow clone if depth specified, otherwise full clone (better for reference cloning)
	if repo.Depth > 0 {
		args = append(args, "--depth", fmt.Sprintf("%d", repo.Depth))
	}

	if repo.Branch != "" {
		args = append(args, "--branch", repo.Branch)
	}

	args = append(args, cloneURL, targetDir)

	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git clone failed: %s: %w", string(output), err)
	}

	// Fetch all branches (for reference cloning to work across branches)
	fetchCmd := exec.Command("git", "-C", targetDir, "fetch", "--all")
	fetchCmd.Run() // Best effort

	// Get current commit SHA
	commitCmd := exec.Command("git", "-C", targetDir, "rev-parse", "HEAD")
	commitOut, err := commitCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get commit SHA: %w", err)
	}

	// Get current branch
	branchCmd := exec.Command("git", "-C", targetDir, "rev-parse", "--abbrev-ref", "HEAD")
	branchOut, _ := branchCmd.Output()
	branch := strings.TrimSpace(string(branchOut))
	if branch == "" {
		branch = repo.Branch
	}

	return &RepoStatus{
		URL:       repo.URL,
		Branch:    branch,
		CommitSHA: strings.TrimSpace(string(commitOut)),
		ClonedAt:  time.Now(),
	}, nil
}

func createGitCacheImage(imagePath, sourceDir string, sizeGB int, log *logrus.Entry) error {
	// Create sparse image file
	if err := exec.Command("truncate", "-s", fmt.Sprintf("%dG", sizeGB), imagePath).Run(); err != nil {
		return fmt.Errorf("truncate failed: %w", err)
	}

	// Create ext4 filesystem
	if output, err := exec.Command("mkfs.ext4", "-F", "-L", "GIT_CACHE", imagePath).CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 failed: %s: %w", string(output), err)
	}

	// Mount and copy
	mountPoint := filepath.Join(filepath.Dir(imagePath), "mnt-git-cache")
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		return fmt.Errorf("failed to create mount point: %w", err)
	}
	defer os.RemoveAll(mountPoint)

	// Mount (requires root)
	if output, err := exec.Command("mount", "-o", "loop", imagePath, mountPoint).CombinedOutput(); err != nil {
		return fmt.Errorf("mount failed: %s: %w", string(output), err)
	}
	defer exec.Command("umount", mountPoint).Run()

	// Copy repos into image
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return fmt.Errorf("failed to read source dir: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		srcPath := filepath.Join(sourceDir, entry.Name())
		dstPath := filepath.Join(mountPoint, entry.Name())

		log.WithFields(logrus.Fields{
			"src": srcPath,
			"dst": dstPath,
		}).Debug("Copying repo to image")

		// Use cp -a to preserve permissions, symlinks, etc.
		if output, err := exec.Command("cp", "-a", srcPath, dstPath).CombinedOutput(); err != nil {
			log.WithError(err).WithField("output", string(output)).Warn("Failed to copy repo")
		}
	}

	// Set permissions
	exec.Command("chmod", "-R", "755", mountPoint).Run()

	// Sync to ensure all writes are flushed
	exec.Command("sync").Run()

	return nil
}

func gcsPath(prefix, path string) string {
	if prefix == "" {
		return path
	}
	return prefix + "/" + path
}

func uploadToGCS(ctx context.Context, bucket, sourceDir, version string, log *logrus.Entry) error {
	// Upload image to versioned path
	imagePath := filepath.Join(sourceDir, "git-cache.img")
	metadataPath := filepath.Join(sourceDir, "git-cache-metadata.json")

	// Upload to versioned location
	versionedPath := fmt.Sprintf("gs://%s/%s/", bucket, gcsPath(*gcsPrefix, "git-cache/"+version))
	if err := gsutilCopy(imagePath, versionedPath+"git-cache.img", log); err != nil {
		return err
	}
	if err := gsutilCopy(metadataPath, versionedPath+"metadata.json", log); err != nil {
		return err
	}

	return nil
}

func gsutilCopy(src, dst string, log *logrus.Entry) error {
	log.WithFields(logrus.Fields{
		"src": src,
		"dst": dst,
	}).Debug("Uploading to GCS")

	cmd := exec.Command("gcloud", "storage", "cp", src, dst)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
