package geo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolver_LookupIP_ParsesResponseAndCaches(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "success": true,
  "ip": "8.8.8.8",
  "country": "United States",
  "country_code": "US",
  "region": "California",
  "city": "Mountain View",
  "connection": { "asn": 15169, "isp": "Google LLC" }
}`))
	}))
	defer srv.Close()

	r := NewResolver(ResolverOptions{
		APIBaseURL: srv.URL,
		Timeout:    2 * time.Second,
		CacheTTL:   1 * time.Hour,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	info, err := r.LookupIP(ctx, "8.8.8.8")
	if err != nil {
		t.Fatalf("LookupIP error: %v", err)
	}
	if info.CountryCode != "US" || info.Region != "California" || info.City != "Mountain View" {
		t.Fatalf("unexpected geo info: %+v", info)
	}
	if info.ASN != "AS15169" {
		t.Fatalf("expected ASN AS15169, got %q", info.ASN)
	}

	// Second call should hit cache (same IP).
	info2, err := r.LookupIP(ctx, "8.8.8.8")
	if err != nil {
		t.Fatalf("LookupIP (cache) error: %v", err)
	}
	if info2 == nil || info2.CountryCode != "US" {
		t.Fatalf("unexpected cached geo info: %+v", info2)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 upstream call, got %d", calls.Load())
	}
}
