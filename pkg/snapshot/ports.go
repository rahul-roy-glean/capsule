package snapshot

const (
	// ThawAgentHealthPort is the VM-internal port for the capsule-thaw-agent health/warmup server.
	// Moved from 8080 to avoid conflicts with user services running inside the VM.
	ThawAgentHealthPort = 10500

	// ThawAgentDebugPort is the VM-internal port for the capsule-thaw-agent debug server
	// (/alive, /progress, /logs, /exec endpoints).
	// Moved from 8081 to avoid conflicts with user services running inside the VM.
	ThawAgentDebugPort = 10501
)
