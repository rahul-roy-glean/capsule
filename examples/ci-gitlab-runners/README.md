# Example: GitLab CI Runners

This example validates the claim that `IntegrationName` and `ci.Adapter` are
unnecessary. GitLab CI works identically to GitHub Actions — the only difference
is the binary name and registration flags.

## Why this proves `IntegrationName` is unnecessary

The old platform had:

```go
switch ciSystem {
case "github-actions":
    registerGitHubRunner(mmdsData)
case "gitlab":
    // Would need to add this case
case "buildkite":
    // And this one
}
```

With the generic approach, there is no switch statement. The snapshot contains
the CI runner binary. The `start_command` runs it with the right flags. The
platform injects `${ci_runner_token}` via MMDS. Done.

## Side-by-side comparison

### GitHub Actions
```yaml
start_command:
  command: ["/home/runner/config.sh", "--url", "https://github.com/myorg/myrepo",
            "--token", "${CI_RUNNER_TOKEN}", "--ephemeral"]
```

### GitLab CI
```yaml
start_command:
  command: ["gitlab-runner", "run", "--working-directory", "/workspace",
            "--url", "https://gitlab.com", "--token", "${CI_RUNNER_TOKEN}"]
```

### Buildkite
```yaml
start_command:
  command: ["buildkite-agent", "start", "--token", "${CI_RUNNER_TOKEN}",
            "--name", "firecracker-%n"]
```

### Jenkins
```yaml
start_command:
  command: ["java", "-jar", "/opt/jenkins/agent.jar",
            "-url", "https://jenkins.internal",
            "-secret", "${CI_RUNNER_TOKEN}", "-name", "fc-%n"]
```

All four CI systems work with the same platform code. No `ci.Adapter`, no
`IntegrationName`, no `switch` statement, no interface.

## What about drain (label removal)?

GitHub Actions drain removes labels via the GitHub API so no new jobs are
scheduled. This is a GitHub-specific API call. It doesn't need an interface —
it needs a nilable `*cigithub.Client`:

```go
if githubClient != nil {
    githubClient.RemoveLabels(ctx, runners)
}
```

GitLab and Buildkite don't need label removal — they use queue-based scheduling.
The `ci.Adapter.OnDrain()` method was designed for GitHub's polling model. Making
it an interface method forces all adapters to implement a method that only GitHub
needs.

## Onboard

```bash
cp examples/ci-gitlab-runners/onboard.yaml my-gitlab.yaml
# Edit: set gcp_project, repository.url, adjust start_command
make onboard CONFIG=my-gitlab.yaml
```
