package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	onboardNamespace     = "capsule"
	portForwardLocalPort = 18080
)

func runCommandStreaming(cmd *exec.Cmd) error {
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func terraformOutputRaw(name string) (string, error) {
	cmd := exec.Command("terraform", "-chdir=deploy/terraform", "output", "-raw", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("terraform output %s failed: %w\n%s", name, err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

func gcsStateBucketForProject(cfg *Config) string {
	if cfg.Platform.TerraformStateBucket != "" {
		return cfg.Platform.TerraformStateBucket
	}
	return fmt.Sprintf("%s-firecracker-tfstate", cfg.Platform.GCPProject)
}

func terraformStatePrefix(cfg *Config) string {
	if cfg.Platform.TerraformStatePrefix != "" {
		return cfg.Platform.TerraformStatePrefix
	}
	return "firecracker"
}

func generateSecretString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func waitForTCP(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", addr)
}

func withControlPlanePortForward(fn func(baseURL string) error) error {
	cmd := exec.Command("kubectl", "-n", onboardNamespace, "port-forward", "svc/control-plane", fmt.Sprintf("%d:8080", portForwardLocalPort))
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start kubectl port-forward: %w", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_, _ = cmd.Process.Wait()
	}()

	if err := waitForTCP(fmt.Sprintf("127.0.0.1:%d", portForwardLocalPort), 20*time.Second); err != nil {
		return err
	}

	return fn(fmt.Sprintf("http://127.0.0.1:%d", portForwardLocalPort))
}

func detectPublicIPCIDR() (string, error) {
	resp, err := http.Get("https://checkip.amazonaws.com")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(body))
	if ip == "" {
		return "", fmt.Errorf("empty public ip response")
	}
	return ip + "/32", nil
}
