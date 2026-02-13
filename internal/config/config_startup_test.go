package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_AllowsEmptyNodes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(path, []byte("mode: pool\nmanagement:\n  enabled: true\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg == nil {
		t.Fatalf("expected config, got nil")
	}
	if len(cfg.Nodes) != 0 {
		t.Fatalf("expected 0 nodes, got %d", len(cfg.Nodes))
	}
}

func TestLoad_AllowsMissingNodesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// nodes_file points to a file that doesn't exist yet.
	if err := os.WriteFile(path, []byte("mode: pool\nnodes_file: nodes.txt\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg == nil {
		t.Fatalf("expected config, got nil")
	}
	if len(cfg.Nodes) != 0 {
		t.Fatalf("expected 0 nodes, got %d", len(cfg.Nodes))
	}
}
