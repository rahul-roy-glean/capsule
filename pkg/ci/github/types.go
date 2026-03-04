package github

// RunnerTokenOpts contains options for getting a runner registration token.
type RunnerTokenOpts struct {
	Repo   string
	Org    string
	Labels []string
}

// RunnerInfo describes a runner for CI adapter callbacks.
type RunnerInfo struct {
	ID     string
	Name   string
	Repo   string
	Labels []string
}
