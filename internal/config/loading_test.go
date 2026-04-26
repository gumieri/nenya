package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPromptFile_DirectPrompt(t *testing.T) {
	got, err := LoadPromptFile("", "hello world", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello world" {
		t.Errorf("expected direct prompt, got %q", got)
	}
}

func TestLoadPromptFile_EmptyPath(t *testing.T) {
	got, err := LoadPromptFile("", "", "default prompt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "default prompt" {
		t.Errorf("expected default prompt, got %q", got)
	}
}

func TestLoadPromptFile_ValidFile(t *testing.T) {
	tmpDir := t.TempDir()
	promptFile := filepath.Join(tmpDir, "prompt.txt")
	if err := os.WriteFile(promptFile, []byte("custom prompt"), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadPromptFile(promptFile, "", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "custom prompt" {
		t.Errorf("expected file content, got %q", got)
	}
}

func TestLoadPromptFile_NonexistentFile(t *testing.T) {
	_, err := LoadPromptFile("/nonexistent/prompt.txt", "", "default")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestLoadPromptFile_PathTraversal(t *testing.T) {
	t.Setenv("CONFIG_DIR", "/etc/nenya")
	_, err := LoadPromptFile("../../etc/passwd", "", "default")
	if err == nil {
		t.Fatal("expected error for path traversal attempt")
	}
}

func TestLoadPromptFile_RelativePathInsideConfigDir(t *testing.T) {
	tmpDir := t.TempDir()
	promptFile := filepath.Join(tmpDir, "prompt.txt")
	if err := os.WriteFile(promptFile, []byte("allowed prompt"), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_DIR", tmpDir)
	got, err := LoadPromptFile(promptFile, "", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "allowed prompt" {
		t.Errorf("expected allowed prompt, got %q", got)
	}
}
