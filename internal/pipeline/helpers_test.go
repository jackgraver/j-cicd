package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"j-cicd/internal/config"
)

// writeAndLoad round-trips cfg through YAML and config.Load, so pipeline
// tests exercise the same defaulted config a real run would get.
func writeAndLoad(t *testing.T, cfg *config.Config, revision string) *config.Config {
	t.Helper()

	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	path := filepath.Join(t.TempDir(), config.DefaultFile)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	loaded, err := config.Load(path, revision)
	if err != nil {
		t.Fatalf("load config: %v\n%s", err, data)
	}
	return loaded
}
