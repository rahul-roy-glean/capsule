export type SnapshotCommand = {
  type?: string
  args?: string[]
  command?: string
  [key: string]: unknown
}

export type SnapshotCommandInput = string | SnapshotCommand

export function normalizeSnapshotCommand(
  command: SnapshotCommandInput,
): Record<string, unknown> {
  if (typeof command === 'string') {
    return { type: 'shell', args: ['bash', '-lc', command] }
  }

  const normalized: Record<string, unknown> = { ...command }
  if ('type' in normalized && 'args' in normalized) {
    return normalized
  }

  const legacyCommand = normalized.command
  delete normalized.command
  if (typeof legacyCommand === 'string') {
    normalized.type = typeof normalized.type === 'string' ? normalized.type : 'shell'
    normalized.args = ['bash', '-lc', legacyCommand]
    return normalized
  }

  if ('args' in normalized && !('type' in normalized)) {
    normalized.type = 'shell'
  }

  return normalized
}

export function normalizeSnapshotCommands(
  commands: Array<SnapshotCommandInput> | undefined,
): Array<Record<string, unknown>> | undefined {
  if (commands === undefined) {
    return undefined
  }
  return commands.map((command) => normalizeSnapshotCommand(command))
}
