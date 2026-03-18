# Server socket
bind = '0.0.0.0:8080'
backlog = 2048

# Worker processes
workers = 1  # Single worker so os.environ updates (e.g. proxy token refresh) are visible to all requests
worker_class = 'gthread'  # Use gthread for concurrent request handling with SSE streaming
threads = 4  # Number of threads per worker for handling concurrent connections
worker_connections = 1000
timeout = 3000
keepalive = 2

# Logging
accesslog = '-'
errorlog = '-'
loglevel = 'info'
access_log_format = '%(h)s %(l)s %(u)s %(t)s "%(r)s" %(s)s %(b)s "%(f)s" "%(a)s" %(D)s'

# Process naming
proc_name = 'claude_sandbox_service'

# Server mechanics
daemon = False
pidfile = None
umask = 0
user = None
group = None
tmp_upload_dir = None

# SSL/Security
limit_request_line = 4094
limit_request_fields = 100
limit_request_field_size = 8190


# Server hooks
def pre_fork(server, worker):
    """Called just before a worker is forked."""
    server.log.info('Worker spawning (pid: %s)', worker.pid)


def pre_exec(server):
    """Called just before a new master process is forked."""
    server.log.info('Forked child, re-executing.')


def when_ready(server):
    """Called just after the server is started."""
    server.log.info('Server is ready. Listening at: %s', server.cfg.bind)
