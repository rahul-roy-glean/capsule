# Claude Sandbox Service

## ENV set up

```
$ export CLAUDE_CODE_USE_VERTEX=1 CLOUD_ML_REGION=us-east5 ANTHROPIC_VERTEX_PROJECT_ID=dev-sandbox-334901 WORKSPACE_DIR=$(pwd) REPO_DIR=$(pwd) CLAUDE_HOME=$HOME/.claude NOOP=1
```

## API Endpoints

```
$ PYTHONPATH=. gunicorn --bind 127.0.0.1:8080 --config deploy/configs/claude_sandbox_service/gunicorn_config.py "python_scio.platform.common.app.claude_sandbox_service.claude_sandbox_service:app"
```

OR

```
$ bazel run //python_scio/platform/common/app/claude_sandbox_service:claude_sandbox_service -- --host 127.0.0.1 --port 8080
```

OR

```
$ PYTHONPATH=. python3 python_scio/platform/common/app/claude_sandbox_service/claude_sandbox_service.py
```

### Health Check
```bash
curl http://localhost:8080/health
```

### Get Service Info
```bash
curl http://localhost:8080/info
```

### Create Or Inspect A Session
```bash
curl -X POST http://localhost:8080/sessions \
  -H "Content-Type: application/json" \
  -d '{"session_id": "default"}'
```

```bash
curl http://localhost:8080/sessions/default
```

### Query Claude In A Session
```bash
curl -X POST http://localhost:8080/sessions/default/query \
  -H "Content-Type: application/json" \
  -d '{"prompt": "What files are in the current directory?", "timeout": 300}' \
  --no-buffer
```

### Compatibility Execute Routes
```bash
curl -X POST http://localhost:8080/execute_stream \
  -H "Content-Type: application/json" \
  -d '{"prompt": "What files are in the current directory?", "timeout": 300}'
```

### Interrupt Or Reset A Session
```bash
curl -X POST http://localhost:8080/sessions/default/interrupt

curl -X POST http://localhost:8080/sessions/default/reset
```

### To run on K8s

Do the below so your session persists

```
$ caffeinate
```

Then, exec directly as port-forwarding is not set up for the given K8s service

```
$ kubectl exec -it notebook-sandbox-orchestrator-deployment-7596975987-xh8wt -- bash
$ curl -X POST http://localhost:8080/getorcreatesandbox -H "Content-Type: application/json" -d '{"executionId": "exec_12345", "userId": "user_abc123", "sandboxType": "claude", "agentConfig": {"repo_url": "https://github.com/askscio/scio.git"}}'
```

Then,

```
$ kubectl exec -it claude-code-pod-mhg8mvznkw -- bash
```

If you need to check the file, use inspect `__file__` on the module

```
$ /workspace/repo/python_scio/platform/common/app/claude_sandbox_service/claude_sandbox_service.py
```

On FE, set the flags when debugging the sandbox E2E and skip the whole orchestrator to sandbox setup:

```
db.debug_mode=1
wo.step_timeout_seconds_override=4000
db.ai_coding_assistant_sandbox_url=http://localhost:8080
```