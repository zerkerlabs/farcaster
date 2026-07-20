package httpapi

import "testing"

func TestValidateUpstreamURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"valid https", "https://agent.example.com/invoke", false},
		{"valid http", "http://agent.example.com:8080/run", false},
		{"valid public ip", "https://203.0.113.10/invoke", false},

		{"empty", "", true},
		{"no scheme", "agent.example.com/invoke", true},
		{"non-http scheme", "ftp://agent.example.com/x", true},
		{"file scheme", "file:///etc/passwd", true},
		{"no host", "https:///invoke", true},

		{"localhost", "http://localhost:8080/", true},
		{"loopback v4", "http://127.0.0.1/", true},
		{"loopback v6", "http://[::1]/", true},
		{"unspecified", "http://0.0.0.0/", true},
		{"cloud metadata (link-local)", "http://169.254.169.254/latest/meta-data/", true},
		{"private 10/8", "http://10.0.0.5/", true},
		{"private 172.16/12", "http://172.16.4.4/", true},
		{"private 192.168/16", "http://192.168.1.1/", true},
		{"unique-local v6", "http://[fc00::1]/", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateUpstreamURL(tc.url)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateUpstreamURL(%q) error = %v, wantErr = %v", tc.url, err, tc.wantErr)
			}
		})
	}
}
