package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/capsule/pkg/network"
	"github.com/rahul-roy-glean/capsule/pkg/runner"
)

func getNetworkPolicyHandler(mgr *runner.Manager, logger *logrus.Logger) http.HandlerFunc {
	log := logger.WithField("component", "http-network-policy")
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		runnerID := r.URL.Query().Get("runner_id")
		if runnerID == "" {
			http.Error(w, "missing runner_id", http.StatusBadRequest)
			return
		}

		policy, version, err := mgr.GetNetworkPolicy(runnerID)
		if err != nil {
			log.WithError(err).WithField("runner_id", runnerID).Warn("Get network policy failed")
			writeJSONResponse(w, http.StatusNotFound, map[string]any{
				"error":     err.Error(),
				"runner_id": runnerID,
			})
			return
		}

		resp := map[string]any{
			"runner_id": runnerID,
			"version":   version,
			"policy":    policy, // null when no policy (consistent shape)
		}

		writeJSONResponse(w, http.StatusOK, resp)
	}
}

func updateNetworkPolicyHandler(mgr *runner.Manager, logger *logrus.Logger) http.HandlerFunc {
	log := logger.WithField("component", "http-network-policy")
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		runnerID := r.URL.Query().Get("runner_id")
		if runnerID == "" {
			http.Error(w, "missing runner_id", http.StatusBadRequest)
			return
		}

		var body struct {
			Policy *network.NetworkPolicy `json:"policy"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if body.Policy == nil {
			http.Error(w, "policy is required", http.StatusBadRequest)
			return
		}

		if err := mgr.UpdateNetworkPolicy(runnerID, body.Policy); err != nil {
			log.WithError(err).WithField("runner_id", runnerID).Warn("Update network policy failed")

			// Validation errors → 400; operational errors → 500
			status := http.StatusInternalServerError
			code := "POLICY_UPDATE_FAILED"
			if strings.Contains(err.Error(), "invalid network policy:") {
				status = http.StatusBadRequest
				code = "INVALID_POLICY"
			} else if strings.Contains(err.Error(), "runner not found") {
				status = http.StatusNotFound
				code = "RUNNER_NOT_FOUND"
			}

			writeJSONResponse(w, status, map[string]any{
				"success":   false,
				"code":      code,
				"error":     err.Error(),
				"runner_id": runnerID,
			})
			return
		}

		_, version, _ := mgr.GetNetworkPolicy(runnerID)
		writeJSONResponse(w, http.StatusOK, map[string]any{
			"success":   true,
			"runner_id": runnerID,
			"version":   version,
		})
	}
}
