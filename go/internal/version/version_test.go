package version

import "testing"

func TestVersion(t *testing.T) {
	tests := []struct {
		name     string
		got      string
		expected string
	}{
		{
			name:     "constant version check",
			got:      Version,
			expected: "0.0.1",
		},
		{
			name:     "getter version check",
			got:      GetVersion(),
			expected: "0.0.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.expected {
				t.Errorf("got %q, want %q", tt.got, tt.expected)
			}
		})
	}
}
