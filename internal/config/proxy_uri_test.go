package config

import "testing"

func TestIsProxyURI_HTTPProxy(t *testing.T) {
	if !isProxyURI("http://127.0.0.1:8080") {
		t.Fatalf("expected http proxy uri to be detected")
	}
	if !isProxyURI("https://127.0.0.1:8443?sni=proxy.example.com") {
		t.Fatalf("expected https proxy uri with port to be detected")
	}
	if isProxyURI("https://www.google.com/") {
		t.Fatalf("expected normal https url without port to be rejected")
	}
	if isProxyURI("http://example.com:8080/path") {
		t.Fatalf("expected http url with non-root path to be rejected")
	}
}

func TestIsProxyURI_SOCKSProxy(t *testing.T) {
	if !isProxyURI("socks5://127.0.0.1:1080") {
		t.Fatalf("expected socks5 proxy uri to be detected")
	}
	if !isProxyURI("socks4a://127.0.0.1:1080") {
		t.Fatalf("expected socks4a proxy uri to be detected")
	}
}
