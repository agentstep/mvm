/** Base error for all mvm API errors. */
export class MVMError extends Error {
  constructor(message: string, public statusCode: number = 0) {
    super(message);
    this.name = 'MVMError';
  }
}

/** Thrown when the server returns 401 Unauthorized. */
export class AuthError extends MVMError {
  constructor(message: string) {
    super(message, 401);
    this.name = 'AuthError';
  }
}

/** Thrown when the server returns 404 Not Found. */
export class NotFoundError extends MVMError {
  constructor(message: string) {
    super(message, 404);
    this.name = 'NotFoundError';
  }
}

/** Thrown when the server returns 409 Conflict. */
export class ConflictError extends MVMError {
  constructor(message: string) {
    super(message, 409);
    this.name = 'ConflictError';
  }
}

/** Thrown when the server returns 5xx. */
export class ServerError extends MVMError {
  constructor(message: string, statusCode: number = 500) {
    super(message, statusCode);
    this.name = 'ServerError';
  }
}
