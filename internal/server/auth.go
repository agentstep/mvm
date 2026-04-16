package server

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

// authMiddleware checks for a valid Bearer token on every request except GET /health.
func authMiddleware(apiKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for health checks.
		if r.Method == http.MethodGet && r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") || strings.TrimPrefix(header, "Bearer ") != apiKey {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}

		next.ServeHTTP(w, r)
	})
}

// LoadAPIKey returns the first non-empty value from: flagValue, MVM_API_KEY env var,
// contents of filePath, contents of /etc/mvm/api-key.
func LoadAPIKey(flagValue, filePath string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("MVM_API_KEY"); v != "" {
		return v
	}
	if filePath != "" {
		if data, err := os.ReadFile(filePath); err == nil {
			if s := strings.TrimSpace(string(data)); s != "" {
				return s
			}
		}
	}
	if data, err := os.ReadFile("/etc/mvm/api-key"); err == nil {
		if s := strings.TrimSpace(string(data)); s != "" {
			return s
		}
	}
	return ""
}
