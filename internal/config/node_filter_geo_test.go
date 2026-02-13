package config

import (
	"testing"

	"easy_proxies/internal/geo"
)

func TestFilterNodes_TargetGeo(t *testing.T) {
	nodes := []NodeConfig{
		{Name: "a", URI: "vless://a", Geo: &geo.Info{CountryCode: "US", Region: "California", City: "Los Angeles"}},
		{Name: "b", URI: "vless://b", Geo: &geo.Info{CountryCode: "JP", Region: "Tokyo"}},
	}
	filter := NodeFilterConfig{
		Target:  "geo",
		Include: []string{"US"},
	}
	out := FilterNodes(nodes, filter)
	if len(out) != 1 {
		t.Fatalf("expected 1 node, got %d", len(out))
	}
	if out[0].Name != "a" {
		t.Fatalf("expected node a, got %q", out[0].Name)
	}
}

func TestFilterNodes_TargetGeo_FallbackWhenNoGeoAvailable(t *testing.T) {
	nodes := []NodeConfig{
		{Name: "a", URI: "vless://a"},
		{Name: "b", URI: "vless://b"},
	}
	filter := NodeFilterConfig{
		Target:  "geo",
		Include: []string{"US"},
	}
	out := FilterNodes(nodes, filter)
	if len(out) != len(nodes) {
		t.Fatalf("expected fallback to include all nodes, got %d", len(out))
	}
}

func TestFilterNodes_TargetNameOrGeo(t *testing.T) {
	nodes := []NodeConfig{
		{Name: "HK node", URI: "vless://a", Geo: &geo.Info{CountryCode: "US"}},
		{Name: "JP node", URI: "vless://b", Geo: &geo.Info{CountryCode: "JP"}},
	}
	filter := NodeFilterConfig{
		Target:  "name_or_geo",
		Include: []string{"US"},
	}
	out := FilterNodes(nodes, filter)
	if len(out) != 1 {
		t.Fatalf("expected 1 node, got %d", len(out))
	}
	if out[0].Name != "HK node" {
		t.Fatalf("expected HK node (matched by geo), got %q", out[0].Name)
	}
}
