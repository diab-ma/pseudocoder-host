package semantic

import "testing"

func TestIsSensitivePath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		// Each canonical token
		{"src/auth/login.go", true},
		{"lib/token_store.dart", true},
		{"certs/ca.pem", true},
		{"config/secret.yaml", true},
		{"ssh/key_manager.go", true},
		{"iam/permission_policy.go", true},
		{"internal/security/audit.go", true},
		{"deploy/policy.rego", true},

		// Case-insensitive
		{"src/AUTH/Login.go", true},
		{"lib/TokenStore.dart", true},
		{"SECRET_CONFIG.yaml", true},

		// Substring match (token embedded in larger word)
		{"src/authentication/handler.go", true},
		{"lib/tokenizer.dart", true},
		{"pkg/certificate.go", true},

		// Non-sensitive paths
		{"main.go", false},
		{"README.md", false},
		{"lib/models/user.dart", false},
		{"internal/diff/poller.go", false},
		{"test/widget_test.dart", false},

		// Empty path
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := IsSensitivePath(tt.path)
			if got != tt.want {
				t.Errorf("IsSensitivePath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
