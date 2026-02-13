package geo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type ResolverOptions struct {
	// APIBaseURL is the base URL for the GeoIP API.
	// Default: https://ipwho.is/
	APIBaseURL string

	// Timeout is the per-request timeout for DNS + HTTP.
	// Default: 5s
	Timeout time.Duration

	// CacheTTL controls how long successful IP lookups are cached in-memory.
	// Default: 7d
	CacheTTL time.Duration

	// HostCacheTTL controls how long DNS resolutions (host->ip) are cached in-memory.
	// Default: 1h
	HostCacheTTL time.Duration

	// Language controls the response language for GeoIP text fields (country/region/city).
	// It is passed to the upstream API as the "lang" query parameter (e.g. "zh-CN").
	// Default: "zh-CN" (to match the built-in WebUI language).
	Language string
}

type Resolver struct {
	baseURL      string
	timeout      time.Duration
	cacheTTL     time.Duration
	hostCacheTTL time.Duration
	language     string

	httpDirect *http.Client
	httpEnv    *http.Client

	ipGroup   singleflight.Group
	hostGroup singleflight.Group

	ipCache   sync.Map // ip -> cachedIP
	hostCache sync.Map // host -> cachedHost
}

type cachedIP struct {
	info    *Info
	expires time.Time
}

type cachedHost struct {
	ip      string
	expires time.Time
}

func NewResolver(opts ResolverOptions) *Resolver {
	baseURL := strings.TrimSpace(opts.APIBaseURL)
	if baseURL == "" {
		baseURL = "https://ipwho.is/"
	}
	baseURL = strings.TrimRight(baseURL, "/") + "/"

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	cacheTTL := opts.CacheTTL
	if cacheTTL <= 0 {
		cacheTTL = 7 * 24 * time.Hour
	}
	hostCacheTTL := opts.HostCacheTTL
	if hostCacheTTL <= 0 {
		hostCacheTTL = 1 * time.Hour
	}
	language := strings.TrimSpace(opts.Language)
	if language == "" {
		language = "zh-CN"
	}

	directTransport := http.DefaultTransport.(*http.Transport).Clone()
	directTransport.Proxy = nil
	directTransport.ProxyConnectHeader = nil

	return &Resolver{
		baseURL:      baseURL,
		timeout:      timeout,
		cacheTTL:     cacheTTL,
		hostCacheTTL: hostCacheTTL,
		language:     language,
		httpDirect:   &http.Client{Timeout: timeout, Transport: directTransport},
		httpEnv:      &http.Client{Timeout: timeout},
	}
}

// LookupHost resolves host to a public IP and returns its GeoIP metadata.
func (r *Resolver) LookupHost(ctx context.Context, host string) (*Info, error) {
	if r == nil {
		return nil, errors.New("geo resolver is nil")
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, errors.New("empty host")
	}

	// Fast-path: host is already an IP.
	if addr, err := netip.ParseAddr(host); err == nil {
		if !isPublicAddr(addr) {
			return nil, fmt.Errorf("non-public ip: %s", host)
		}
		return r.LookupIP(ctx, addr.String())
	}

	// Cached host->ip
	if v, ok := r.hostCache.Load(host); ok {
		if entry, ok := v.(cachedHost); ok && time.Now().Before(entry.expires) && entry.ip != "" {
			return r.LookupIP(ctx, entry.ip)
		}
	}

	ctx, cancel := withBoundedTimeout(ctx, r.timeout)
	defer cancel()
	v, err, _ := r.hostGroup.Do(host, func() (any, error) {
		ip, err := resolveHostToPublicIP(ctx, host)
		if err != nil {
			return "", err
		}
		r.hostCache.Store(host, cachedHost{ip: ip, expires: time.Now().Add(r.hostCacheTTL)})
		return ip, nil
	})
	if err != nil {
		return nil, err
	}
	ip, _ := v.(string)
	if strings.TrimSpace(ip) == "" {
		return nil, fmt.Errorf("resolve host %q: empty ip", host)
	}
	return r.LookupIP(ctx, ip)
}

// LookupIP returns GeoIP metadata for the given public IP.
func (r *Resolver) LookupIP(ctx context.Context, ip string) (*Info, error) {
	if r == nil {
		return nil, errors.New("geo resolver is nil")
	}
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return nil, errors.New("empty ip")
	}
	if addr, err := netip.ParseAddr(ip); err != nil {
		return nil, fmt.Errorf("invalid ip %q: %w", ip, err)
	} else if !isPublicAddr(addr) {
		return nil, fmt.Errorf("non-public ip: %s", ip)
	}

	// Cache hit
	if v, ok := r.ipCache.Load(ip); ok {
		if entry, ok := v.(cachedIP); ok && entry.info != nil && time.Now().Before(entry.expires) {
			return entry.info, nil
		}
	}

	ctx, cancel := withBoundedTimeout(ctx, r.timeout)
	defer cancel()
	v, err, _ := r.ipGroup.Do(ip, func() (any, error) {
		info, err := r.lookupIPWhoIs(ctx, ip)
		if err != nil {
			return nil, err
		}
		r.ipCache.Store(ip, cachedIP{info: info, expires: time.Now().Add(r.cacheTTL)})
		return info, nil
	})
	if err != nil {
		return nil, err
	}
	info, _ := v.(*Info)
	if info == nil {
		return nil, fmt.Errorf("geo lookup returned empty for %s", ip)
	}
	return info, nil
}

func withBoundedTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if d <= 0 {
		return ctx, func() {}
	}
	if deadline, ok := ctx.Deadline(); ok {
		// If the caller's deadline is already sooner than d, don't wrap.
		if time.Until(deadline) <= d {
			return ctx, func() {}
		}
	}
	return context.WithTimeout(ctx, d)
}

func isPublicAddr(addr netip.Addr) bool {
	// Private, loopback, multicast, link-local, unspecified, etc are not useful for GeoIP.
	if !addr.IsValid() {
		return false
	}
	if addr.IsPrivate() || addr.IsLoopback() || addr.IsMulticast() || addr.IsUnspecified() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return false
	}
	return true
}

func resolveHostToPublicIP(ctx context.Context, host string) (string, error) {
	// Prefer IPv4 for more stable / widely-supported GeoIP responses.
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("resolve %q: no ip addresses", host)
	}

	for _, a := range addrs {
		if a.IP == nil {
			continue
		}
		if v4 := a.IP.To4(); v4 != nil {
			if addr, err := netip.ParseAddr(v4.String()); err == nil && isPublicAddr(addr) {
				return addr.String(), nil
			}
		}
	}

	for _, a := range addrs {
		if a.IP == nil {
			continue
		}
		if addr, err := netip.ParseAddr(a.IP.String()); err == nil && isPublicAddr(addr) {
			return addr.String(), nil
		}
	}

	return "", fmt.Errorf("resolve %q: no public ip found", host)
}

type ipWhoIsResponse struct {
	Success     bool   `json:"success"`
	Message     string `json:"message"`
	IP          string `json:"ip"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	Region      string `json:"region"`
	City        string `json:"city"`
	Connection  struct {
		ASN int    `json:"asn"`
		ISP string `json:"isp"`
	} `json:"connection"`
}

func (r *Resolver) lookupIPWhoIs(ctx context.Context, ip string) (*Info, error) {
	// Minimize payload to speed up and reduce data transfer.
	endpoint := r.baseURL + url.PathEscape(ip)
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse geo endpoint: %w", err)
	}
	q := u.Query()
	q.Set("fields", "success,message,ip,country,country_code,region,city,connection")
	if strings.TrimSpace(r.language) != "" {
		q.Set("lang", r.language)
	}
	u.RawQuery = q.Encode()

	// Try direct first to avoid proxy loops; fall back to env proxy if needed.
	body, err := r.doGETJSON(ctx, u.String())
	if err != nil {
		return nil, err
	}

	var resp ipWhoIsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode geo response: %w", err)
	}
	if !resp.Success {
		msg := strings.TrimSpace(resp.Message)
		if msg == "" {
			msg = "geo lookup failed"
		}
		return nil, errors.New(msg)
	}

	info := &Info{
		IP:          strings.TrimSpace(resp.IP),
		Country:     strings.TrimSpace(resp.Country),
		CountryCode: strings.TrimSpace(resp.CountryCode),
		Region:      strings.TrimSpace(resp.Region),
		City:        strings.TrimSpace(resp.City),
		ISP:         strings.TrimSpace(resp.Connection.ISP),
	}
	if resp.Connection.ASN > 0 {
		info.ASN = fmt.Sprintf("AS%d", resp.Connection.ASN)
	}
	if info.IP == "" {
		info.IP = ip
	}
	return info, nil
}

func (r *Resolver) doGETJSON(ctx context.Context, urlStr string) ([]byte, error) {
	if r == nil {
		return nil, errors.New("geo resolver is nil")
	}

	try := func(client *http.Client) ([]byte, int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, resp.StatusCode, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, resp.StatusCode, fmt.Errorf("status %d", resp.StatusCode)
		}
		return b, resp.StatusCode, nil
	}

	if b, _, err := try(r.httpDirect); err == nil {
		return b, nil
	} else {
		// Keep the direct error only for debugging if env also fails.
		directErr := err
		if b, _, envErr := try(r.httpEnv); envErr == nil {
			return b, nil
		} else {
			return nil, fmt.Errorf("geo http failed (direct: %v, env: %v)", directErr, envErr)
		}
	}
}
