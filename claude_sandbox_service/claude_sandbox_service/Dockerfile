FROM us-central1-docker.pkg.dev/scio-engineering/glean-images/ubuntu:24.04

# Prevent interactive prompts during package installation
ENV DEBIAN_FRONTEND=noninteractive

# Install system dependencies
RUN apt-get update && apt-get install -y \
    curl \
    git \
    python3 \
    python3-pip \
    python3-venv \
    bash \
    ca-certificates \
    gnupg \
    openssh-client \
    openssh-server \
    sudo \
    build-essential \
    jq \
    && rm -rf /var/lib/apt/lists/*

# Create non-root user aligned with runAsUser=1000 (Option A)
RUN set -eux; \
    # Remove any pre-existing 'claude' user to avoid name conflicts
    userdel -f claude 2>/dev/null || true; \
    # If any user already owns UID 1000, remove it to free the UID
    EXISTING_USER="$(getent passwd 1000 | cut -d: -f1 || true)"; \
    if [ -n "${EXISTING_USER}" ] && [ "${EXISTING_USER}" != "claude" ]; then \
      userdel -f "${EXISTING_USER}" || true; \
    fi; \
    # Ensure GID 1000 exists (name may differ); create if missing
    if ! getent group 1000 >/dev/null 2>&1; then \
      groupadd -g 1000 claude; \
    fi; \
    # Create 'claude' with UID/GID 1000
    id -u claude >/dev/null 2>&1 || useradd -u 1000 -g 1000 -m -s /bin/bash claude; \
    echo "claude ALL=(ALL) NOPASSWD:ALL" >> /etc/sudoers

# Install Node.js 22.x (required for Claude CLI)
# Using new NodeSource installation method for Ubuntu 24.04 compatibility
RUN curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key | gpg --dearmor -o /usr/share/keyrings/nodesource.gpg \
    && echo "deb [signed-by=/usr/share/keyrings/nodesource.gpg] https://deb.nodesource.com/node_22.x nodistro main" | tee /etc/apt/sources.list.d/nodesource.list \
    && apt-get update \
    && apt-get install -y nodejs \
    && rm -rf /var/lib/apt/lists/*

# Install GitHub CLI for repository operations
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" | tee /etc/apt/sources.list.d/github-cli.list > /dev/null \
    && apt-get update \
    && apt-get install -y gh \
    && rm -rf /var/lib/apt/lists/*
# Install bazelisk (for bazel)
ADD https://github.com/bazelbuild/bazelisk/releases/download/v1.27.0/bazelisk-linux-amd64 /usr/local/bin/bazel

RUN chmod +x /usr/local/bin/bazel \
    && /usr/local/bin/bazel


# Create a dedicated virtual environment for Python deps (avoids PEP 668 issues)
RUN python3 -m venv /opt/venv \
    && /opt/venv/bin/python -m pip install --no-cache-dir --upgrade pip setuptools wheel \
    && /opt/venv/bin/pip install --no-cache-dir "pydantic>=2.11.7,<3.0" flask gunicorn anthropic boto3

# Install Claude CLI globally
RUN npm install -g @anthropic-ai/claude-code

# Install the Glean MCP server globally
RUN npm install -g @gleanwork/local-mcp-server@0.7.16

# Install ESLint and TypeScript support for pre-commit linting
RUN npm install -g eslint @typescript-eslint/parser @typescript-eslint/eslint-plugin

# Install Go for pre-commit linting (gofmt comes with Go)
ARG GO_VERSION=1.23.4
RUN curl -fsSL https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz | tar -C /usr/local -xzf -
ENV PATH="/usr/local/go/bin:${PATH}"

# Install Go tools for pre-commit linting
RUN mkdir -p /opt/go-tools/bin \
    # golines - enforce line length (tarball contains golines_*/golines)
    && curl -fsSL https://github.com/segmentio/golines/releases/download/v0.12.2/golines_0.12.2_linux_amd64.tar.gz | tar -C /tmp -xzf - \
    && mv /tmp/golines_*/golines /opt/go-tools/bin/golines \
    && rm -rf /tmp/golines_* \
    # goimports - organize imports
    && GOBIN=/opt/go-tools/bin go install golang.org/x/tools/cmd/goimports@v0.28.0
