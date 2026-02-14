package builder

import (
	"errors"
	"testing"
	"time"

	"easy_proxies/internal/config"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
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

func TestBuildNodeOutbound_HTTPProxy(t *testing.T) {
	out, err := buildNodeOutbound("t", "http://user:pass@127.0.0.1:8080", false, 5*time.Second)
	if err != nil {
		t.Fatalf("buildNodeOutbound error: %v", err)
	}
	if out.Type != C.TypeHTTP {
		t.Fatalf("expected type %q, got %q", C.TypeHTTP, out.Type)
	}
	opts, ok := out.Options.(*option.HTTPOutboundOptions)
	if !ok {
		t.Fatalf("expected *option.HTTPOutboundOptions, got %T", out.Options)
	}
	if opts.Server != "127.0.0.1" || opts.ServerPort != 8080 {
		t.Fatalf("unexpected server: %s:%d", opts.Server, opts.ServerPort)
	}
	if opts.Username != "user" || opts.Password != "pass" {
		t.Fatalf("unexpected auth: %q/%q", opts.Username, opts.Password)
	}
	if opts.ConnectTimeout.Build() != 5*time.Second {
		t.Fatalf("unexpected connect timeout: %v", opts.ConnectTimeout.Build())
	}
	if opts.TLS != nil {
		t.Fatalf("expected no TLS for http proxy, got %+v", opts.TLS)
	}
}

func TestBuildNodeOutbound_HTTPSProxy_WithSNI(t *testing.T) {
	out, err := buildNodeOutbound("t", "https://127.0.0.1:8443?sni=proxy.example.com", true, 0)
	if err != nil {
		t.Fatalf("buildNodeOutbound error: %v", err)
	}
	if out.Type != C.TypeHTTP {
		t.Fatalf("expected type %q, got %q", C.TypeHTTP, out.Type)
	}
	opts, ok := out.Options.(*option.HTTPOutboundOptions)
	if !ok {
		t.Fatalf("expected *option.HTTPOutboundOptions, got %T", out.Options)
	}
	if opts.TLS == nil || !opts.TLS.Enabled {
		t.Fatalf("expected TLS enabled for https proxy, got %+v", opts.TLS)
	}
	if opts.TLS.ServerName != "proxy.example.com" {
		t.Fatalf("unexpected TLS server name: %q", opts.TLS.ServerName)
	}
	if !opts.TLS.Insecure {
		t.Fatalf("expected TLS insecure=true when skipCertVerify=true")
	}
}

func TestBuildNodeOutbound_SOCKS5Proxy(t *testing.T) {
	out, err := buildNodeOutbound("t", "socks5://user:pass@127.0.0.1:1080", false, 3*time.Second)
	if err != nil {
		t.Fatalf("buildNodeOutbound error: %v", err)
	}
	if out.Type != C.TypeSOCKS {
		t.Fatalf("expected type %q, got %q", C.TypeSOCKS, out.Type)
	}
	opts, ok := out.Options.(*option.SOCKSOutboundOptions)
	if !ok {
		t.Fatalf("expected *option.SOCKSOutboundOptions, got %T", out.Options)
	}
	if opts.Server != "127.0.0.1" || opts.ServerPort != 1080 {
		t.Fatalf("unexpected server: %s:%d", opts.Server, opts.ServerPort)
	}
	if opts.Username != "user" || opts.Password != "pass" {
		t.Fatalf("unexpected auth: %q/%q", opts.Username, opts.Password)
	}
	if opts.Version != "5" {
		t.Fatalf("expected version 5, got %q", opts.Version)
	}
	if opts.ConnectTimeout.Build() != 3*time.Second {
		t.Fatalf("unexpected connect timeout: %v", opts.ConnectTimeout.Build())
	}
}
