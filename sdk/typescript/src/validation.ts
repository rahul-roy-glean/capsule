const VALID_CONFIG_ID = /^[a-z0-9][a-z0-9-]*[a-z0-9]$/;

export function validateConfigId(configId: string): void {
  if (configId.length < 3 || configId.length > 64) {
    throw new Error(
      `config_id must be 3-64 characters, got ${configId.length}: ${JSON.stringify(configId)}`,
    );
  }
  if (!VALID_CONFIG_ID.test(configId)) {
    throw new Error(
      `config_id must be lowercase alphanumeric with hyphens (no leading/trailing hyphens): ${JSON.stringify(configId)}`,
    );
  }
}
