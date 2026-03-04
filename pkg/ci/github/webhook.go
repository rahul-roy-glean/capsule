package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/sirupsen/logrus"
)

// JobEnqueuer enqueues CI webhook jobs. Implemented by the control-plane's JobQueue.
type JobEnqueuer interface {
	EnqueueJob(ctx context.Context, event *WorkflowJobEvent) (string, error)
	UpdateJobInProgress(ctx context.Context, githubJobID int64, runnerName string) error
	CompleteJob(ctx context.Context, githubJobID int64, runnerName string) error
}

// RunnerReleaser releases runners. Implemented by the control-plane's Scheduler.
type RunnerReleaser interface {
	ReleaseRunner(ctx context.Context, runnerID string, destroy bool) error
}

// RunnerLookup looks up runners by name. Implemented by the control-plane's HostRegistry.
type RunnerLookup interface {
	GetRunnerByName(ctx context.Context, name string) (id string, err error)
}

// WebhookDeps holds the dependencies needed by the GitHubWebhookHandler.
type WebhookDeps struct {
	JobQueue       JobEnqueuer
	RunnerReleaser RunnerReleaser
	RunnerLookup   RunnerLookup
	WebhookSecret  string // GITHUB_WEBHOOK_SECRET; if empty, read from env
	Logger         *logrus.Logger
}

// GitHubWebhookHandler handles GitHub webhook events.
type GitHubWebhookHandler struct {
	deps          WebhookDeps
	webhookSecret string
	logger        *logrus.Entry
}

// NewGitHubWebhookHandler creates a new webhook handler.
func NewGitHubWebhookHandler(deps WebhookDeps) *GitHubWebhookHandler {
	secret := deps.WebhookSecret
	if secret == "" {
		secret = os.Getenv("GITHUB_WEBHOOK_SECRET")
	}
	logger := deps.Logger
	if logger == nil {
		logger = logrus.New()
	}
	return &GitHubWebhookHandler{
		deps:          deps,
		webhookSecret: secret,
		logger:        logger.WithField("component", "github-webhook"),
	}
}

// ServeHTTP implements http.Handler.
func (h *GitHubWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.HandleWebhook(w, r)
}

