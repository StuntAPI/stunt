package har

import "testing"

func TestParameterizePath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/users/42/orders", "/users/{id}/orders"},
		{"/v1/charges/ch_abc123def", "/v1/charges/{id}"},
		{"/users/550e8400-e29b-41d4-a716-446655440000", "/users/{id}"},
		{"/repos/abc123def456ghi789jklmno", "/repos/{id}"},   // 24-char opaque id
		{"/users/{userId}/orders", "/users/{userId}/orders"}, // preserved
		{"/api/v1/users", "/api/v1/users"},                   // no IDs
		{"/", "/"},
		{"/items/ghp_1234567890abcdef/tokens", "/items/{id}/tokens"},
		{"/accounts/AKIA1234567890ABC", "/accounts/{id}"},
		{"/xoxb-1234567890-abcdef/info", "/{id}/info"}, // Slack token prefix + mixed alnum → parameterized
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parameterizePath(tt.input)
			if got != tt.want {
				t.Errorf("parameterizePath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestLooksLikeID(t *testing.T) {
	ids := []string{
		"42", "12345",
		"550e8400-e29b-41d4-a716-446655440000",
		"ch_abc123", "cus_xyz123ABC",
		"ghp_1234567890abcdef", "AKIA12345678AB",
		"abc123def456ghi789", // 18 chars
		"sk_live_abc123def456",
		"AIzaSyABCDEFGHIJKLMN0123456789",
		"xoxb-1234567890-abcdef",
	}
	for _, s := range ids {
		if !looksLikeID(s) {
			t.Errorf("looksLikeID(%q) = false, want true", s)
		}
	}
	notIDs := []string{
		"", "users", "orders", "api", "v1",
		"{userId}", // already a placeholder
		"real", "health", "items", "products",
	}
	for _, s := range notIDs {
		if looksLikeID(s) {
			t.Errorf("looksLikeID(%q) = true, want false", s)
		}
	}
}
