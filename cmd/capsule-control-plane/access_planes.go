package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// AccessPlaneEntry is the JSON representation of a project_access_planes row.
type AccessPlaneEntry struct {
	ProjectID            string    `json:"project_id"`
	AccessPlaneAddr      string    `json:"access_plane_addr"`
	ProxyAddr            string    `json:"proxy_addr"`
	AttestationSecretRef string    `json:"attestation_secret_ref"`
	CACertPEM            string    `json:"ca_cert_pem,omitempty"`
	TenantID             string    `json:"tenant_id"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// HandleAccessPlanes multiplexes /api/v1/access-planes requests.
func HandleAccessPlanes(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/access-planes")
		path = strings.TrimPrefix(path, "/")

		if path == "" {
			switch r.Method {
			case http.MethodGet:
				handleListAccessPlanes(db, w, r)
			case http.MethodPost:
				handleUpsertAccessPlane(db, w, r)
			default:
				writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
			return
		}

		// path is the project_id
		projectID := path
		switch r.Method {
		case http.MethodGet:
			handleGetAccessPlane(db, w, r, projectID)
		case http.MethodDelete:
			handleDeleteAccessPlane(db, w, r, projectID)
		default:
			writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

func handleListAccessPlanes(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	rows, err := db.QueryContext(r.Context(),
		`SELECT project_id, access_plane_addr, proxy_addr, attestation_secret_ref, COALESCE(ca_cert_pem, ''), tenant_id, created_at, updated_at
		 FROM project_access_planes ORDER BY project_id`)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var entries []AccessPlaneEntry
	for rows.Next() {
		var e AccessPlaneEntry
		if err := rows.Scan(&e.ProjectID, &e.AccessPlaneAddr, &e.ProxyAddr, &e.AttestationSecretRef, &e.CACertPEM, &e.TenantID, &e.CreatedAt, &e.UpdatedAt); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		entries = append(entries, e)
	}
	if entries == nil {
		entries = []AccessPlaneEntry{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"access_planes": entries,
		"count":         len(entries),
	})
}

func handleGetAccessPlane(db *sql.DB, w http.ResponseWriter, r *http.Request, projectID string) {
	var e AccessPlaneEntry
	err := db.QueryRowContext(r.Context(),
		`SELECT project_id, access_plane_addr, proxy_addr, attestation_secret_ref, COALESCE(ca_cert_pem, ''), tenant_id, created_at, updated_at
		 FROM project_access_planes WHERE project_id = $1`, projectID).Scan(
		&e.ProjectID, &e.AccessPlaneAddr, &e.ProxyAddr, &e.AttestationSecretRef, &e.CACertPEM, &e.TenantID, &e.CreatedAt, &e.UpdatedAt)
	if err == sql.ErrNoRows {
		writeAPIError(w, http.StatusNotFound, "access plane not found for project: "+projectID)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(e)
}

func handleUpsertAccessPlane(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectID            string `json:"project_id"`
		AccessPlaneAddr      string `json:"access_plane_addr"`
		ProxyAddr            string `json:"proxy_addr"`
		AttestationSecretRef string `json:"attestation_secret_ref"`
		CACertPEM            string `json:"ca_cert_pem"`
		TenantID             string `json:"tenant_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID == "" {
		writeAPIError(w, http.StatusBadRequest, "project_id is required")
		return
	}
	if req.AccessPlaneAddr == "" || req.ProxyAddr == "" {
		writeAPIError(w, http.StatusBadRequest, "access_plane_addr and proxy_addr are required")
		return
	}
	if req.AttestationSecretRef == "" {
		writeAPIError(w, http.StatusBadRequest, "attestation_secret_ref is required")
		return
	}
	if req.TenantID == "" {
		writeAPIError(w, http.StatusBadRequest, "tenant_id is required")
		return
	}

	_, err := db.ExecContext(r.Context(), `
		INSERT INTO project_access_planes (project_id, access_plane_addr, proxy_addr, attestation_secret_ref, ca_cert_pem, tenant_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (project_id) DO UPDATE SET
			access_plane_addr = EXCLUDED.access_plane_addr,
			proxy_addr = EXCLUDED.proxy_addr,
			attestation_secret_ref = EXCLUDED.attestation_secret_ref,
			ca_cert_pem = EXCLUDED.ca_cert_pem,
			tenant_id = EXCLUDED.tenant_id,
			updated_at = NOW()
	`, req.ProjectID, req.AccessPlaneAddr, req.ProxyAddr, req.AttestationSecretRef, req.CACertPEM, req.TenantID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{
		"project_id": req.ProjectID,
		"status":     "ok",
	})
}

func handleDeleteAccessPlane(db *sql.DB, w http.ResponseWriter, r *http.Request, projectID string) {
	result, err := db.ExecContext(r.Context(),
		`DELETE FROM project_access_planes WHERE project_id = $1`, projectID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		writeAPIError(w, http.StatusNotFound, "access plane not found for project: "+projectID)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"project_id": projectID,
		"status":     "deleted",
	})
}
