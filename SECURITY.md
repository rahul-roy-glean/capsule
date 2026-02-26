# Security Policy

## Reporting a Vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

If you discover a security vulnerability in bazel-firecracker, please report it responsibly through one of the following channels:

### Option 1: GitHub Security Advisories (Preferred)

Use [GitHub's private vulnerability reporting](../../security/advisories/new) to report the issue confidentially. This allows us to coordinate disclosure before any public announcement.

### Option 2: Email

Send a report to the maintainers via email. Include "bazel-firecracker security" in the subject line. You can find maintainer contact information in the repository's GitHub profile.

---

## What to Include in Your Report

Please include as much of the following as possible:

- A description of the vulnerability and its potential impact
- The component(s) affected (e.g., control plane, firecracker-manager, UFFD handler)
- Steps to reproduce the issue
- Any proof-of-concept code or exploit
- Your suggested fix, if you have one

---

## What Constitutes a Security Issue

The following categories are considered security vulnerabilities for this project:

**VM Isolation Bypass**
- Any technique that allows code running inside a Firecracker microVM to escape to the host or access another VM's memory, disk, or processes.
- Exploitation of the UFFD (userfaultfd) memory handler to read or write host memory outside the intended VM address space.

**Privilege Escalation**
- Exploiting the firecracker-manager (which runs as root on hosts) to gain elevated privileges beyond the intended scope.
- Bypassing the security boundaries between the control plane (GKE) and host VMs.

**Credential and Secret Exposure**
- Vulnerabilities that expose GitHub App private keys, database credentials, or GCS access tokens.
- Leaking MMDS tokens or GitHub runner registration tokens between VM instances.

**Network Isolation Bypass**
- Techniques that allow a microVM to reach networks or services outside its intended sandbox (e.g., host metadata APIs, other tenants' VMs, internal GCP services).
- Bypassing the iptables/bridge network isolation between microVMs.

**Snapshot Integrity**
- Tampering with Firecracker snapshots in GCS to execute arbitrary code when a snapshot is restored.
- Injection attacks via the rootfs image or kernel.

---

## Response Timeline

We take security reports seriously. Our response targets are:

| Milestone | Target |
|-----------|--------|
| Acknowledgment of report | Within 3 business days |
| Initial assessment and triage | Within 7 business days |
| Status update | Every 7 days until resolved |
| Fix and coordinated disclosure | Within 90 days for most issues |

We will coordinate the public disclosure date with you. We ask that you keep the vulnerability confidential until we have released a fix.

---

## Scope

This security policy applies to the code in this repository. It does not cover:

- Vulnerabilities in upstream projects (Firecracker, Linux kernel, Go standard library). Please report those to the respective upstream projects.
- Security issues in your own deployment configuration or infrastructure.
- Issues that require physical access to the host hardware.

---

## Acknowledgments

We appreciate the work of security researchers who responsibly disclose vulnerabilities. With your permission, we will acknowledge your contribution in the release notes when we publish a fix.
