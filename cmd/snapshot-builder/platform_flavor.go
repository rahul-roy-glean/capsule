package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
)

type rootfsFlavor string

const (
	rootfsFlavorDebianLike rootfsFlavor = "debian-like"
	rootfsFlavorAlpineLike rootfsFlavor = "alpine-like"
)

type flavorConfig struct {
	flavor               rootfsFlavor
	packageManagerBinary string
	installCommand       string
	installEnv           []string
	requiredPaths        []string
	requiredBinaries     []string
	preferredRunnerShell []string
}

var commonRequiredPaths = []string{
	"/usr/local/bin/capsule-thaw-agent",
	"/workspace",
	"/var/run/capsule-thaw-agent",
	"/var/log/capsule-thaw-agent",
	"/etc/hostname",
	"/etc/hosts",
	"/etc/resolv.conf.default",
}

var commonRequiredBinaries = []string{
	"sh",
	"ip",
	"mount",
	"umount",
	"mountpoint",
	"blkid",
	"e2fsck",
	"fsfreeze",
	"hostname",
	"sync",
	"chown",
	"update-ca-certificates",
}

const platformShimSchemaVersion = "platform-shim-v2"

func getFlavorConfig(flavor rootfsFlavor) (flavorConfig, error) {
	switch flavor {
	case rootfsFlavorDebianLike:
		return flavorConfig{
			flavor:               flavor,
			packageManagerBinary: "apt-get",
			installCommand:       "apt-get update -qq && apt-get install -y -qq --no-install-recommends systemd systemd-sysv dbus iproute2 sudo util-linux e2fsprogs ca-certificates passwd",
			installEnv:           []string{"DEBIAN_FRONTEND=noninteractive", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"},
			requiredPaths: []string{
				"/lib/systemd/systemd",
				"/sbin/init",
				"/etc/systemd/system/capsule-thaw-agent.service",
				"/etc/systemd/system/multi-user.target.wants/capsule-thaw-agent.service",
				"/etc/systemd/network/10-eth0.network",
				"/etc/systemd/system/default.target",
				"/etc/systemd/system/getty.target.wants/serial-getty@ttyS0.service",
			},
			preferredRunnerShell: []string{"/bin/bash", "/bin/sh"},
		}, nil
	case rootfsFlavorAlpineLike:
		return flavorConfig{
			flavor:               flavor,
			packageManagerBinary: "apk",
			installCommand:       "apk add --no-cache openrc dbus iproute2 sudo util-linux e2fsprogs ca-certificates",
			installEnv:           []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"},
			requiredPaths: []string{
				"/sbin/openrc",
				"/sbin/init",
				"/lib/systemd/systemd",
				"/etc/init.d/capsule-thaw-agent",
				"/etc/runlevels/default/capsule-thaw-agent",
				"/etc/runlevels/default/networking",
				"/etc/runlevels/default/hostname",
				"/etc/network/interfaces",
				"/etc/inittab",
			},
			preferredRunnerShell: []string{"/bin/sh"},
		}, nil
	default:
		return flavorConfig{}, fmt.Errorf("unsupported rootfs flavor: %s", flavor)
	}
}

func detectRootfsFlavor(rootfsDir string) (rootfsFlavor, error) {
	if pathExistsInRootfs(rootfsDir, "/etc/alpine-release") {
		return rootfsFlavorAlpineLike, nil
	}
	if pathExistsInRootfs(rootfsDir, "/etc/debian_version") {
		return rootfsFlavorDebianLike, nil
	}

	if osReleaseData, err := os.ReadFile(filepath.Join(rootfsDir, "etc/os-release")); err == nil {
		lower := strings.ToLower(string(osReleaseData))
		switch {
		case strings.Contains(lower, "id=alpine"), strings.Contains(lower, "id_like=alpine"):
			return rootfsFlavorAlpineLike, nil
		case strings.Contains(lower, "id=debian"),
			strings.Contains(lower, "id=ubuntu"),
			strings.Contains(lower, "id_like=debian"),
			strings.Contains(lower, "id_like=ubuntu"):
			return rootfsFlavorDebianLike, nil
		}
	}

	switch {
	case binaryExistsInRootfs(rootfsDir, "apk"):
		return rootfsFlavorAlpineLike, nil
	case binaryExistsInRootfs(rootfsDir, "apt-get"):
		return rootfsFlavorDebianLike, nil
	default:
		return "", fmt.Errorf("unsupported base image: unable to detect a supported rootfs flavor")
	}
}

func bindRootfsSystemDirs(rootfsDir string, log *logrus.Entry) func() {
	bindMounts := []struct{ src, dst string }{
		{"/proc", filepath.Join(rootfsDir, "proc")},
		{"/sys", filepath.Join(rootfsDir, "sys")},
		{"/dev", filepath.Join(rootfsDir, "dev")},
	}
	for _, m := range bindMounts {
		_ = os.MkdirAll(m.dst, 0755)
		if out, err := exec.Command("mount", "--bind", m.src, m.dst).CombinedOutput(); err != nil {
			log.WithField("output", string(out)).Warnf("failed to bind-mount %s", m.src)
		}
	}
	return func() {
		for i := len(bindMounts) - 1; i >= 0; i-- {
			_ = exec.Command("umount", "-l", bindMounts[i].dst).Run()
		}
	}
}

func seedRootfsResolvConf(rootfsDir string) error {
	resolvPath := filepath.Join(rootfsDir, "etc/resolv.conf")
	if err := os.MkdirAll(filepath.Dir(resolvPath), 0755); err != nil {
		return fmt.Errorf("create resolv.conf directory: %w", err)
	}
	if hostResolv, err := os.ReadFile("/etc/resolv.conf"); err == nil {
		return os.WriteFile(resolvPath, hostResolv, 0644)
	}
	return os.WriteFile(resolvPath, []byte("nameserver 8.8.8.8\nnameserver 8.8.4.4\n"), 0644)
}

func ensurePlatformDependencies(rootfsDir string, flavor rootfsFlavor, log *logrus.Entry) error {
	cfg, err := getFlavorConfig(flavor)
	if err != nil {
		return err
	}

	if !binaryExistsInRootfs(rootfsDir, "sh") {
		return fmt.Errorf("unsupported base image: missing /bin/sh required for platform injection")
	}

	missing := missingPlatformBinaries(rootfsDir)
	if len(missing) == 0 && hasFlavorBaseInit(rootfsDir, flavor) {
		log.WithField("flavor", flavor).Info("Rootfs already has required platform tooling")
		return nil
	}

	if !binaryExistsInRootfs(rootfsDir, cfg.packageManagerBinary) {
		return fmt.Errorf("unsupported %s base image: missing package manager %q needed to install platform dependencies", flavor, cfg.packageManagerBinary)
	}

	installCmd := exec.Command("chroot", rootfsDir, "/bin/sh", "-c", cfg.installCommand)
	installCmd.Env = cfg.installEnv
	output, err := installCmd.CombinedOutput()
	if err != nil {
		log.WithField("output", string(output)).Warn("platform dependency install failed")
		return fmt.Errorf("install platform dependencies for %s: %w", flavor, err)
	}

	return nil
}

func hasFlavorBaseInit(rootfsDir string, flavor rootfsFlavor) bool {
	switch flavor {
	case rootfsFlavorDebianLike:
		return pathExistsInRootfs(rootfsDir, "/lib/systemd/systemd")
	case rootfsFlavorAlpineLike:
		return pathExistsInRootfs(rootfsDir, "/sbin/openrc") && pathExistsInRootfs(rootfsDir, "/sbin/init")
	default:
		return false
	}
}

func missingPlatformBinaries(rootfsDir string) []string {
	var missing []string
	for _, binary := range commonRequiredBinaries {
		if !binaryExistsInRootfs(rootfsDir, binary) {
			missing = append(missing, binary)
		}
	}
	return missing
}

func installThawAgentBinary(rootfsDir, thawAgentBin string) error {
	if _, err := os.Stat(thawAgentBin); os.IsNotExist(err) {
		return fmt.Errorf("capsule-thaw-agent binary not found at %s (build with 'make capsule-thaw-agent' or set --capsule-thaw-agent-path)", thawAgentBin)
	}

	thawAgentDst := filepath.Join(rootfsDir, "usr/local/bin/capsule-thaw-agent")
	if err := os.MkdirAll(filepath.Dir(thawAgentDst), 0755); err != nil {
		return fmt.Errorf("create capsule-thaw-agent destination directory: %w", err)
	}
	if err := copyFile(thawAgentBin, thawAgentDst); err != nil {
		return fmt.Errorf("failed to copy capsule-thaw-agent: %w", err)
	}
	if err := os.Chmod(thawAgentDst, 0755); err != nil {
		return fmt.Errorf("chmod capsule-thaw-agent: %w", err)
	}
	return nil
}

func configureFlavorPlatform(rootfsDir string, flavor rootfsFlavor, runnerUser string) error {
	switch flavor {
	case rootfsFlavorDebianLike:
		return configureDebianLikeRootfs(rootfsDir, runnerUser)
	case rootfsFlavorAlpineLike:
		return configureAlpineRootfs(rootfsDir, runnerUser)
	default:
		return fmt.Errorf("unsupported rootfs flavor: %s", flavor)
	}
}

func configureDebianLikeRootfs(rootfsDir, runnerUser string) error {
	if err := forceSymlink("/lib/systemd/systemd", filepath.Join(rootfsDir, "init")); err != nil {
		return fmt.Errorf("create /init symlink: %w", err)
	}

	serviceDir := filepath.Join(rootfsDir, "etc/systemd/system")
	if err := os.MkdirAll(serviceDir, 0755); err != nil {
		return fmt.Errorf("create systemd service dir: %w", err)
	}
	serviceContent := renderDebianThawAgentService(runnerUser)
	if err := os.WriteFile(filepath.Join(serviceDir, "capsule-thaw-agent.service"), []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("write capsule-thaw-agent.service: %w", err)
	}

	wantsDir := filepath.Join(serviceDir, "multi-user.target.wants")
	if err := os.MkdirAll(wantsDir, 0755); err != nil {
		return fmt.Errorf("create multi-user.target wants dir: %w", err)
	}
	if err := forceSymlink("/etc/systemd/system/capsule-thaw-agent.service", filepath.Join(wantsDir, "capsule-thaw-agent.service")); err != nil {
		return fmt.Errorf("enable capsule-thaw-agent service: %w", err)
	}

	networkDir := filepath.Join(rootfsDir, "etc/systemd/network")
	if err := os.MkdirAll(networkDir, 0755); err != nil {
		return fmt.Errorf("create systemd network dir: %w", err)
	}
	networkContent := renderDebianNetworkConfig()
	if err := os.WriteFile(filepath.Join(networkDir, "10-eth0.network"), []byte(networkContent), 0644); err != nil {
		return fmt.Errorf("write 10-eth0.network: %w", err)
	}

	if err := forceSymlink("/lib/systemd/system/multi-user.target", filepath.Join(serviceDir, "default.target")); err != nil {
		return fmt.Errorf("set default.target: %w", err)
	}
	for _, unit := range []string{"systemd-resolved.service", "systemd-timesyncd.service"} {
		if err := forceSymlink("/dev/null", filepath.Join(serviceDir, unit)); err != nil {
			return fmt.Errorf("mask %s: %w", unit, err)
		}
	}

	gettySource, err := firstExistingRootfsPath(rootfsDir, []string{
		"/lib/systemd/system/serial-getty@.service",
		"/usr/lib/systemd/system/serial-getty@.service",
	})
	if err != nil {
		return fmt.Errorf("find serial-getty unit: %w", err)
	}
	gettyWantsDir := filepath.Join(serviceDir, "getty.target.wants")
	if err := os.MkdirAll(gettyWantsDir, 0755); err != nil {
		return fmt.Errorf("create getty.target wants dir: %w", err)
	}
	if err := forceSymlink(gettySource, filepath.Join(gettyWantsDir, "serial-getty@ttyS0.service")); err != nil {
		return fmt.Errorf("enable serial-getty@ttyS0.service: %w", err)
	}

	return nil
}

func configureAlpineRootfs(rootfsDir, runnerUser string) error {
	if err := os.MkdirAll(filepath.Join(rootfsDir, "lib/systemd"), 0755); err != nil {
		return fmt.Errorf("create lib/systemd dir: %w", err)
	}
	if err := forceSymlink("/sbin/init", filepath.Join(rootfsDir, "lib/systemd/systemd")); err != nil {
		return fmt.Errorf("create Alpine systemd shim: %w", err)
	}
	if err := forceSymlink("/lib/systemd/systemd", filepath.Join(rootfsDir, "init")); err != nil {
		return fmt.Errorf("create /init symlink: %w", err)
	}

	initScript := renderAlpineThawAgentInitScript(runnerUser)
	initDir := filepath.Join(rootfsDir, "etc/init.d")
	if err := os.MkdirAll(initDir, 0755); err != nil {
		return fmt.Errorf("create init.d dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(initDir, "capsule-thaw-agent"), []byte(initScript), 0755); err != nil {
		return fmt.Errorf("write capsule-thaw-agent init script: %w", err)
	}

	runlevelDir := filepath.Join(rootfsDir, "etc/runlevels/default")
	if err := os.MkdirAll(runlevelDir, 0755); err != nil {
		return fmt.Errorf("create default runlevel dir: %w", err)
	}
	if err := forceSymlink("/etc/init.d/capsule-thaw-agent", filepath.Join(runlevelDir, "capsule-thaw-agent")); err != nil {
		return fmt.Errorf("enable capsule-thaw-agent in default runlevel: %w", err)
	}

	interfacesContent := renderAlpineInterfacesConfig()
	if err := os.MkdirAll(filepath.Join(rootfsDir, "etc/network"), 0755); err != nil {
		return fmt.Errorf("create network dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(rootfsDir, "etc/network/interfaces"), []byte(interfacesContent), 0644); err != nil {
		return fmt.Errorf("write /etc/network/interfaces: %w", err)
	}

	for _, svc := range []string{"networking", "hostname"} {
		target := filepath.Join(rootfsDir, "etc/init.d", svc)
		if _, err := os.Stat(target); err != nil {
			return fmt.Errorf("required OpenRC service %s missing: %w", svc, err)
		}
		if err := forceSymlink("/etc/init.d/"+svc, filepath.Join(runlevelDir, svc)); err != nil {
			return fmt.Errorf("enable %s in default runlevel: %w", svc, err)
		}
	}

	bootDir := filepath.Join(rootfsDir, "etc/runlevels/boot")
	if err := os.MkdirAll(bootDir, 0755); err != nil {
		return fmt.Errorf("create boot runlevel dir: %w", err)
	}
	for _, svc := range []string{"procfs", "sysfs", "devfs", "mdev", "hwclock", "modules", "sysctl", "hostname", "bootmisc", "syslog"} {
		target := filepath.Join(rootfsDir, "etc/init.d", svc)
		if _, err := os.Stat(target); err == nil {
			if err := forceSymlink("/etc/init.d/"+svc, filepath.Join(bootDir, svc)); err != nil {
				return fmt.Errorf("enable %s in boot runlevel: %w", svc, err)
			}
		}
	}

	inittabContent := renderAlpineInittab()
	if err := os.WriteFile(filepath.Join(rootfsDir, "etc/inittab"), []byte(inittabContent), 0644); err != nil {
		return fmt.Errorf("write /etc/inittab: %w", err)
	}

	return nil
}

func createRunnerUser(rootfsDir string, flavor rootfsFlavor, runnerUser string) error {
	if userExistsInRootfs(rootfsDir, runnerUser) {
		return nil
	}

	cfg, err := getFlavorConfig(flavor)
	if err != nil {
		return err
	}
	shell := runnerShell(rootfsDir, cfg.preferredRunnerShell)

	var cmd *exec.Cmd
	switch flavor {
	case rootfsFlavorAlpineLike:
		cmd = exec.Command("chroot", rootfsDir, "adduser", "-D", "-s", shell, "-h", "/home/"+runnerUser, runnerUser)
	case rootfsFlavorDebianLike:
		cmd = exec.Command("chroot", rootfsDir, "useradd", "-m", "-s", shell, runnerUser)
	default:
		return fmt.Errorf("unsupported rootfs flavor: %s", flavor)
	}

	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create runner user %q: %s: %w", runnerUser, strings.TrimSpace(string(output)), err)
	}
	return nil
}

func ensureWorkspaceOwnership(rootfsDir, runnerUser string) error {
	output, err := exec.Command("chroot", rootfsDir, "chown", "-R", runnerUser+":"+runnerUser, "/workspace").CombinedOutput()
	if err != nil {
		return fmt.Errorf("chown /workspace for %s: %s: %w", runnerUser, strings.TrimSpace(string(output)), err)
	}
	return nil
}

func writeCommonRootfsFiles(rootfsDir string, dockerEnv []string, log *logrus.Entry) error {
	for _, dir := range []string{
		"workspace",
		"var/run/capsule-thaw-agent",
		"var/log/capsule-thaw-agent",
		"mnt/ephemeral/caches/repository",
		"mnt/ephemeral/bazel",
		"mnt/ephemeral/output",
	} {
		if err := os.MkdirAll(filepath.Join(rootfsDir, dir), 0755); err != nil {
			return fmt.Errorf("create required directory %s: %w", dir, err)
		}
	}

	if err := os.WriteFile(filepath.Join(rootfsDir, "etc/hostname"), []byte("runner\n"), 0644); err != nil {
		return fmt.Errorf("write /etc/hostname: %w", err)
	}
	if err := os.WriteFile(filepath.Join(rootfsDir, "etc/hosts"), []byte("127.0.0.1\tlocalhost\n::1\t\tlocalhost ip6-localhost ip6-loopback\n"), 0644); err != nil {
		return fmt.Errorf("write /etc/hosts: %w", err)
	}
	if err := os.WriteFile(filepath.Join(rootfsDir, "etc/resolv.conf.default"), []byte("nameserver 8.8.8.8\n"), 0644); err != nil {
		return fmt.Errorf("write /etc/resolv.conf.default: %w", err)
	}

	nsswitchPath := filepath.Join(rootfsDir, "etc/nsswitch.conf")
	if nssData, err := os.ReadFile(nsswitchPath); err == nil {
		fixed := strings.ReplaceAll(string(nssData), "resolve [!UNAVAIL=return]", "")
		fixed = strings.ReplaceAll(fixed, "resolve", "")
		if err := os.WriteFile(nsswitchPath, []byte(fixed), 0644); err != nil {
			return fmt.Errorf("write /etc/nsswitch.conf: %w", err)
		}
	}

	if len(dockerEnv) > 0 {
		var envLines []string
		for _, env := range dockerEnv {
			if strings.HasPrefix(env, "DEBIAN_FRONTEND=") {
				continue
			}
			envLines = append(envLines, env)
		}
		if len(envLines) > 0 {
			envContent := strings.Join(envLines, "\n") + "\n"
			envPath := filepath.Join(rootfsDir, "etc/environment")
			if err := os.WriteFile(envPath, []byte(envContent), 0644); err != nil {
				log.WithError(err).Warn("Failed to write /etc/environment")
			} else {
				log.WithField("env_count", len(envLines)).Info("Wrote Docker ENV to /etc/environment")
			}

			profileDir := filepath.Join(rootfsDir, "etc/profile.d")
			_ = os.MkdirAll(profileDir, 0755)
			var profileLines []string
			for _, env := range envLines {
				if idx := strings.Index(env, "="); idx > 0 {
					key := env[:idx]
					val := env[idx+1:]
					profileLines = append(profileLines, fmt.Sprintf("export %s=%q", key, val))
				}
			}
			profileContent := "# Docker image environment variables\n" + strings.Join(profileLines, "\n") + "\n"
			if err := os.WriteFile(filepath.Join(profileDir, "docker-env.sh"), []byte(profileContent), 0755); err != nil {
				log.WithError(err).Warn("Failed to write /etc/profile.d/docker-env.sh")
			}
		}
	}

	return nil
}

func validateInjectedRootfs(rootfsDir string, flavor rootfsFlavor, runnerUser string) error {
	cfg, err := getFlavorConfig(flavor)
	if err != nil {
		return err
	}

	var missing []string
	for _, path := range commonRequiredPaths {
		if !pathExistsInRootfs(rootfsDir, path) {
			missing = append(missing, path)
		}
	}
	for _, path := range cfg.requiredPaths {
		if !pathExistsInRootfs(rootfsDir, path) {
			missing = append(missing, path)
		}
	}
	for _, binary := range commonRequiredBinaries {
		if !binaryExistsInRootfs(rootfsDir, binary) {
			missing = append(missing, "binary:"+binary)
		}
	}
	for _, binary := range cfg.requiredBinaries {
		if !binaryExistsInRootfs(rootfsDir, binary) {
			missing = append(missing, "binary:"+binary)
		}
	}
	if !userExistsInRootfs(rootfsDir, runnerUser) {
		missing = append(missing, "user:"+runnerUser)
	}

	if len(missing) > 0 {
		return fmt.Errorf("missing platform requirements: %s", strings.Join(missing, ", "))
	}
	return nil
}

func binaryExistsInRootfs(rootfsDir, binary string) bool {
	for _, dir := range []string{
		"/bin",
		"/sbin",
		"/usr/bin",
		"/usr/sbin",
		"/usr/local/bin",
		"/usr/local/sbin",
	} {
		if pathExistsInRootfs(rootfsDir, filepath.Join(dir, binary)) {
			return true
		}
	}
	return false
}

func pathExistsInRootfs(rootfsDir, relPath string) bool {
	path := filepath.Join(rootfsDir, strings.TrimPrefix(relPath, "/"))
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return true
	}

	target, err := os.Readlink(path)
	if err != nil {
		return false
	}

	var resolved string
	if filepath.IsAbs(target) {
		resolved = filepath.Join(rootfsDir, strings.TrimPrefix(target, "/"))
	} else {
		resolved = filepath.Join(filepath.Dir(path), target)
	}
	_, err = os.Stat(filepath.Clean(resolved))
	return err == nil
}

func forceSymlink(target, linkPath string) error {
	_ = os.Remove(linkPath)
	if err := os.MkdirAll(filepath.Dir(linkPath), 0755); err != nil {
		return err
	}
	return os.Symlink(target, linkPath)
}

func runnerShell(rootfsDir string, candidates []string) string {
	for _, candidate := range candidates {
		if pathExistsInRootfs(rootfsDir, candidate) {
			return candidate
		}
	}
	return "/bin/sh"
}

func userExistsInRootfs(rootfsDir, username string) bool {
	passwdPath := filepath.Join(rootfsDir, "etc/passwd")
	data, err := os.ReadFile(passwdPath)
	if err != nil {
		return false
	}
	prefix := username + ":"
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func firstExistingRootfsPath(rootfsDir string, candidates []string) (string, error) {
	for _, candidate := range candidates {
		if pathExistsInRootfs(rootfsDir, candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("none of the candidate paths exist: %s", strings.Join(candidates, ", "))
}

func computePlatformShimFingerprint(flavor rootfsFlavor, runnerUser string) (string, error) {
	cfg, err := getFlavorConfig(flavor)
	if err != nil {
		return "", err
	}

	payload := struct {
		SchemaVersion      string   `json:"schema_version"`
		Flavor             string   `json:"flavor"`
		CommonRequiredPath []string `json:"common_required_paths"`
		CommonRequiredBins []string `json:"common_required_bins"`
		RequiredPaths      []string `json:"required_paths"`
		RequiredBinaries   []string `json:"required_binaries"`
		InstallCommand     string   `json:"install_command"`
		RunnerUser         string   `json:"runner_user"`
		ServiceTemplate    string   `json:"service_template,omitempty"`
		NetworkTemplate    string   `json:"network_template,omitempty"`
		InitTemplate       string   `json:"init_template,omitempty"`
		InterfacesTemplate string   `json:"interfaces_template,omitempty"`
		InittabTemplate    string   `json:"inittab_template,omitempty"`
	}{
		SchemaVersion:      platformShimSchemaVersion,
		Flavor:             string(flavor),
		CommonRequiredPath: commonRequiredPaths,
		CommonRequiredBins: commonRequiredBinaries,
		RequiredPaths:      cfg.requiredPaths,
		RequiredBinaries:   cfg.requiredBinaries,
		InstallCommand:     cfg.installCommand,
		RunnerUser:         runnerUser,
	}

	switch flavor {
	case rootfsFlavorDebianLike:
		payload.ServiceTemplate = renderDebianThawAgentService(runnerUser)
		payload.NetworkTemplate = renderDebianNetworkConfig()
	case rootfsFlavorAlpineLike:
		payload.InitTemplate = renderAlpineThawAgentInitScript(runnerUser)
		payload.InterfacesTemplate = renderAlpineInterfacesConfig()
		payload.InittabTemplate = renderAlpineInittab()
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func renderDebianThawAgentService(runnerUser string) string {
	return fmt.Sprintf(`[Unit]
Description=Thaw Agent - MicroVM initialization
After=network.target
Wants=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/capsule-thaw-agent --runner-user=%s
Restart=on-failure
RestartSec=5
StandardOutput=journal+console
StandardError=journal+console
Environment=LOG_LEVEL=info
Environment=HOME=/root

[Install]
WantedBy=multi-user.target
`, runnerUser)
}

func renderDebianNetworkConfig() string {
	return `[Match]
Name=eth0

[Network]
DHCP=no
`
}

func renderAlpineThawAgentInitScript(runnerUser string) string {
	return fmt.Sprintf(`#!/sbin/openrc-run

name="capsule-thaw-agent"
description="Thaw Agent - MicroVM initialization"
command="/usr/local/bin/capsule-thaw-agent"
command_args="--runner-user=%s"
command_background=true
pidfile="/run/${RC_SVCNAME}.pid"
output_log="/var/log/capsule-thaw-agent/stdout.log"
error_log="/var/log/capsule-thaw-agent/stderr.log"

depend() {
	need net
	after firewall
}
`, runnerUser)
}

func renderAlpineInterfacesConfig() string {
	return `auto lo
iface lo inet loopback

auto eth0
iface eth0 inet manual
`
}

func renderAlpineInittab() string {
	return `# Firecracker microVM inittab (busybox init + OpenRC)
# Ensure /dev is devtmpfs and block device nodes exist before OpenRC services.
# Alpine-like images may not have udev/mdev services; devtmpfs auto-creates
# nodes for kernel-known devices, and mdev -s catches anything missed.
::sysinit:/bin/sh -c 'mount -t devtmpfs devtmpfs /dev 2>/dev/null; mount -t sysfs sysfs /sys 2>/dev/null; mount -t proc proc /proc 2>/dev/null; [ -x /sbin/mdev ] && mdev -s 2>/dev/null; true'
::sysinit:/sbin/openrc sysinit
::sysinit:/sbin/openrc boot
::wait:/sbin/openrc default
::shutdown:/sbin/openrc shutdown
ttyS0::respawn:/sbin/getty -L ttyS0 115200 vt100
`
}
