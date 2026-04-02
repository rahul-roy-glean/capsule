-- project_access_planes stores per-project access plane deployment info.
-- Each project gets a dedicated access plane instance that handles credential
-- injection, policy enforcement, and audit logging for its VMs.
CREATE TABLE IF NOT EXISTS project_access_planes (
    project_id TEXT PRIMARY KEY,
    access_plane_addr TEXT NOT NULL,
    proxy_addr TEXT NOT NULL,
    attestation_secret_ref TEXT NOT NULL,
    ca_cert_pem TEXT,
    tenant_id TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);
