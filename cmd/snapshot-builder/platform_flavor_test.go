package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectRootfsFlavorAlpine(t *testing.T) {
	rootfs := t.TempDir()
	mustWriteRootfsFile(t, rootfs, "/etc/alpine-release", "3.20.0\n")

	flavor, err := detectRootfsFlavor(rootfs)
	if err != nil {
		t.Fatalf("detectRootfsFlavor(alpine) returned error: %v", err)
	}
	if flavor != rootfsFlavorAlpineLike {
		t.Fatalf("detectRootfsFlavor(alpine) = %q, want %q", flavor, rootfsFlavorAlpineLike)
	}
}

func TestDetectRootfsFlavorDebian(t *testing.T) {
	rootfs := t.TempDir()
	mustWriteRootfsFile(t, rootfs, "/etc/debian_version", "12.0\n")

	flavor, err := detectRootfsFlavor(rootfs)
	if err != nil {
		t.Fatalf("detectRootfsFlavor(debian) returned error: %v", err)
	}
	if flavor != rootfsFlavorDebianLike {
		t.Fatalf("detectRootfsFlavor(debian) = %q, want %q", flavor, rootfsFlavorDebianLike)
	}
}

func TestDetectRootfsFlavorFromOSRelease(t *testing.T) {
	rootfs := t.TempDir()
	mustWriteRootfsFile(t, rootfs, "/etc/os-release", "ID=ubuntu\nID_LIKE=debian\n")

	flavor, err := detectRootfsFlavor(rootfs)
	if err != nil {
		t.Fatalf("detectRootfsFlavor(os-release) returned error: %v", err)
	}
	if flavor != rootfsFlavorDebianLike {
		t.Fatalf("detectRootfsFlavor(os-release) = %q, want %q", flavor, rootfsFlavorDebianLike)
	}
}

func TestDetectRootfsFlavorUnsupported(t *testing.T) {
	rootfs := t.TempDir()

	if _, err := detectRootfsFlavor(rootfs); err == nil {
		t.Fatal("detectRootfsFlavor(unsupported) returned nil error, want unsupported error")
	}
}

func TestValidateInjectedRootfsDebianLike(t *testing.T) {
	rootfs := buildValidRootfsFixture(t, rootfsFlavorDebianLike, "runner")

	if err := validateInjectedRootfs(rootfs, rootfsFlavorDebianLike, "runner"); err != nil {
		t.Fatalf("validateInjectedRootfs(debian-like) returned error: %v", err)
	}
}

func TestValidateInjectedRootfsDebianLikeWithRootfsLocalSymlinks(t *testing.T) {
	rootfs := buildValidRootfsFixture(t, rootfsFlavorDebianLike, "runner")

	mustSymlinkRootfs(t, rootfs, "/etc/systemd/system/capsule-thaw-agent.service", "/etc/systemd/system/multi-user.target.wants/capsule-thaw-agent.service")
	mustWriteRootfsFile(t, rootfs, "/lib/systemd/system/multi-user.target", "fixture\n")
	mustSymlinkRootfs(t, rootfs, "/lib/systemd/system/multi-user.target", "/etc/systemd/system/default.target")
	mustWriteRootfsFile(t, rootfs, "/lib/systemd/system/serial-getty@.service", "fixture\n")
	mustSymlinkRootfs(t, rootfs, "/lib/systemd/system/serial-getty@.service", "/etc/systemd/system/getty.target.wants/serial-getty@ttyS0.service")

	if err := validateInjectedRootfs(rootfs, rootfsFlavorDebianLike, "runner"); err != nil {
		t.Fatalf("validateInjectedRootfs(debian-like symlinks) returned error: %v", err)
	}
}

func TestValidateInjectedRootfsMissingRequiredBinary(t *testing.T) {
	rootfs := buildValidRootfsFixture(t, rootfsFlavorDebianLike, "runner")
	if err := os.Remove(filepath.Join(rootfs, strings.TrimPrefix(binaryFixturePath("mountpoint"), "/"))); err != nil {
		t.Fatalf("remove mountpoint binary: %v", err)
	}

	err := validateInjectedRootfs(rootfs, rootfsFlavorDebianLike, "runner")
	if err == nil {
		t.Fatal("validateInjectedRootfs with missing mountpoint returned nil, want error")
	}
}

func TestValidateInjectedRootfsMissingRunnerUser(t *testing.T) {
	rootfs := buildValidRootfsFixture(t, rootfsFlavorAlpineLike, "runner")
	if err := os.WriteFile(filepath.Join(rootfs, "etc/passwd"), []byte("root:x:0:0:root:/root:/bin/sh\n"), 0644); err != nil {
		t.Fatalf("rewrite passwd fixture: %v", err)
	}

	err := validateInjectedRootfs(rootfs, rootfsFlavorAlpineLike, "runner")
	if err == nil {
		t.Fatal("validateInjectedRootfs with missing runner user returned nil, want error")
	}
}