// WorkflowJobEvent represents a GitHub workflow_job webhook event.
type WorkflowJobEvent struct {
	Action      string `json:"action"`
	WorkflowJob struct {
		ID         int64    `json:"id"`
		RunID      int64    `json:"run_id"`
		Status     string   `json:"status"`
		Conclusion string   `json:"conclusion"`
		Name       string   `json:"name"`
		Labels     []string `json:"labels"`
		RunnerID   int64    `json:"runner_id"`
		RunnerName string   `json:"runner_name"`
		HeadBranch string   `json:"head_branch"`
		HeadSHA    string   `json:"head_sha"`
	} `json:"workflow_job"`
	Repository struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
	} `json:"repository"`
	Organization struct {
		Login string `json:"login"`
	} `json:"organization"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// HandleWebhook handles incoming GitHub webhooks.
func (h *GitHubWebhookHandler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.logger.WithError(err).Error("Failed to read request body")
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	if h.webhookSecret != "" {
		signature := r.Header.Get("X-Hub-Signature-256")
		if !h.verifySignature(body, signature) {
			h.logger.Warn("Invalid webhook signature")
			http.Error(w, "Invalid signature", http.StatusUnauthorized)
			return
		}
	}

	eventType := r.Header.Get("X-GitHub-Event")
	deliveryID := r.Header.Get("X-GitHub-Delivery")

	h.logger.WithFields(logrus.Fields{
		"event_type":  eventType,
		"delivery_id": deliveryID,
	}).Debug("Received webhook")

	switch eventType {
	case "workflow_job":
		h.handleWorkflowJob(r.Context(), body, w)
	case "ping":
		h.handlePing(w)
	default:
		h.logger.WithField("event_type", eventType).Debug("Ignoring unhandled event type")
		w.WriteHeader(http.StatusOK)
	}
}

func (h *GitHubWebhookHandler) verifySignature(body []byte, signature string) bool {
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	sig := strings.TrimPrefix(signature, "sha256=")
	mac := hmac.New(sha256.New, []byte(h.webhookSecret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sig), []byte(expected))
}

func (h *GitHubWebhookHandler) handleWorkflowJob(ctx context.Context, body []byte, w http.ResponseWriter) {
	var event WorkflowJobEvent
	if err := json.Unmarshal(body, &event); err != nil {
		h.logger.WithError(err).Error("Failed to parse workflow_job event")
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	h.logger.WithFields(logrus.Fields{
		"action":   event.Action,
		"job_id":   event.WorkflowJob.ID,
		"job_name": event.WorkflowJob.Name,
		"repo":     event.Repository.FullName,
		"branch":   event.WorkflowJob.HeadBranch,
		"labels":   event.WorkflowJob.Labels,
	}).Info("Handling workflow_job event")

	switch event.Action {
	case "queued":
		h.handleJobQueued(ctx, &event, w)
	case "in_progress":
		h.handleJobInProgress(ctx, &event, w)
	case "completed":
		h.handleJobCompleted(ctx, &event, w)
	default:
		h.logger.WithField("action", event.Action).Debug("Ignoring workflow_job action")
		w.WriteHeader(http.StatusOK)
	}
}

func (h *GitHubWebhookHandler) handleJobQueued(ctx context.Context, event *WorkflowJobEvent, w http.ResponseWriter) {
	if !h.shouldHandleJob(event) {
		h.logger.Debug("Job does not match our labels, ignoring")
		w.WriteHeader(http.StatusOK)
		return
	}

	h.logger.WithFields(logrus.Fields{
		"job_id": event.WorkflowJob.ID,
		"repo":   event.Repository.FullName,
	}).Info("Enqueuing job for allocation")

	if h.deps.JobQueue != nil {
		jobID, err := h.deps.JobQueue.EnqueueJob(ctx, event)
		if err != nil {
			h.logger.WithError(err).Error("Failed to enqueue job")
		} else {
			h.logger.WithField("job_id", jobID).Info("Job enqueued successfully")
		}
	}

	w.WriteHeader(http.StatusOK)
}

func (h *GitHubWebhookHandler) handleJobInProgress(ctx context.Context, event *WorkflowJobEvent, w http.ResponseWriter) {
	h.logger.WithFields(logrus.Fields{
		"job_id":      event.WorkflowJob.ID,
		"runner_name": event.WorkflowJob.RunnerName,
	}).Info("Job started running")

	if h.deps.JobQueue != nil {
		if err := h.deps.JobQueue.UpdateJobInProgress(ctx, event.WorkflowJob.ID, event.WorkflowJob.RunnerName); err != nil {
			h.logger.WithError(err).Warn("Failed to update job to in_progress")
		}
	}

	w.WriteHeader(http.StatusOK)
}

func (h *GitHubWebhookHandler) handleJobCompleted(ctx context.Context, event *WorkflowJobEvent, w http.ResponseWriter) {
	h.logger.WithFields(logrus.Fields{
		"job_id":      event.WorkflowJob.ID,
		"conclusion":  event.WorkflowJob.Conclusion,
		"runner_name": event.WorkflowJob.RunnerName,
	}).Info("Job completed")

	if h.deps.JobQueue != nil {
		if err := h.deps.JobQueue.CompleteJob(ctx, event.WorkflowJob.ID, event.WorkflowJob.RunnerName); err != nil {
			h.logger.WithError(err).Warn("Failed to complete job")
		}
	}

	if event.WorkflowJob.RunnerName != "" && h.deps.RunnerLookup != nil && h.deps.RunnerReleaser != nil {
		if runnerID, err := h.deps.RunnerLookup.GetRunnerByName(ctx, event.WorkflowJob.RunnerName); err == nil && runnerID != "" {
			if releaseErr := h.deps.RunnerReleaser.ReleaseRunner(ctx, runnerID, true); releaseErr != nil {
				h.logger.WithError(releaseErr).WithField("runner_id", runnerID).Warn("Failed to release runner")
			}
		}
	}

	w.WriteHeader(http.StatusOK)
}

func (h *GitHubWebhookHandler) shouldHandleJob(event *WorkflowJobEvent) bool {
	for _, label := range event.WorkflowJob.Labels {
		if label == "firecracker" || label == "self-hosted" {
			return true
		}
	}
	return false
}

func (h *GitHubWebhookHandler) handlePing(w http.ResponseWriter) {
	h.logger.Info("Received ping event")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("pong"))
}

// LabelsToMap converts a slice of labels to a map.
func LabelsToMap(labels []string) map[string]string {
	m := make(map[string]string)
	for _, label := range labels {
		m[label] = "true"
	}
	return m
}
