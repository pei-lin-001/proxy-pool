package builder

import (
	"errors"
	"testing"

	"easy_proxies/internal/config"
)

func TestBuild_NoNodes_ReturnsErrNoValidNodes(t *testing.T) {
	_, err := Build(&config.Config{})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, ErrNoValidNodes) {
		t.Fatalf("expected ErrNoValidNodes, got %v", err)
	}
}
