package errors

import (
	"errors"
	"testing"
)

func TestCodedError_Error(t *testing.T) {
	tests := []struct {
		name     string
		err      *CodedError
		expected string
	}{
		{
			name:     "error without cause",
			err:      New(CodeStorageNotFound, "card not found"),
			expected: "storage.not_found: card not found",
		},
		{
			name:     "error with cause",
			err:      Wrap(CodeActionGitFailed, "git add failed", errors.New("exit status 1")),
			expected: "action.git_failed: git add failed (exit status 1)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.expected {
				t.Errorf("Error() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestCodedError_Unwrap(t *testing.T) {
	cause := errors.New("original error")
	err := Wrap(CodeInternal, "wrapped", cause)

	if err.Unwrap() != cause {
		t.Error("Unwrap() should return the original cause")
	}

	// Test without cause
	err2 := New(CodeStorageNotFound, "not found")
	if err2.Unwrap() != nil {
		t.Error("Unwrap() should return nil when no cause")
	}
}

func TestGetCode(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected string
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: "",
		},
		{
			name:     "CodedError",
			err:      New(CodeStorageNotFound, "not found"),
			expected: CodeStorageNotFound,
		},
		{
			name:     "wrapped CodedError",
			err:      Wrap(CodeActionGitFailed, "failed", errors.New("cause")),
			expected: CodeActionGitFailed,
		},
		{
			name:     "plain error",
			err:      errors.New("some error"),
			expected: CodeUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GetCode(tt.err); got != tt.expected {
				t.Errorf("GetCode() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestGetMessage(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected string
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: "",
		},
		{
			name:     "CodedError",
			err:      New(CodeStorageNotFound, "card not found"),
			expected: "card not found",
		},
		{
			name:     "plain error",
			err:      errors.New("some error"),
			expected: "some error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GetMessage(tt.err); got != tt.expected {
				t.Errorf("GetMessage() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestToCodeAndMessage(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantCode    string
		wantMessage string
	}{
		{
			name:        "nil error",
			err:         nil,
			wantCode:    "",
			wantMessage: "",
		},
		{
			name:        "CodedError",
			err:         New(CodeStorageNotFound, "card not found"),
			wantCode:    CodeStorageNotFound,
			wantMessage: "card not found",
		},
		{
			name:        "plain error",
			err:         errors.New("some error"),
			wantCode:    CodeUnknown,
			wantMessage: "some error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, message := ToCodeAndMessage(tt.err)
			if code != tt.wantCode {
				t.Errorf("ToCodeAndMessage() code = %q, want %q", code, tt.wantCode)
			}
			if message != tt.wantMessage {
				t.Errorf("ToCodeAndMessage() message = %q, want %q", message, tt.wantMessage)
			}
		})
	}
}

func TestIsCode(t *testing.T) {
	err := New(CodeStorageNotFound, "not found")

	if !IsCode(err, CodeStorageNotFound) {
		t.Error("IsCode() should return true for matching code")
	}

	if IsCode(err, CodeActionGitFailed) {
		t.Error("IsCode() should return false for non-matching code")
	}

	if IsCode(nil, CodeStorageNotFound) {
		t.Error("IsCode() should return false for nil error")
	}
}

func TestErrorConstructors(t *testing.T) {
	t.Run("NotFound", func(t *testing.T) {
		err := NotFound("card")
		if !IsCode(err, CodeStorageNotFound) {
			t.Errorf("NotFound() code = %q, want %q", GetCode(err), CodeStorageNotFound)
		}
		if err.Message != "card not found" {
			t.Errorf("NotFound() message = %q, want %q", err.Message, "card not found")
		}
	})

	t.Run("AlreadyDecided", func(t *testing.T) {
		err := AlreadyDecided("card-123")
		if !IsCode(err, CodeStorageAlreadyDecided) {
			t.Errorf("AlreadyDecided() code = %q, want %q", GetCode(err), CodeStorageAlreadyDecided)
		}
		if err.Message != "card card-123 already has a decision" {
			t.Errorf("AlreadyDecided() message = %q", err.Message)
		}
	})

	t.Run("InvalidAction", func(t *testing.T) {
		err := InvalidAction("maybe")
		if !IsCode(err, CodeActionInvalid) {
			t.Errorf("InvalidAction() code = %q, want %q", GetCode(err), CodeActionInvalid)
		}
		if err.Message != "invalid action: maybe (must be 'accept' or 'reject')" {
			t.Errorf("InvalidAction() message = %q", err.Message)
		}
	})

	t.Run("GitFailed", func(t *testing.T) {
		cause := errors.New("exit status 1")
		err := GitFailed("add", "file.txt", cause)
		if !IsCode(err, CodeActionGitFailed) {
			t.Errorf("GitFailed() code = %q, want %q", GetCode(err), CodeActionGitFailed)
		}
		if err.Message != "git add failed for file.txt" {
			t.Errorf("GitFailed() message = %q", err.Message)
		}
		if err.Cause != cause {
			t.Error("GitFailed() should preserve cause")
		}
	})

	t.Run("InvalidMessage", func(t *testing.T) {
		err := InvalidMessage("missing card_id")
		if !IsCode(err, CodeServerInvalidMessage) {
			t.Errorf("InvalidMessage() code = %q, want %q", GetCode(err), CodeServerInvalidMessage)
		}
	})

	t.Run("Internal", func(t *testing.T) {
		cause := errors.New("db connection lost")
		err := Internal("database error", cause)
		if !IsCode(err, CodeInternal) {
			t.Errorf("Internal() code = %q, want %q", GetCode(err), CodeInternal)
		}
		if err.Cause != cause {
			t.Error("Internal() should preserve cause")
		}
	})
}

func TestErrorsAs(t *testing.T) {
	// Test that errors.As works with wrapped errors
	cause := errors.New("original")
	coded := Wrap(CodeActionGitFailed, "wrapped", cause)
	wrapped := Wrap(CodeInternal, "double wrapped", coded)

	var target *CodedError
	if !errors.As(wrapped, &target) {
		t.Error("errors.As should find CodedError in chain")
	}
	if target.Code != CodeInternal {
		t.Errorf("errors.As should find outermost CodedError, got code %q", target.Code)
	}
}

func TestErrorCodes(t *testing.T) {
	// Verify error code format is {domain}.{error}
	codes := []string{
		CodeStorageNotFound,
		CodeStorageAlreadyExists,
		CodeStorageAlreadyDecided,
		CodeStorageOpenFailed,
		CodeStorageQueryFailed,
		CodeStorageSaveFailed,
		CodeActionInvalid,
		CodeActionGitFailed,
		CodeActionCardNotFound,
		CodeActionAlreadyDecided,
		CodeServerUpgradeFailed,
		CodeServerInvalidMessage,
		CodeServerHandlerMissing,
		CodeServerSendFailed,
		CodeServerConnectionLost,
		CodeSessionAlreadyRunning,
		CodeSessionNotRunning,
		CodeSessionSpawnFailed,
		CodeSessionWriteFailed,
		CodeDiffParseFailed,
		CodeDiffPollFailed,
		CodeAuthRequired,
		CodeAuthInvalid,
		CodeAuthExpired,
		CodeAuthDeviceRevoked,
		CodeUnknown,
		CodeInternal,
	}

	for _, code := range codes {
		if code == "" {
			t.Error("error code should not be empty")
			continue
		}

		// Check format: should contain a dot
		hasDot := false
		for _, c := range code {
			if c == '.' {
				hasDot = true
				break
			}
		}
		if !hasDot {
			t.Errorf("error code %q should be in format {domain}.{error}", code)
		}
	}
}
