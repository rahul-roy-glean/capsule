package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/rahul-roy-glean/bazel-firecracker/api/proto/runner"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/authproxy"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/network"
	fcrotel "github.com/rahul-roy-glean/bazel-firecracker/pkg/otel"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/runner"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

// HostAgentServer implements the HostAgent gRPC service
type HostAgentServer struct {
	pb.UnimplementedHostAgentServer
	manager              *runner.Manager
	chunkedMgr           *runner.ChunkedManager
	sessionPauseHist     metric.Float64Histogram
	sessionPauseCounter  metric.Int64Counter
	sessionResumeHist    metric.Float64Histogram
	sessionResumeCounter metric.Int64Counter
	logger               *logrus.Entry
}

// NewHostAgentServer creates a HostAgentServer with chunked snapshot support
func NewHostAgentServer(mgr *runner.Manager, chunkedMgr *runner.ChunkedManager, logger *logrus.Logger) *HostAgentServer {
	return &HostAgentServer{
		manager:    mgr,
		chunkedMgr: chunkedMgr,
		logger:     logger.WithField("service", "host-agent"),
	}
}

// SetOTelInstruments attaches OTel instruments for session pause/resume metrics.
func (s *HostAgentServer) SetOTelInstruments(pauseHist metric.Float64Histogram, pauseCounter metric.Int64Counter, resumeHist metric.Float64Histogram, resumeCounter metric.Int64Counter) {
	s.sessionPauseHist = pauseHist
	s.sessionPauseCounter = pauseCounter
	s.sessionResumeHist = resumeHist
	s.sessionResumeCounter = resumeCounter
}

