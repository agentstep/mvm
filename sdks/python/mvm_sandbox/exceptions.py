"""Exception types for the mvm sandbox SDK."""


class MVMError(Exception):
    """Base exception for mvm sandbox errors."""

    def __init__(self, message: str, status_code: int = 0):
        super().__init__(message)
        self.status_code = status_code


class AuthError(MVMError):
    """Raised on 401 Unauthorized responses."""

    pass


class NotFoundError(MVMError):
    """Raised on 404 Not Found responses."""

    pass


class ConflictError(MVMError):
    """Raised on 409 Conflict responses."""

    pass


class ServerError(MVMError):
    """Raised on 5xx server error responses."""

    pass
