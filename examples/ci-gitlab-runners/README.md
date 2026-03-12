# Example: GitLab Runners

Use this example when you want to run GitLab-style CI workloads on Capsule.

The main lesson of this example is simple: most CI platforms differ primarily in
their runner binary and startup flags, not in the underlying Capsule runtime.

## What Changes From Other CI Examples

Compared with a GitHub Actions-style runner setup, the main differences are:

- the runner binary
- the registration or startup flags
- any CI-provider-specific environment variables

The surrounding Capsule model stays the same:

- build a warm snapshot
- restore a VM
- launch the runner process with `start_command`

## Example `start_command`

```yaml
start_command:
  command: ["gitlab-runner", "run", "--working-directory", "/workspace",
            "--url", "https://gitlab.com", "--token", "${CI_RUNNER_TOKEN}"]
```

## What You Need To Edit

Before using this example, update:

- `platform.gcp_project`
- the GitLab URL
- the runner token source
- any runner tags or working-directory settings

## Onboard

```bash
cp examples/ci-gitlab-runners/onboard.yaml my-gitlab.yaml
# Edit the fields described above
make onboard CONFIG=my-gitlab.yaml
```

## Good Fit

This example is most useful as a minimal delta from other CI examples. If your
main question is "how do I run a different CI runner binary on Capsule?", start
here.
