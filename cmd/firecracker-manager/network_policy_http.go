package main

import (
	"encoding/json"
	"net/http"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/network"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/runner"
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
		}
		if policy != nil {
			resp["policy"] = policy
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
			writeJSONResponse(w, http.StatusOK, map[string]any{
				"success":   false,
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