// AllocateRunner allocates a new runner
func (s *HostAgentServer) AllocateRunner(ctx context.Context, req *pb.AllocateRunnerRequest) (*pb.AllocateRunnerResponse, error) {
	s.logger.WithFields(logrus.Fields{
		"request_id": req.RequestId,
	}).Info("AllocateRunner request")

	if req.WorkloadKey == "" {
		return nil, status.Error(codes.InvalidArgument, "workload_key is required")
	}

	allocReq := runner.AllocateRequest{
		RequestID:           req.RequestId,
		Labels:              req.Labels,
		WorkloadKey:         req.WorkloadKey,
		SnapshotVersion:     req.SnapshotVersion,
		SessionID:           req.SessionId,
		TTLSeconds:          int(req.TtlSeconds),
		AutoPause:           req.AutoPause,
		NetworkPolicyPreset: req.NetworkPolicyPreset,
	}

	// Extract network policy from labels (control plane packs them here
	// because manually-added proto fields 17-18 aren't in the wire descriptor).
	if v, ok := req.Labels["_network_policy_preset"]; ok && v != "" {
		allocReq.NetworkPolicyPreset = v
	}
	if v, ok := req.Labels["_network_policy_json"]; ok && v != "" {
		req.NetworkPolicyJson = v
	}

	// Parse network policy JSON if provided
	if req.NetworkPolicyJson != "" {
		var np network.NetworkPolicy
		if err := json.Unmarshal([]byte(req.NetworkPolicyJson), &np); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid network_policy_json: %v", err)
		}
		allocReq.NetworkPolicy = &np
	}

	// Extract auth config from labels (control plane packs auth config here).
	if v, ok := req.Labels["_auth_config_json"]; ok && v != "" {
		var ac authproxy.AuthConfig
		if err := json.Unmarshal([]byte(v), &ac); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid _auth_config_json: %v", err)
		}
		allocReq.AuthConfig = &ac
	}

	if req.Resources != nil {
		allocReq.Resources = runner.Resources{
			VCPUs:    int(req.Resources.Vcpus),
			MemoryMB: int(req.Resources.MemoryMb),
			DiskGB:   int(req.Resources.DiskGb),
		}
	}
	if req.StartCommand != nil {
		allocReq.StartCommand = &snapshot.StartCommand{
			Command:    req.StartCommand.Command,
			Port:       int(req.StartCommand.Port),
			HealthPath: req.StartCommand.HealthPath,
			Env:        req.StartCommand.Env,
			RunAs:      req.StartCommand.RunAs,
		}
	}

	var r *runner.Runner
	var err error
	var resumed bool

	// Session-aware allocation: if session_id is provided, try to resume
	if allocReq.SessionID != "" {
		// Check if there's already a running runner for this session
		if existing := s.manager.FindRunnerBySessionID(allocReq.SessionID); existing != nil {
			if existing.State != runner.StateSuspended {
				return &pb.AllocateRunnerResponse{
					Runner:    runnerToProto(existing),
					SessionId: allocReq.SessionID,
				}, nil
			}
		}

		// Try to resume from session snapshot
		if s.manager.SessionExists(allocReq.SessionID) {
			resumeStart := time.Now()
			r, err = s.manager.ResumeFromSession(ctx, allocReq.SessionID, allocReq.WorkloadKey)
			resumeDuration := time.Since(resumeStart)

			if err == nil {
				resumed = true

				// Determine routing type: GCS-backed or local
				meta, _ := s.manager.GetSessionMetadata(allocReq.SessionID)
				routing := fcrotel.RoutingLocal
				if meta != nil && meta.GCSManifestPath != "" {
					routing = fcrotel.RoutingGCS
				}

				if s.sessionResumeHist != nil {
					s.sessionResumeHist.Record(ctx, resumeDuration.Seconds(), metric.WithAttributes(
						fcrotel.AttrRouting.String(routing),
					))
				}
				if s.sessionResumeCounter != nil {
					s.sessionResumeCounter.Add(ctx, 1, metric.WithAttributes(
						fcrotel.AttrResult.String(fcrotel.ResultSuccess),
						fcrotel.AttrRouting.String(routing),
					))
				}
			} else {
				s.logger.WithError(err).Warn("Failed to resume from session, falling back to fresh allocation")
				if s.sessionResumeCounter != nil {
					s.sessionResumeCounter.Add(ctx, 1, metric.WithAttributes(
						fcrotel.AttrResult.String(fcrotel.ResultFailure),
					))
				}
				err = nil // Reset error for fresh allocation
			}
		}
	}

	// Fresh allocation if not resumed from session
	if r == nil {
		r, err = s.chunkedMgr.AllocateRunnerChunked(ctx, allocReq)

		// Bind session_id to the new runner
		if err == nil && allocReq.SessionID != "" {
			r.SessionID = allocReq.SessionID
			r.LastExecAt = r.StartedAt
		}
		// Set TTL config from request
		if err == nil && allocReq.TTLSeconds > 0 {
			r.TTLSeconds = allocReq.TTLSeconds
			r.AutoPause = allocReq.AutoPause
			if r.LastExecAt.IsZero() {
				r.LastExecAt = r.StartedAt
			}
		}
	}

	if err != nil {
		s.logger.WithError(err).Error("Failed to allocate runner")
		return &pb.AllocateRunnerResponse{
			Error: err.Error(),
		}, nil
	}

	// Apply network policy if requested
	if allocReq.NetworkPolicyPreset != "" || allocReq.NetworkPolicy != nil {
		if policyErr := s.manager.ApplyNetworkPolicy(r.ID, allocReq); policyErr != nil {
			s.logger.WithError(policyErr).WithField("runner_id", r.ID).Warn("Failed to apply network policy (non-fatal)")
		}
	}

	return &pb.AllocateRunnerResponse{
		Runner:    runnerToProto(r),
		Resumed:   resumed,
		SessionId: r.SessionID,
	}, nil
}

// ReleaseRunner releases a runner
func (s *HostAgentServer) ReleaseRunner(ctx context.Context, req *pb.ReleaseRunnerRequest) (*pb.ReleaseRunnerResponse, error) {
	s.logger.WithFields(logrus.Fields{
		"runner_id":        req.RunnerId,
		"destroy":          req.Destroy,
		"finished_cleanly": req.FinishedCleanly,
	}).Info("ReleaseRunner request")

	// Don't save incremental for normal destroy - only for explicit snapshot saves
	err := s.chunkedMgr.ReleaseRunnerChunked(ctx, req.RunnerId, false)

	if err != nil {
		s.logger.WithError(err).Error("Failed to release runner")
		return &pb.ReleaseRunnerResponse{
			Success: false,
			Error:   err.Error(),
		}, nil
	}

	return &pb.ReleaseRunnerResponse{
		Success: true,
	}, nil
}

