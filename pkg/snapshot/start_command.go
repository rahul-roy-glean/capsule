package snapshot

// StartCommand describes a user service to run inside the microVM after snapshot restore.
// The capsule-thaw-agent starts the command, waits for the health check to pass, then
// forwards traffic from the host through to the service port.
//
// SOURCE OF TRUTH for the StartCommand struct shape.
// Inline copies exist (with omitempty JSON tags for MMDS serialisation) in:
//   - cmd/capsule-thaw-agent/main.go   — MMDSData.Latest.StartCommand (×2: waitForMMDS + fetchMMDSData)
//   - pkg/runner/types.go      — MMDSData.Latest.StartCommand
//
// Keep all copies in sync when adding/removing fields.
type StartCommand struct {
	Command    []string          `json:"command"`
	Port       int               `json:"port"`
	HealthPath string            `json:"health_path"`
	Env        map[string]string `json:"env,omitempty"`
	RunAs      string            `json:"run_as,omitempty"`
}
