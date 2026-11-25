package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestArrayConfigReadsSecretFiles(t *testing.T) {
	dir := t.TempDir()
	pw := filepath.Join(dir, "password")
	if err := os.WriteFile(pw, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SC_HOST", "array.example")
	t.Setenv("SC_USER", "csi")
	t.Setenv("SC_PASSWORD", "ignored")
	t.Setenv("SC_PASSWORD_FILE", pw)
	t.Setenv("SC_PORT", "")
	cfg, err := arrayConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Password != "s3cret" || cfg.Port != "3033" || cfg.Host != "array.example" {
		t.Errorf("got %+v", cfg)
	}

	t.Setenv("SC_PASSWORD_FILE", filepath.Join(dir, "missing"))
	if _, err := arrayConfig(); err == nil {
		t.Error("missing password file: want error")
	}
	t.Setenv("SC_PASSWORD_FILE", "")
	t.Setenv("SC_HOST", "")
	if _, err := arrayConfig(); err == nil {
		t.Error("no host: want error")
	}
}