// GetHostStatus returns the host status
func (s *HostAgentServer) GetHostStatus(ctx context.Context, req *pb.GetHostStatusRequest) (*pb.HostStatus, error) {
	st := s.manager.GetStatus()

	resp := &pb.HostStatus{
		IdleRunners: int32(st.IdleRunners),
		BusyRunners: int32(st.BusyRunners),
	}

	return resp, nil
}

// ListRunners lists all runners
func (s *HostAgentServer) ListRunners(ctx context.Context, req *pb.ListRunnersRequest) (*pb.ListRunnersResponse, error) {
	var stateFilter runner.State
	switch req.StateFilter {
	case pb.RunnerState_RUNNER_STATE_IDLE:
		stateFilter = runner.StateIdle
	case pb.RunnerState_RUNNER_STATE_BUSY:
		stateFilter = runner.StateBusy
	case pb.RunnerState_RUNNER_STATE_INITIALIZING:
		stateFilter = runner.StateInitializing
	}

	runners := s.manager.ListRunners(stateFilter)

	var protoRunners []*pb.Runner
	for _, r := range runners {
		protoRunners = append(protoRunners, runnerToProto(r))
	}

	return &pb.ListRunnersResponse{
		Runners: protoRunners,
	}, nil
}

// GetRunner gets a specific runner
func (s *HostAgentServer) GetRunner(ctx context.Context, req *pb.GetRunnerRequest) (*pb.Runner, error) {
	r, err := s.manager.GetRunner(req.RunnerId)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}

	return runnerToProto(r), nil
}

func (s *HostAgentServer) QuarantineRunner(ctx context.Context, req *pb.QuarantineRunnerRequest) (*pb.QuarantineRunnerResponse, error) {
	s.logger.WithFields(logrus.Fields{
		"runner_id":    req.RunnerId,
		"block_egress": req.BlockEgress,
		"pause_vm":     req.PauseVm,
	}).Info("QuarantineRunner request")

	var blockEgress *bool
	if req.BlockEgress {
		v := true
		blockEgress = &v
	}
	var pauseVM *bool
	if req.PauseVm {
		v := true
		pauseVM = &v
	}

	dir, err := s.manager.QuarantineRunner(ctx, req.RunnerId, runner.QuarantineOptions{
		Reason:      req.Reason,
		BlockEgress: blockEgress,
		PauseVM:     pauseVM,
	})
	if err != nil {
		return &pb.QuarantineRunnerResponse{Success: false, Error: err.Error()}, nil
	}
	return &pb.QuarantineRunnerResponse{Success: true, QuarantineDir: dir}, nil
}

func (s *HostAgentServer) UnquarantineRunner(ctx context.Context, req *pb.UnquarantineRunnerRequest) (*pb.UnquarantineRunnerResponse, error) {
	s.logger.WithFields(logrus.Fields{
		"runner_id":      req.RunnerId,
		"unblock_egress": req.UnblockEgress,
		"resume_vm":      req.ResumeVm,
	}).Info("UnquarantineRunner request")

	var unblockEgress *bool
	if req.UnblockEgress {
		v := true
		unblockEgress = &v
	}
	var resumeVM *bool
	if req.ResumeVm {
		v := true
		resumeVM = &v
	}

	if err := s.manager.UnquarantineRunner(ctx, req.RunnerId, runner.UnquarantineOptions{
		UnblockEgress: unblockEgress,
		ResumeVM:      resumeVM,
	}); err != nil {
		return &pb.UnquarantineRunnerResponse{Success: false, Error: err.Error()}, nil
	}
	return &pb.UnquarantineRunnerResponse{Success: true}, nil
}

