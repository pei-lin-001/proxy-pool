package config

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"easy_proxies/internal/geo"
)

var defaultGeoResolver = geo.NewResolver(geo.ResolverOptions{})

// PopulateNodeGeo resolves each node's server host to an IP and attaches GeoIP metadata to NodeConfig.Geo.
//
// This is best-effort: it never returns an error if some nodes fail; failed nodes will get Geo.Error set.
func (c *Config) PopulateNodeGeo(ctx context.Context) {
	if c == nil || len(c.Nodes) == 0 {
		return
	}

	workerLimit := runtime.NumCPU() * 2
	if workerLimit < 8 {
		workerLimit = 8
	}
	if workerLimit > 32 {
		workerLimit = 32
	}

	sem := make(chan struct{}, workerLimit)
	var wg sync.WaitGroup

	for i := range c.Nodes {
		// Skip if already populated successfully.
		if c.Nodes[i].Geo != nil && c.Nodes[i].Geo.Error == "" && c.Nodes[i].Geo.CountryCode != "" {
			continue
		}

		sem <- struct{}{}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()

			host, err := extractNodeServerHost(c.Nodes[idx].URI)
			if err != nil {
				c.Nodes[idx].Geo = &geo.Info{Error: err.Error()}
				return
			}

			info, err := defaultGeoResolver.LookupHost(ctx, host)
			if err != nil {
				c.Nodes[idx].Geo = &geo.Info{Error: err.Error()}
				return
			}
			// Copy to detach from shared cache pointer (safe for mutation/extending later).
			c.Nodes[idx].Geo = &geo.Info{
				IP:          info.IP,
				Country:     info.Country,
				CountryCode: info.CountryCode,
				Region:      info.Region,
				City:        info.City,
				ASN:         info.ASN,
				ISP:         info.ISP,
			}
		}(i)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	// Avoid blocking indefinitely if caller provides a canceled context with no deadline.
	select {
	case <-done:
	case <-ctx.Done():
		// Give remaining workers a short grace period to exit.
		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func extractNodeServerHost(rawURI string) (string, error) {
	rawURI = strings.TrimSpace(rawURI)
	if rawURI == "" {
		return "", fmt.Errorf("empty uri")
	}

	u, err := url.Parse(rawURI)
	if err != nil {
		return "", fmt.Errorf("parse uri: %w", err)
	}

	switch strings.ToLower(strings.TrimSpace(u.Scheme)) {
	case "vmess":
		return extractHostFromVMessURI(rawURI)
	default:
		host := strings.TrimSpace(u.Hostname())
		if host == "" {
			return "", fmt.Errorf("missing host")
		}
		return host, nil
	}
}

type vmessJSONServer struct {
	Add string `json:"add"`
}

func extractHostFromVMessURI(uri string) (string, error) {
	encoded := strings.TrimSpace(strings.TrimPrefix(uri, "vmess://"))
	if encoded == "" {
		return "", fmt.Errorf("vmess missing payload")
	}
	decoded, err := decodeBase64Any(encoded)
	if err != nil {
		return "", fmt.Errorf("decode vmess base64: %w", err)
	}
	var payload vmessJSONServer
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return "", fmt.Errorf("parse vmess json: %w", err)
	}
	host := strings.TrimSpace(payload.Add)
	if host == "" {
		return "", fmt.Errorf("vmess missing server address")
	}
	return host, nil
}

// NeedsGeo reports whether the filter target requires GeoIP metadata.
func (f NodeFilterConfig) NeedsGeo() bool {
	switch strings.ToLower(strings.TrimSpace(f.Target)) {
	case "geo", "name_or_geo":
		return true
	default:
		return false
	}
}
