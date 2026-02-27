package snapshot

// DriveSpec declares how to build and populate one extension drive.
type DriveSpec struct {
	DriveID   string            `json:"drive_id"`             // e.g. "git_drive"
	Label     string            `json:"label"`                // ext4 label, <=16 chars
	SizeGB    int               `json:"size_gb"`
	ReadOnly  bool              `json:"read_only"`            // true = read-only at runtime
	Commands  []SnapshotCommand `json:"commands,omitempty"`   // run inside VM at MountPath
	MountPath string            `json:"mount_path,omitempty"` // path inside VM during population
}

// ExtensionDrive describes a single extension block device and its chunks.
type ExtensionDrive struct {
	Chunks    []ChunkRef `json:"chunks"`
	ReadOnly  bool       `json:"read_only"`
	SizeBytes int64      `json:"size_bytes"`
}
