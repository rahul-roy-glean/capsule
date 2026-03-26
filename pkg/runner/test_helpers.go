package runner

// TestInjectRunner adds a runner to the manager's internal map.
// Exported for use by server-level integration tests in other packages.
func (m *Manager) TestInjectRunner(r *Runner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runners[r.ID] = r
}
