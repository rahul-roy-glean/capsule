export interface FileEntry {
  name: string
  path?: string
  is_dir?: boolean
  size?: number
  mode?: string
}

export interface FileReadResult {
  content?: string
  encoding?: string
  size?: number
}

export interface FileWriteResult {
  success?: boolean
  bytes_written?: number
}

export interface FileUploadResult {
  success?: boolean
  bytes_written?: number
}

export interface FileListResult {
  entries: FileEntry[]
}

export interface FileStatResult {
  exists?: boolean
  size?: number
  mode?: string
  is_dir?: boolean
}

export interface FileRemoveResult {
  success?: boolean
  removed?: boolean
}

export interface FileMkdirResult {
  success?: boolean
}
