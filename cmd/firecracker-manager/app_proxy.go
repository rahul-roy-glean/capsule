package main

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/runner"
)

func appProxyHandler(mgr *runner.Manager, logger *logrus.Logger) http.HandlerFunc {
	log := logger.WithField("handler", "app-proxy")
	return func(w http.ResponseWriter, r *http.Request) {
		// Parse path: /api/v1/runners/{runner_id}/app/...
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/runners/")
		parts := strings.SplitN(path, "/", 2) // [runner_id, "app/..."]
		if len(parts) < 2 || !strings.HasPrefix(parts[1], "app") {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		runnerID := parts[0]
		remaining := "/" + strings.TrimPrefix(parts[1], "app")

		rn, err := mgr.GetRunner(runnerID)
		if err != nil {
			http.Error(w, "runner not found", http.StatusNotFound)
			return
		}
		if rn.ApplicationPort == 0 {
			http.Error(w, "no application configured", http.StatusBadRequest)
			return
		}

		target, _ := url.Parse(fmt.Sprintf("http://%s:%d", rn.InternalIP, rn.ApplicationPort))
		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.FlushInterval = -1 // critical for SSE streaming

		r.URL.Path = remaining
		r.URL.RawPath = ""

		log.WithFields(logrus.Fields{
			"runner_id": runnerID,
			"target":    target.String(),
			"path":      remaining,
		}).Debug("Proxying request to application")

		proxy.ServeHTTP(w, r)
	}
}
