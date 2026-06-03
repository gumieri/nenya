package config

import (
	"os"
	"testing"
)

func TestValidatePromptPath_Security(t *testing.T) {
	origDir := os.Getenv("CONFIG_DIR")
	defer os.Setenv("CONFIG_DIR", origDir)

	configDir := t.TempDir()
	os.Setenv("CONFIG_DIR", configDir)

	promptFile, err := os.CreateTemp(configDir, "prompt-*.txt")
	if err != nil {
		t.Fatalf("failed to create temp prompt file: %v", err)
	}
	_ = promptFile.Close()

	tests := []struct {
		name    string
		path    string
		wantErr bool
		reason  string
	}{
		{
			name:    "relative traversal blocked",
			path:    "../../../etc/passwd",
			wantErr: true,
			reason:  "relative path with .. escapes",
		},
		{
			name:    "absolute path outside config dir blocked",
			path:    "/etc/passwd",
			wantErr: true,
			reason:  "absolute path escape",
		},
		{
			name:    "valid file within config dir allowed",
			path:    promptFile.Name(),
			wantErr: false,
			reason:  "within config dir",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePromptPath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("validatePromptPath() error = %v, wantErr %v (%s)", err, tt.wantErr, tt.reason)
			}
		})
	}
}