ENV PATH="/opt/go-tools/bin:${PATH}"

# Install google cloud SDK to run gsutil and other GCloud commands (gcloud)
ARG GCLOUD_SDK_VERSION=555.0.0-0
ENV GCLOUD_SDK_VERSION=${GCLOUD_SDK_VERSION}
COPY google-cloud-sdk.list /etc/apt/sources.list.d/google-cloud-sdk.list
# Add Google Cloud SDK apt key and install
RUN curl -fsSL https://packages.cloud.google.com/apt/doc/apt-key.gpg \
      | gpg --dearmor --no-tty -o /usr/share/keyrings/cloud.google.gpg && \
    apt-get update -y && \
    apt-get -y install google-cloud-cli=${GCLOUD_SDK_VERSION} && \
    rm -rf /var/lib/apt/lists/*

# Prepare SSH server
RUN mkdir -p /var/run/sshd

# Set a development password for the claude user (password: claude)
RUN echo 'claude:claude' | chpasswd

RUN mkdir -p /home/claude/.claude /workspace /app \
    && chown -R 1000:1000 /home/claude /workspace /app /usr/local/

WORKDIR /app

ADD . /app
ENV RUNFILES_DIR=claude_sandbox_service.runfiles
ENV PYTHONPATH="/app/claude_sandbox_service.runfiles/_main:claude_sandbox_service.runfiles:${PYTHONPATH}"
COPY gunicorn_config.py /app/gunicorn_config.py
COPY startup.sh /app/startup.sh
RUN chmod 755 /app/startup.sh && chown 1000:1000 /app/startup.sh

# Copy sandbox system prompt
COPY sandbox-claude.md /opt/claude-defaults/system-prompt.md
RUN chown 1000:1000 /opt/claude-defaults/system-prompt.md

# Copy hooks
COPY git-clean-state-hook.sh /opt/claude-hooks/git-clean-state-hook.sh
COPY pr-creation-hook.sh /opt/claude-hooks/pr-creation-hook.sh
COPY push-branch-hook.sh /opt/claude-hooks/push-branch-hook.sh
COPY branch-guard.sh /opt/claude-hooks/branch-guard.sh
RUN chmod +x /opt/claude-hooks/*.sh && chown -R 1000:1000 /opt/claude-hooks

# Copy GCE metadata emulator for cross-cloud Vertex AI authentication (AWS)
COPY gce_metadata_emulator.py /opt/gce-metadata-emulator/gce_metadata_emulator.py
RUN chmod +x /opt/gce-metadata-emulator/gce_metadata_emulator.py

# Copy Claude Code settings file with hooks (as root, before USER switch)
COPY --chown=1000:1000 settings.json /home/claude/.claude/settings.json

# Copy git branding hook installer
RUN mkdir -p /home/claude/.git-helpers
COPY install-branding-hook.sh /home/claude/.git-helpers/install-branding-hook.sh
RUN chmod +x /home/claude/.git-helpers/install-branding-hook.sh && chown -R 1000:1000 /home/claude/.git-helpers

# Switch to non-root user
USER claude

# Set up git defaults for the user (GPG signing will be configured at runtime)
RUN git config --global user.name "Glean Code Writer" \
    && git config --global user.email "code_writer@glean.com" \
    && git config --global init.defaultBranch main \
    && git config --global credential.helper store \
    && git config --global core.editor "vim" \
    && mkdir -p /home/claude/.gnupg \
    && chmod 700 /home/claude/.gnupg


# Set environment variables
ENV HOME=/home/claude
ENV CLAUDE_HOME=/home/claude/.claude
ENV VIRTUAL_ENV=/opt/venv
ENV PATH="/opt/venv/bin:/home/claude/.local/bin:${PATH}"
ENV PYTHONUNBUFFERED=1


# Expose the service and SSH ports
EXPOSE 8080 22

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD curl -f http://localhost:8080/health || exit 1

# Run the startup script
ENTRYPOINT ["/app/startup.sh"]
