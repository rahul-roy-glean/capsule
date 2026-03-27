export class CapsuleError extends Error {
  constructor(message: string) {
    super(message)
    this.name = new.target.name
  }
}

export class CapsuleHTTPError extends CapsuleError {
  statusCode: number
  requestId?: string
  details?: Record<string, unknown>
  message: string

  constructor(
    statusCode: number,
    message: string,
    options: { requestId?: string; details?: Record<string, unknown> } = {},
  ) {
    super(`HTTP ${statusCode}: ${message}`)
    this.statusCode = statusCode
    this.message = message
    this.requestId = options.requestId
    this.details = options.details
  }
}

export class CapsuleAuthError extends CapsuleHTTPError {
  constructor(message = 'Unauthorized', options: { requestId?: string; details?: Record<string, unknown> } = {}) {
    super(401, message, options)
  }
}

export class CapsuleNotFound extends CapsuleHTTPError {
  constructor(message = 'Not found', options: { requestId?: string; details?: Record<string, unknown> } = {}) {
    super(404, message, options)
  }
}

export class CapsuleConflict extends CapsuleHTTPError {
  constructor(message = 'Conflict', options: { requestId?: string; details?: Record<string, unknown> } = {}) {
    super(409, message, options)
  }
}

export class CapsuleRateLimited extends CapsuleHTTPError {
  retryAfter?: number

  constructor(
    message = 'Rate limited',
    options: { requestId?: string; details?: Record<string, unknown>; retryAfter?: number } = {},
  ) {
    super(429, message, options)
    this.retryAfter = options.retryAfter
  }
}

export class CapsuleServiceUnavailable extends CapsuleHTTPError {
  retryAfter?: number

  constructor(
    message = 'Service unavailable',
    options: { requestId?: string; details?: Record<string, unknown>; retryAfter?: number } = {},
  ) {
    super(503, message, options)
    this.retryAfter = options.retryAfter
  }
}

export class CapsuleConnectionError extends CapsuleError {}

export class CapsuleTimeoutError extends CapsuleError {
  requestId?: string
  runnerId?: string
  timeout?: number
  operation?: string
  message: string

  constructor(
    message = 'Timed out',
    options: { requestId?: string; runnerId?: string; timeout?: number; operation?: string } = {},
  ) {
    super(message)
    this.message = message
    this.requestId = options.requestId
    this.runnerId = options.runnerId
    this.timeout = options.timeout
    this.operation = options.operation
  }
}

export class CapsuleRequestTimeoutError extends CapsuleTimeoutError {}

export class CapsuleOperationTimeoutError extends CapsuleTimeoutError {}

export class CapsuleAllocationTimeoutError extends CapsuleTimeoutError {
  workloadKey: string

  constructor(
    message: string,
    options: { workloadKey: string; requestId?: string; timeout?: number },
  ) {
    super(message, {
      requestId: options.requestId,
      timeout: options.timeout,
      operation: 'allocate',
    })
    this.workloadKey = options.workloadKey
  }
}

export class CapsuleRunnerUnavailableError extends CapsuleError {
  runnerId: string
  status?: string
  retryAfter?: number
  message: string

  constructor(
    message: string,
    options: { runnerId: string; status?: string; retryAfter?: number },
  ) {
    super(message)
    this.message = message
    this.runnerId = options.runnerId
    this.status = options.status
    this.retryAfter = options.retryAfter
  }
}