// PauseRunner pauses a runner and creates a session snapshot
func (s *HostAgentServer) PauseRunner(ctx context.Context, req *pb.PauseRunnerRequest) (*pb.PauseRunnerResponse, error) {
	s.logger.WithField("runner_id", req.RunnerId).Info("PauseRunner request")

	start := time.Now()
	result, err := s.manager.PauseRunner(ctx, req.RunnerId)
	duration := time.Since(start)

	if err != nil {
		s.logger.WithError(err).Error("Failed to pause runner")
		if s.sessionPauseCounter != nil {
			s.sessionPauseCounter.Add(ctx, 1, metric.WithAttributes(
				fcrotel.AttrResult.String(fcrotel.ResultFailure),
			))
		}
		return &pb.PauseRunnerResponse{
			Success: false,
			Error:   err.Error(),
		}, nil
	}

	if s.sessionPauseHist != nil {
		s.sessionPauseHist.Record(ctx, duration.Seconds())
	}
	if s.sessionPauseCounter != nil {
		s.sessionPauseCounter.Add(ctx, 1, metric.WithAttributes(
			fcrotel.AttrResult.String(fcrotel.ResultSuccess),
		))
	}

	return &pb.PauseRunnerResponse{
		Success:           true,
		SessionId:         result.SessionID,
		SnapshotSizeBytes: result.SnapshotSizeBytes,
		Layer:             int32(result.Layer),
	}, nil
}

// ResumeRunner resumes a runner from a session snapshot
func (s *HostAgentServer) ResumeRunner(ctx context.Context, req *pb.ResumeRunnerRequest) (*pb.ResumeRunnerResponse, error) {
	s.logger.WithFields(logrus.Fields{
		"session_id":   req.SessionId,
		"workload_key": req.WorkloadKey,
	}).Info("ResumeRunner request")

	r, err := s.manager.ResumeFromSession(ctx, req.SessionId, req.WorkloadKey)
	if err != nil {
		s.logger.WithError(err).Error("Failed to resume runner")
		return &pb.ResumeRunnerResponse{
			Error: err.Error(),
		}, nil
	}

	return &pb.ResumeRunnerResponse{
		Runner:             runnerToProto(r),
		ResumedFromSession: true,
	}, nil
}

// runnerToProto converts a runner to protobuf
func runnerToProto(r *runner.Runner) *pb.Runner {
	state := pb.RunnerState_RUNNER_STATE_UNSPECIFIED
	switch r.State {
	case runner.StateCold:
		state = pb.RunnerState_RUNNER_STATE_COLD
	case runner.StateBooting:
		state = pb.RunnerState_RUNNER_STATE_BOOTING
	case runner.StateInitializing:
		state = pb.RunnerState_RUNNER_STATE_INITIALIZING
	case runner.StateIdle:
		state = pb.RunnerState_RUNNER_STATE_IDLE
	case runner.StateBusy:
		state = pb.RunnerState_RUNNER_STATE_BUSY
	case runner.StateDraining:
		state = pb.RunnerState_RUNNER_STATE_DRAINING
	case runner.StateQuarantined:
		state = pb.RunnerState_RUNNER_STATE_QUARANTINED
	case runner.StateRetiring:
		state = pb.RunnerState_RUNNER_STATE_RETIRING
	case runner.StateTerminated:
		state = pb.RunnerState_RUNNER_STATE_TERMINATED
	case runner.StatePaused:
		state = pb.RunnerState_RUNNER_STATE_PAUSED
	case runner.StatePausing:
		state = pb.RunnerState_RUNNER_STATE_PAUSING
	case runner.StateSuspended:
		state = pb.RunnerState_RUNNER_STATE_SUSPENDED
	}

	proto := &pb.Runner{
		Id:              r.ID,
		HostId:          r.HostID,
		State:           state,
		JobId:           r.JobID,
		SnapshotVersion: r.SnapshotVersion,
		CreatedAt:       timestamppb.New(r.CreatedAt),
		Resources: &pb.Resources{
			Vcpus:    int32(r.Resources.VCPUs),
			MemoryMb: int32(r.Resources.MemoryMB),
			DiskGb:   int32(r.Resources.DiskGB),
		},
	}

	if r.InternalIP != nil {
		proto.InternalIp = r.InternalIP.String()
	}

	if !r.StartedAt.IsZero() {
		proto.StartedAt = timestamppb.New(r.StartedAt)
	}

	return proto
}
