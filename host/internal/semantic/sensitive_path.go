package semantic

import "strings"

// sensitivePathTokens is the canonical list of substrings that flag a
// file as security-sensitive. This is the single source of truth used
// by both B1 card-risk (server/message.go) and C2 semantic-group risk.
var sensitivePathTokens = []string{
	"auth", "token", "cert", "secret", "key", "permission", "security", "policy",
}

// IsSensitivePath returns true when path contains any sensitive token
// (case-insensitive substring match).
func IsSensitivePath(path string) bool {
	lower := strings.ToLower(path)
	for _, tok := range sensitivePathTokens {
		if strings.Contains(lower, tok) {
			return true
		}
	}
	return false
}