func buildValidRootfsFixture(t *testing.T, flavor rootfsFlavor, runnerUser string) string {
	t.Helper()

	rootfs := t.TempDir()
	for _, dir := range []string{
		"/bin",
		"/sbin",
		"/usr/bin",
		"/usr/sbin",
		"/usr/local/bin",
		"/etc",
		"/etc/systemd/system",
		"/etc/systemd/system/multi-user.target.wants",
		"/etc/systemd/system/getty.target.wants",
		"/etc/systemd/network",
		"/etc/init.d",
		"/etc/runlevels/default",
		"/etc/network",
		"/var/run/capsule-thaw-agent",
		"/var/log/capsule-thaw-agent",
		"/workspace",
	} {
		mustMkdirRootfs(t, rootfs, dir)
	}

	for _, binary := range commonRequiredBinaries {
		mustWriteExecutable(t, rootfs, binaryFixturePath(binary), "#!/bin/sh\n")
	}
	mustWriteExecutable(t, rootfs, "/usr/local/bin/capsule-thaw-agent", "#!/bin/sh\n")
	mustWriteRootfsFile(t, rootfs, "/etc/hostname", "runner\n")
	mustWriteRootfsFile(t, rootfs, "/etc/hosts", "127.0.0.1 localhost\n")
	mustWriteRootfsFile(t, rootfs, "/etc/resolv.conf.default", "nameserver 8.8.8.8\n")
	mustWriteRootfsFile(t, rootfs, "/etc/passwd", "root:x:0:0:root:/root:/bin/sh\n"+runnerUser+":x:1000:1000::/home/"+runnerUser+":/bin/sh\n")

	switch flavor {
	case rootfsFlavorDebianLike:
		for _, path := range []string{
			"/lib/systemd/systemd",
			"/sbin/init",
			"/etc/systemd/system/capsule-thaw-agent.service",
			"/etc/systemd/system/multi-user.target.wants/capsule-thaw-agent.service",
			"/etc/systemd/network/10-eth0.network",
			"/etc/systemd/system/default.target",
			"/etc/systemd/system/getty.target.wants/serial-getty@ttyS0.service",
		} {
			mustWriteRootfsFile(t, rootfs, path, "fixture\n")
		}
	case rootfsFlavorAlpineLike:
		for _, path := range []string{
			"/etc/alpine-release",
			"/sbin/openrc",
			"/sbin/init",
			"/lib/systemd/systemd",
			"/etc/init.d/capsule-thaw-agent",
			"/etc/runlevels/default/capsule-thaw-agent",
			"/etc/runlevels/default/networking",
			"/etc/runlevels/default/hostname",
			"/etc/network/interfaces",
			"/etc/inittab",
		} {
			mustWriteRootfsFile(t, rootfs, path, "fixture\n")
		}
	default:
		t.Fatalf("unsupported fixture flavor: %s", flavor)
	}

	return rootfs
}

func binaryFixturePath(binary string) string {
	switch binary {
	case "sh", "mount", "umount", "hostname", "sync", "chown":
		return "/bin/" + binary
	case "ip", "mountpoint", "blkid", "fsfreeze", "update-ca-certificates":
		return "/usr/sbin/" + binary
	case "e2fsck":
		return "/sbin/" + binary
	default:
		return "/usr/bin/" + binary
	}
}

func mustMkdirRootfs(t *testing.T, rootfs, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(rootfs, strings.TrimPrefix(rel, "/")), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
}

func mustWriteRootfsFile(t *testing.T, rootfs, rel, content string) {
	t.Helper()
	path := filepath.Join(rootfs, strings.TrimPrefix(rel, "/"))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func mustWriteExecutable(t *testing.T, rootfs, rel, content string) {
	t.Helper()
	path := filepath.Join(rootfs, strings.TrimPrefix(rel, "/"))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("write executable %s: %v", rel, err)
	}
}

func mustSymlinkRootfs(t *testing.T, rootfs, target, link string) {
	t.Helper()
	linkPath := filepath.Join(rootfs, strings.TrimPrefix(link, "/"))
	if err := os.MkdirAll(filepath.Dir(linkPath), 0755); err != nil {
		t.Fatalf("mkdir for symlink %s: %v", link, err)
	}
	if err := os.RemoveAll(linkPath); err != nil {
		t.Fatalf("remove existing path for symlink %s: %v", link, err)
	}
	if err := os.Symlink(target, linkPath); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}
