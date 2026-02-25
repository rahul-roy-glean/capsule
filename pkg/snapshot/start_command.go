package snapshot

// StartCommand describes a user service to run inside the microVM after snapshot restore.
// The thaw-agent starts the command, waits for the health check to pass, then
// forwards traffic from the host through to the service port.
type StartCommand struct {
	Command    []string `json:"command"`
	Port       int      `json:"port"`
	HealthPath string   `json:"health_path"`
}
