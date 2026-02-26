package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
)

// JobQueue manages job lifecycle from queued through completion with retry.
type JobQueue struct {
	db           *sql.DB
	scheduler    *Scheduler
	hostRegistry *HostRegistry
	logger       *logrus.Entry
}

// NewJobQueue creates a new JobQueue.
func NewJobQueue(db *sql.DB, scheduler *Scheduler, hostRegistry *HostRegistry, logger *logrus.Logger) *JobQueue {
	return &JobQueue{
		db:           db,
		scheduler:    scheduler,
		hostRegistry: hostRegistry,
		logger:       logger.WithField("component", "job-queue"),
	}
}

// EnqueueJob inserts a new job row from a webhook event and returns the job ID.
func (jq *JobQueue) EnqueueJob(ctx context.Context, event *WorkflowJobEvent) (string, error) {
	labelsJSON, err := json.Marshal(event.WorkflowJob.Labels)
	if err != nil {
		return "", fmt.Errorf("failed to marshal labels: %w", err)
	}

	var jobID string
	err = jq.db.QueryRowContext(ctx, `
		INSERT INTO jobs (github_workflow_run_id, github_job_id, repo, branch, commit_sha, status, labels)
		VALUES ($1, $2, $3, $4, $5, 'queued', $6)
		RETURNING id
	`, event.WorkflowJob.RunID, event.WorkflowJob.ID, event.Repository.FullName,
		event.WorkflowJob.HeadBranch, event.WorkflowJob.HeadSHA, labelsJSON).Scan(&jobID)
	if err != nil {
		return "", fmt.Errorf("failed to insert job: %w", err)
	}

	jq.logger.WithFields(logrus.Fields{
		"job_id":        jobID,
		"github_job_id": event.WorkflowJob.ID,
		"repo":          event.Repository.FullName,
	}).Info("Job enqueued")

	return jobID, nil
}

// jobRetryLoop periodically attempts to allocate runners for queued jobs.
func (jq *JobQueue) jobRetryLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			jq.processQueuedJobs(ctx)
		}
	}
}

func (jq *JobQueue) processQueuedJobs(ctx context.Context) {
	rows, err := jq.db.QueryContext(ctx, `
		SELECT id, github_workflow_run_id, github_job_id, repo, branch, commit_sha, labels, queued_at
		FROM jobs WHERE status='queued' ORDER BY queued_at LIMIT 10
	`)
	if err != nil {
		jq.logger.WithError(err).Error("Failed to query queued jobs")
		return
	}
	defer rows.Close()

	for rows.Next() {
		var jobID, repo, branch, commitSHA string
		var githubRunID, githubJobID int64
		var labelsJSON []byte
		var queuedAt time.Time

		if err := rows.Scan(&jobID, &githubRunID, &githubJobID, &repo, &branch, &commitSHA, &labelsJSON, &queuedAt); err != nil {
			jq.logger.WithError(err).Error("Failed to scan job row")
			continue
		}

		// Check if job has been queued too long
		if time.Since(queuedAt) > 5*time.Minute {
			jq.logger.WithFields(logrus.Fields{
				"job_id":        jobID,
				"github_job_id": githubJobID,
				"queued_since":  queuedAt,
			}).Error("Job exceeded queue timeout, marking failed")

			_, _ = jq.db.ExecContext(ctx, `UPDATE jobs SET status='failed' WHERE id=$1`, jobID)
			continue
		}

		// Look up workload_key from snapshot_configs by matching a git-clone command for this repo
		workloadKey := lookupWorkloadKeyForRepo(jq.db, repo)

		// Attempt allocation
		req := AllocateRunnerRequest{
			RequestID:   fmt.Sprintf("gh-%d", githubJobID),
			WorkloadKey: workloadKey,
			Labels:      labelsToMap(parseLabels(labelsJSON)),
		}

		resp, err := jq.scheduler.AllocateRunner(ctx, req)
		if err != nil {
			jq.logger.WithError(err).WithFields(logrus.Fields{
				"job_id":        jobID,
				"github_job_id": githubJobID,
			}).Debug("Allocation attempt failed, will retry")
			continue
		}

		// Update job to assigned
		_, err = jq.db.ExecContext(ctx, `
			UPDATE jobs SET status='assigned', runner_id=$2 WHERE id=$1
		`, jobID, resp.RunnerID)
		if err != nil {
			jq.logger.WithError(err).Error("Failed to update job status to assigned")
			continue
		}

		jq.logger.WithFields(logrus.Fields{
			"job_id":    jobID,
			"runner_id": resp.RunnerID,
			"host_id":   resp.HostID,
		}).Info("Job assigned to runner")
	}
}

// CompleteJob marks a job as completed.
func (jq *JobQueue) CompleteJob(ctx context.Context, githubJobID int64, runnerName string) error {
	result, err := jq.db.ExecContext(ctx, `
		UPDATE jobs SET status='completed', completed_at=NOW() WHERE github_job_id=$1 AND status IN ('assigned','in_progress')
	`, githubJobID)
	if err != nil {
		return fmt.Errorf("failed to complete job: %w", err)
	}

	rows, _ := result.RowsAffected()
	jq.logger.WithFields(logrus.Fields{
		"github_job_id": githubJobID,
		"runner_name":   runnerName,
		"rows_affected": rows,
	}).Info("Job completed")

	return nil
}

// UpdateJobInProgress marks a job as in_progress.
func (jq *JobQueue) UpdateJobInProgress(ctx context.Context, githubJobID int64, runnerName string) error {
	_, err := jq.db.ExecContext(ctx, `
		UPDATE jobs SET status='in_progress', started_at=NOW() WHERE github_job_id=$1 AND status='assigned'
	`, githubJobID)
	if err != nil {
		return fmt.Errorf("failed to update job to in_progress: %w", err)
	}

	jq.logger.WithFields(logrus.Fields{
		"github_job_id": githubJobID,
		"runner_name":   runnerName,
	}).Info("Job in progress")

	return nil
}

// parseLabels parses a JSON label array back into a string slice.
func parseLabels(labelsJSON []byte) []string {
	var labels []string
	_ = json.Unmarshal(labelsJSON, &labels)
	return labels
}

// lookupWorkloadKeyForRepo finds the workload_key in snapshot_configs whose commands
// contain a git-clone arg matching the given repo name. Returns "" if not found.
func lookupWorkloadKeyForRepo(db *sql.DB, repoFullName string) string {
	if db == nil {
		return ""
	}
	rows, err := db.Query(`SELECT workload_key, commands FROM snapshot_configs`)
	if err != nil {
		return ""
	}
	defer rows.Close()

	for rows.Next() {
		var workloadKey, commandsJSON string
		if err := rows.Scan(&workloadKey, &commandsJSON); err != nil {
			continue
		}
		// If any command arg contains the repo name, use this workload_key
		if commandsJSON != "" && containsRepo(commandsJSON, repoFullName) {
			return workloadKey
		}
	}
	return ""
}

func containsRepo(commandsJSON, repoFullName string) bool {
	return len(repoFullName) > 0 && len(commandsJSON) > 0 &&
		(containsString(commandsJSON, repoFullName) ||
			containsString(commandsJSON, repoFullName+".git"))
}

func containsString(s, substr string) bool {
	return len(substr) > 0 && len(s) >= len(substr) &&
		func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}()
}
