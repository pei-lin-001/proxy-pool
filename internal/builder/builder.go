package builder

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"easy_proxies/internal/config"
	poolout "easy_proxies/internal/outbound/pool"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/auth"
	"github.com/sagernet/sing/common/json/badoption"
)

var ErrNoValidNodes = errors.New("no valid nodes available")

// Build converts high level config into sing-box Options tree.
func Build(cfg *config.Config) (option.Options, error) {
	if cfg == nil {
		return option.Options{}, errors.New("config is nil")
	}
	if len(cfg.Nodes) == 0 {
		return option.Options{}, fmt.Errorf("%w: no nodes configured", ErrNoValidNodes)
	}
	selectedNodes := cfg.FilterNodes()
	if len(selectedNodes) == 0 {
		return option.Options{}, fmt.Errorf("%w: no nodes matched node_filter", ErrNoValidNodes)
	}
	if filtered := len(cfg.Nodes) - len(selectedNodes); filtered > 0 {
		log.Printf("🔎 Node filter applied: %d/%d nodes enabled", len(selectedNodes), len(cfg.Nodes))
	}

	baseOutbounds := make([]option.Outbound, 0, len(selectedNodes))
	memberTags := make([]string, 0, len(selectedNodes))
	metadata := make(map[string]poolout.MemberMeta)
	var failedNodes []string
	usedTags := make(map[string]int) // Track tag usage for uniqueness

	for _, node := range selectedNodes {
		baseTag := sanitizeTag(node.Name)
		if baseTag == "" {
			baseTag = fmt.Sprintf("node-%d", len(memberTags)+1)
		}

		// Ensure tag uniqueness by appending a counter if needed
		tag := baseTag
		if count, exists := usedTags[baseTag]; exists {
			usedTags[baseTag] = count + 1
			tag = fmt.Sprintf("%s-%d", baseTag, count+1)
		} else {
			usedTags[baseTag] = 1
		}

		outbound, err := buildNodeOutbound(tag, node.URI, cfg.SkipCertVerify, cfg.ConnectTimeout)
		if err != nil {
			log.Printf("❌ Failed to build node '%s': %v (skipping)", node.Name, err)
			failedNodes = append(failedNodes, node.Name)
			continue
		}
		memberTags = append(memberTags, tag)
		baseOutbounds = append(baseOutbounds, outbound)
		meta := poolout.MemberMeta{
			Name: node.Name,
			URI:  node.URI,
			Mode: cfg.Mode,
			Geo:  node.Geo,
		}
		// For multi-port and hybrid modes, use per-node port
		if cfg.Mode == "multi-port" || cfg.Mode == "hybrid" {
			meta.ListenAddress = cfg.MultiPort.Address
			meta.Port = node.Port
		} else {
			meta.ListenAddress = cfg.Listener.Address
			meta.Port = cfg.Listener.Port
		}
		metadata[tag] = meta
	}

	// Check if we have at least one valid node
	if len(baseOutbounds) == 0 {
		return option.Options{}, fmt.Errorf("%w: all %d enabled nodes failed to build", ErrNoValidNodes, len(selectedNodes))
	}

	// Log summary
	if len(failedNodes) > 0 {
		log.Printf("⚠️  %d/%d nodes failed and were skipped: %v", len(failedNodes), len(selectedNodes), failedNodes)
	}
	log.Printf("✅ Successfully built %d/%d nodes", len(baseOutbounds), len(selectedNodes))

	// Print proxy links for each node
	printProxyLinks(cfg, metadata)

	var (
		inbounds  []option.Inbound
		outbounds = make([]option.Outbound, len(baseOutbounds))
		route     option.RouteOptions
	)
	copy(outbounds, baseOutbounds)

	// Determine which components to enable based on mode
	enablePoolInbound := cfg.Mode == "pool" || cfg.Mode == "hybrid"
	enableMultiPort := cfg.Mode == "multi-port" || cfg.Mode == "hybrid"

	if !enablePoolInbound && !enableMultiPort {
		return option.Options{}, fmt.Errorf("unsupported mode %s", cfg.Mode)
	}

	// Build pool inbound (single entry point for all nodes)
	if enablePoolInbound {
		inbound, err := buildPoolInbound(cfg)
		if err != nil {
			return option.Options{}, err
		}
		inbounds = append(inbounds, inbound)
		poolOptions := poolout.Options{
			Mode:              cfg.Pool.Mode,
			Members:           memberTags,
			FailureThreshold:  cfg.Pool.FailureThreshold,
			BlacklistDuration: cfg.Pool.BlacklistDuration,
			Metadata:          metadata,
		}
		outbounds = append(outbounds, option.Outbound{
			Type:    poolout.Type,
			Tag:     poolout.Tag,
			Options: &poolOptions,
		})
		route.Final = poolout.Tag
	}

	// Build multi-port inbounds (one port per node)
	if enableMultiPort {
		addr, err := parseAddr(cfg.MultiPort.Address)
		if err != nil {
			return option.Options{}, fmt.Errorf("parse multi-port address: %w", err)
		}
		for _, tag := range memberTags {
			meta := metadata[tag]
			perMeta := map[string]poolout.MemberMeta{tag: meta}
			poolTag := fmt.Sprintf("%s-%s", poolout.Tag, tag)
			perOptions := poolout.Options{
				Mode:              "sequential",
				Members:           []string{tag},
				FailureThreshold:  cfg.Pool.FailureThreshold,
				BlacklistDuration: cfg.Pool.BlacklistDuration,
				Metadata:          perMeta,
			}
			perPool := option.Outbound{
				Type:    poolout.Type,
				Tag:     poolTag,
				Options: &perOptions,
			}
			outbounds = append(outbounds, perPool)
			inboundOptions := &option.HTTPMixedInboundOptions{
				ListenOptions: option.ListenOptions{
					Listen:     addr,
					ListenPort: meta.Port,
				},
			}
			username := cfg.MultiPort.Username
			password := cfg.MultiPort.Password
			if username != "" {
				inboundOptions.Users = []auth.User{{Username: username, Password: password}}
			}
			inboundTag := fmt.Sprintf("in-%s", tag)
			inbounds = append(inbounds, option.Inbound{
				Type:    C.TypeHTTP,
				Tag:     inboundTag,
				Options: inboundOptions,
			})
			route.Rules = append(route.Rules, option.Rule{
				Type: C.RuleTypeDefault,
				DefaultOptions: option.DefaultRule{
					RawDefaultRule: option.RawDefaultRule{
						Inbound: badoption.Listable[string]{inboundTag},
					},
					RuleAction: option.RuleAction{
						Action: C.RuleActionTypeRoute,
						RouteOptions: option.RouteActionOptions{
							Outbound: poolTag,
						},
					},
				},
			})
		}
	}

	opts := option.Options{
		Log:       &option.LogOptions{Level: strings.ToLower(cfg.LogLevel)},
		Inbounds:  inbounds,
		Outbounds: outbounds,
		Route:     &route,
	}
	return opts, nil
}

func buildPoolInbound(cfg *config.Config) (option.Inbound, error) {
	listenAddr, err := parseAddr(cfg.Listener.Address)
	if err != nil {
		return option.Inbound{}, fmt.Errorf("parse listener address: %w", err)
	}
	inboundOptions := &option.HTTPMixedInboundOptions{
		ListenOptions: option.ListenOptions{
			Listen:     listenAddr,
			ListenPort: cfg.Listener.Port,
		},
	}
	if cfg.Listener.Username != "" {
		inboundOptions.Users = []auth.User{{
			Username: cfg.Listener.Username,
			Password: cfg.Listener.Password,
		}}
	}
	inbound := option.Inbound{
		Type:    C.TypeHTTP,
		Tag:     "http-in",
		Options: inboundOptions,
	}
	return inbound, nil
}

func buildNodeOutbound(tag, rawURI string, skipCertVerify bool, connectTimeout time.Duration) (option.Outbound, error) {
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return option.Outbound{}, fmt.Errorf("parse uri: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "vless":
		opts, err := buildVLESSOptions(parsed, skipCertVerify)
		if err != nil {
			return option.Outbound{}, err
		}
		if connectTimeout > 0 && opts.ConnectTimeout == 0 {
			opts.ConnectTimeout = badoption.Duration(connectTimeout)
		}
		return option.Outbound{Type: C.TypeVLESS, Tag: tag, Options: &opts}, nil
	case "hysteria2":
		opts, err := buildHysteria2Options(parsed, skipCertVerify)
		if err != nil {
			return option.Outbound{}, err
		}
		if connectTimeout > 0 && opts.ConnectTimeout == 0 {
			opts.ConnectTimeout = badoption.Duration(connectTimeout)
		}
		return option.Outbound{Type: C.TypeHysteria2, Tag: tag, Options: &opts}, nil
	case "ss", "shadowsocks":
		opts, err := buildShadowsocksOptions(parsed)
		if err != nil {
			return option.Outbound{}, err
		}
		if connectTimeout > 0 && opts.ConnectTimeout == 0 {
			opts.ConnectTimeout = badoption.Duration(connectTimeout)
		}
		return option.Outbound{Type: C.TypeShadowsocks, Tag: tag, Options: &opts}, nil
	case "trojan":
		opts, err := buildTrojanOptions(parsed, skipCertVerify)
		if err != nil {
			return option.Outbound{}, err
		}
		if connectTimeout > 0 && opts.ConnectTimeout == 0 {
			opts.ConnectTimeout = badoption.Duration(connectTimeout)
		}
		return option.Outbound{Type: C.TypeTrojan, Tag: tag, Options: &opts}, nil
	case "vmess":
		opts, err := buildVMessOptions(rawURI, skipCertVerify)
		if err != nil {
			return option.Outbound{}, err
		}
		if connectTimeout > 0 && opts.ConnectTimeout == 0 {
			opts.ConnectTimeout = badoption.Duration(connectTimeout)
		}
		return option.Outbound{Type: C.TypeVMess, Tag: tag, Options: &opts}, nil
	default:
		return option.Outbound{}, fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}
}

func buildVLESSOptions(u *url.URL, skipCertVerify bool) (option.VLESSOutboundOptions, error) {
	if u.User == nil {
		return option.VLESSOutboundOptions{}, errors.New("vless uri missing userinfo")
	}
	server, port, err := hostPort(u, 443)
	if err != nil {
		return option.VLESSOutboundOptions{}, err
	}
	query := u.Query()
	uuid, err := extractVLESSUUID(u)
	if err != nil {
		return option.VLESSOutboundOptions{}, err
	}
	opts := option.VLESSOutboundOptions{
		UUID:          uuid,
		ServerOptions: option.ServerOptions{Server: server, ServerPort: uint16(port)},
		Network:       option.NetworkList(""),
	}
	if flow := query.Get("flow"); flow != "" {
		opts.Flow = flow
	} else if xtls := strings.TrimSpace(query.Get("xtls")); xtls != "" {
		// Shadowrocket style: xtls=2 means vision.
		if xtls == "2" {
			opts.Flow = "xtls-rprx-vision"
		} else if xtls == "1" {
			opts.Flow = "xtls-rprx-origin"
		}
	}
	if packetEncoding := query.Get("packetEncoding"); packetEncoding != "" {
		opts.PacketEncoding = &packetEncoding
	}
	if tfo := strings.TrimSpace(query.Get("tfo")); tfo != "" {
		opts.TCPFastOpen = parseBool01(tfo)
	}
	if transport, err := buildV2RayTransport(query); err != nil {
		return option.VLESSOutboundOptions{}, err
	} else if transport != nil {
		opts.Transport = transport
	}
	if tlsOptions, err := buildTLSOptions(query, skipCertVerify); err != nil {
		return option.VLESSOutboundOptions{}, err
	} else if tlsOptions != nil {
		opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tlsOptions}
	}
	return opts, nil
}

func buildHysteria2Options(u *url.URL, skipCertVerify bool) (option.Hysteria2OutboundOptions, error) {
	if u.User == nil {
		return option.Hysteria2OutboundOptions{}, errors.New("hysteria2 uri missing userinfo")
	}
	password := u.User.String()
	if password == "" {
		return option.Hysteria2OutboundOptions{}, errors.New("hysteria2 uri missing password in userinfo")
	}
	server, port, err := hostPort(u, 443)
	if err != nil {
		return option.Hysteria2OutboundOptions{}, err
	}
	query := u.Query()
	opts := option.Hysteria2OutboundOptions{
		ServerOptions: option.ServerOptions{Server: server, ServerPort: uint16(port)},
		Password:      password,
	}
	if up := query.Get("upMbps"); up != "" {
		opts.UpMbps = atoiDefault(up)
	}
	if down := query.Get("downMbps"); down != "" {
		opts.DownMbps = atoiDefault(down)
	}
	if obfs := query.Get("obfs"); obfs != "" {
		opts.Obfs = &option.Hysteria2Obfs{Type: obfs, Password: query.Get("obfs-password")}
	}
	opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: hysteriaTLSOptions(server, query, skipCertVerify)}
	return opts, nil
}

func hysteriaTLSOptions(host string, query url.Values, skipCertVerify bool) *option.OutboundTLSOptions {
	tlsOptions := &option.OutboundTLSOptions{
		Enabled:    true,
		ServerName: host,
		Insecure:   skipCertVerify,
	}
	if sni := query.Get("sni"); sni != "" {
		tlsOptions.ServerName = sni
	}
	insecure := query.Get("insecure")
	if insecure == "" {
		insecure = query.Get("allowInsecure")
	}
	if insecure != "" {
		tlsOptions.Insecure = insecure == "1" || strings.EqualFold(insecure, "true")
	}
	if alpn := query.Get("alpn"); alpn != "" {
		tlsOptions.ALPN = badoption.Listable[string](strings.Split(alpn, ","))
	}
	return tlsOptions
}

func buildTLSOptions(query url.Values, skipCertVerify bool) (*option.OutboundTLSOptions, error) {
	security := strings.ToLower(strings.TrimSpace(query.Get("security")))
	if security == "" {
		// Shadowrocket style: tls=1, peer=..., pbk/sid for reality.
		if parseBool01(query.Get("tls")) {
			if query.Get("pbk") != "" || query.Get("sid") != "" {
				security = "reality"
			} else {
				security = "tls"
			}
		}
	}
	if security == "" || security == "none" {
		return nil, nil
	}
	tlsOptions := &option.OutboundTLSOptions{Enabled: true, Insecure: skipCertVerify}
	if sni := query.Get("sni"); sni != "" {
		tlsOptions.ServerName = sni
	}
	if peer := query.Get("peer"); peer != "" && tlsOptions.ServerName == "" {
		tlsOptions.ServerName = peer
	}
	if serverName := query.Get("servername"); serverName != "" && tlsOptions.ServerName == "" {
		tlsOptions.ServerName = serverName
	}
	insecure := query.Get("allowInsecure")
	if insecure == "" {
		insecure = query.Get("insecure")
	}
	if insecure != "" {
		tlsOptions.Insecure = insecure == "1" || strings.EqualFold(insecure, "true")
	}
	if alpn := query.Get("alpn"); alpn != "" {
		tlsOptions.ALPN = badoption.Listable[string](strings.Split(alpn, ","))
	}
	fp := query.Get("fp")
	if fp != "" {
		tlsOptions.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: fp}
	}
	if security == "reality" {
		tlsOptions.Reality = &option.OutboundRealityOptions{Enabled: true, PublicKey: query.Get("pbk"), ShortID: query.Get("sid")}
		// Reality requires uTLS; use default fingerprint if not specified
		if tlsOptions.UTLS == nil {
			if fp == "" {
				fp = "chrome"
			}
			tlsOptions.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: fp}
		}
	}
	return tlsOptions, nil
}

func extractVLESSUUID(u *url.URL) (string, error) {
	if u == nil || u.User == nil {
		return "", errors.New("vless uri missing userinfo")
	}
	raw := strings.TrimSpace(u.User.Username())
	if raw == "" {
		return "", errors.New("vless uri missing uuid in userinfo")
	}
	if normalized := normalizeUUID(raw); normalized != "" {
		return normalized, nil
	}

	// Shadowrocket style: userinfo is base64 ("none:uuid@host:port").
	decoded, err := decodeBase64Any(raw)
	if err != nil {
		return "", fmt.Errorf("vless invalid uuid %q", raw)
	}
	if extracted := extractUUIDFromText(string(decoded)); extracted != "" {
		return extracted, nil
	}
	return "", fmt.Errorf("vless invalid uuid %q", raw)
}

func parseBool01(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if value == "1" {
		return true
	}
	if value == "0" {
		return false
	}
	return strings.EqualFold(value, "true") || strings.EqualFold(value, "yes") || strings.EqualFold(value, "on")
}

func normalizeUUID(value string) string {
	value = strings.TrimSpace(value)
	if isUUID36(value) {
		return strings.ToLower(value)
	}
	if isUUID32(value) {
		value = strings.ToLower(value)
		return value[0:8] + "-" + value[8:12] + "-" + value[12:16] + "-" + value[16:20] + "-" + value[20:32]
	}
	return ""
}

func isUUID36(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHexByte(c) {
				return false
			}
		}
	}
	return true
}

func isUUID32(value string) bool {
	if len(value) != 32 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if !isHexByte(value[i]) {
			return false
		}
	}
	return true
}

func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func decodeBase64Any(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "\n", "")
	value = strings.ReplaceAll(value, "\r", "")
	if value == "" {
		return nil, errors.New("empty base64")
	}
	encodings := []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.StdEncoding,
	}
	var lastErr error
	for _, enc := range encodings {
		decoded, err := enc.DecodeString(value)
		if err == nil {
			return decoded, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func extractUUIDFromText(text string) string {
	// Prefer canonical 36-char UUID with dashes.
	for i := 0; i+36 <= len(text); i++ {
		cand := text[i : i+36]
		if normalized := normalizeUUID(cand); normalized != "" {
			return normalized
		}
	}
	// Also accept 32-char hex UUID (no dashes).
	for i := 0; i+32 <= len(text); i++ {
		cand := text[i : i+32]
		if normalized := normalizeUUID(cand); normalized != "" {
			return normalized
		}
	}
	return ""
}

func buildV2RayTransport(query url.Values) (*option.V2RayTransportOptions, error) {
	transportType := strings.ToLower(query.Get("type"))
	if transportType == "" || transportType == "tcp" {
		return nil, nil
	}
	options := &option.V2RayTransportOptions{Type: transportType}
	switch transportType {
	case C.V2RayTransportTypeWebsocket:
		wsPath := query.Get("path")
		// 解析 path 中的 early data 参数，如 /path?ed=2048
		if idx := strings.Index(wsPath, "?ed="); idx != -1 {
			edPart := wsPath[idx+4:]
			wsPath = wsPath[:idx]
			// 解析 ed 值
			edValue := edPart
			if ampIdx := strings.Index(edPart, "&"); ampIdx != -1 {
				edValue = edPart[:ampIdx]
			}
			if ed, err := strconv.Atoi(edValue); err == nil && ed > 0 {
				options.WebsocketOptions.MaxEarlyData = uint32(ed)
				options.WebsocketOptions.EarlyDataHeaderName = "Sec-WebSocket-Protocol"
			}
		}
		options.WebsocketOptions.Path = wsPath
		if host := query.Get("host"); host != "" {
			options.WebsocketOptions.Headers = badoption.HTTPHeader{"Host": {host}}
		}
	case C.V2RayTransportTypeHTTP:
		options.HTTPOptions.Path = query.Get("path")
		if host := query.Get("host"); host != "" {
			options.HTTPOptions.Host = badoption.Listable[string]{host}
		}
	case C.V2RayTransportTypeGRPC:
		options.GRPCOptions.ServiceName = query.Get("serviceName")
	case C.V2RayTransportTypeHTTPUpgrade:
		options.HTTPUpgradeOptions.Path = query.Get("path")
	case "xhttp":
		// XHTTP is not supported by sing-box, fallback to HTTPUpgrade
		log.Printf("⚠️  XHTTP transport not supported by sing-box, falling back to HTTPUpgrade")
		options.Type = C.V2RayTransportTypeHTTPUpgrade
		options.HTTPUpgradeOptions.Path = query.Get("path")
		if host := query.Get("host"); host != "" {
			options.HTTPUpgradeOptions.Headers = badoption.HTTPHeader{"Host": {host}}
		}
	default:
		return nil, fmt.Errorf("unsupported transport type %q", transportType)
	}
	return options, nil
}

func buildShadowsocksOptions(u *url.URL) (option.ShadowsocksOutboundOptions, error) {
	server, port, err := hostPort(u, 8388)
	if err != nil {
		return option.ShadowsocksOutboundOptions{}, err
	}

	// Decode userinfo (base64 encoded method:password)
	if u.User == nil {
		return option.ShadowsocksOutboundOptions{}, errors.New("shadowsocks uri missing userinfo")
	}
	userInfo := u.User.String()
	if userInfo == "" {
		return option.ShadowsocksOutboundOptions{}, errors.New("shadowsocks uri missing userinfo")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(userInfo)
	if err != nil {
		// Try standard base64
		decoded, err = base64.StdEncoding.DecodeString(userInfo)
		if err != nil {
			return option.ShadowsocksOutboundOptions{}, fmt.Errorf("decode shadowsocks userinfo: %w", err)
		}
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return option.ShadowsocksOutboundOptions{}, errors.New("shadowsocks userinfo format must be method:password")
	}

	method := parts[0]
	password := parts[1]

	opts := option.ShadowsocksOutboundOptions{
		ServerOptions: option.ServerOptions{Server: server, ServerPort: uint16(port)},
		Method:        method,
		Password:      password,
	}

	query := u.Query()
	if plugin := query.Get("plugin"); plugin != "" {
		opts.Plugin = plugin
		opts.PluginOptions = query.Get("plugin-opts")
	}

	return opts, nil
}

func buildTrojanOptions(u *url.URL, skipCertVerify bool) (option.TrojanOutboundOptions, error) {
	if u.User == nil {
		return option.TrojanOutboundOptions{}, errors.New("trojan uri missing userinfo")
	}
	password := u.User.Username()
	if password == "" {
		return option.TrojanOutboundOptions{}, errors.New("trojan uri missing password in userinfo")
	}

	server, port, err := hostPort(u, 443)
	if err != nil {
		return option.TrojanOutboundOptions{}, err
	}

	query := u.Query()
	opts := option.TrojanOutboundOptions{
		ServerOptions: option.ServerOptions{Server: server, ServerPort: uint16(port)},
		Password:      password,
		Network:       option.NetworkList(""),
	}

	// Parse TLS options
	if tlsOptions, err := buildTrojanTLSOptions(query, skipCertVerify); err != nil {
		return option.TrojanOutboundOptions{}, err
	} else if tlsOptions != nil {
		opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tlsOptions}
	}

	// Parse transport options
	if transport, err := buildV2RayTransport(query); err != nil {
		return option.TrojanOutboundOptions{}, err
	} else if transport != nil {
		opts.Transport = transport
	}

	return opts, nil
}

// vmessJSON represents the JSON structure of a VMess URI
type vmessJSON struct {
	V    interface{} `json:"v"`    // Version, can be string or int
	PS   string      `json:"ps"`   // Remarks/name
	Add  string      `json:"add"`  // Server address
	Port interface{} `json:"port"` // Server port, can be string or int
	ID   string      `json:"id"`   // UUID
	Aid  interface{} `json:"aid"`  // Alter ID, can be string or int
	Scy  string      `json:"scy"`  // Security/cipher
	Net  string      `json:"net"`  // Network type (tcp, ws, etc.)
	Type string      `json:"type"` // Header type
	Host string      `json:"host"` // Host header
	Path string      `json:"path"` // Path
	TLS  string      `json:"tls"`  // TLS (tls or empty)
	SNI  string      `json:"sni"`  // SNI
	ALPN string      `json:"alpn"` // ALPN
	FP   string      `json:"fp"`   // Fingerprint
}

func (v *vmessJSON) GetPort() int {
	switch p := v.Port.(type) {
	case float64:
		return int(p)
	case int:
		return p
	case string:
		port, _ := strconv.Atoi(p)
		return port
	}
	return 443
}

func (v *vmessJSON) GetAlterId() int {
	switch a := v.Aid.(type) {
	case float64:
		return int(a)
	case int:
		return a
	case string:
		aid, _ := strconv.Atoi(a)
		return aid
	}
	return 0
}

func buildVMessOptions(rawURI string, skipCertVerify bool) (option.VMessOutboundOptions, error) {
	// Remove vmess:// prefix
	encoded := strings.TrimPrefix(rawURI, "vmess://")

	// Try to decode as base64 JSON (standard format)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// Try URL-safe base64
		decoded, err = base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			// Try as URL format: vmess://uuid@server:port?...
			return buildVMessOptionsFromURL(rawURI, skipCertVerify)
		}
	}

	var vmess vmessJSON
	if err := json.Unmarshal(decoded, &vmess); err != nil {
		return option.VMessOutboundOptions{}, fmt.Errorf("parse vmess json: %w", err)
	}

	if vmess.Add == "" {
		return option.VMessOutboundOptions{}, errors.New("vmess missing server address")
	}
	if vmess.ID == "" {
		return option.VMessOutboundOptions{}, errors.New("vmess missing uuid")
	}

	port := vmess.GetPort()
	if port == 0 {
		port = 443
	}

	opts := option.VMessOutboundOptions{
		ServerOptions: option.ServerOptions{
			Server:     vmess.Add,
			ServerPort: uint16(port),
		},
		UUID:     vmess.ID,
		AlterId:  vmess.GetAlterId(),
		Security: vmess.Scy,
	}

	// Default security
	if opts.Security == "" {
		opts.Security = "auto"
	}

	// Build transport options
	if vmess.Net != "" && vmess.Net != "tcp" {
		transport := &option.V2RayTransportOptions{}
		switch vmess.Net {
		case "ws":
			transport.Type = C.V2RayTransportTypeWebsocket
			wsPath := vmess.Path
			// Handle early data in path
			if idx := strings.Index(wsPath, "?ed="); idx != -1 {
				edPart := wsPath[idx+4:]
				wsPath = wsPath[:idx]
				edValue := edPart
				if ampIdx := strings.Index(edPart, "&"); ampIdx != -1 {
					edValue = edPart[:ampIdx]
				}
				if ed, err := strconv.Atoi(edValue); err == nil && ed > 0 {
					transport.WebsocketOptions.MaxEarlyData = uint32(ed)
					transport.WebsocketOptions.EarlyDataHeaderName = "Sec-WebSocket-Protocol"
				}
			}
			transport.WebsocketOptions.Path = wsPath
			if vmess.Host != "" {
				transport.WebsocketOptions.Headers = badoption.HTTPHeader{"Host": {vmess.Host}}
			}
		case "h2":
			transport.Type = C.V2RayTransportTypeHTTP
			transport.HTTPOptions.Path = vmess.Path
			if vmess.Host != "" {
				transport.HTTPOptions.Host = badoption.Listable[string]{vmess.Host}
			}
		case "grpc":
			transport.Type = C.V2RayTransportTypeGRPC
			transport.GRPCOptions.ServiceName = vmess.Path
		default:
			transport.Type = vmess.Net
		}
		opts.Transport = transport
	}

	// Build TLS options
	if vmess.TLS == "tls" {
		tlsOptions := &option.OutboundTLSOptions{Enabled: true, Insecure: skipCertVerify}
		if vmess.SNI != "" {
			tlsOptions.ServerName = vmess.SNI
		} else if vmess.Host != "" {
			tlsOptions.ServerName = vmess.Host
		}
		if vmess.ALPN != "" {
			tlsOptions.ALPN = badoption.Listable[string](strings.Split(vmess.ALPN, ","))
		}
		if vmess.FP != "" {
			tlsOptions.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: vmess.FP}
		}
		opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tlsOptions}
	}

	return opts, nil
}

func buildVMessOptionsFromURL(rawURI string, skipCertVerify bool) (option.VMessOutboundOptions, error) {
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return option.VMessOutboundOptions{}, fmt.Errorf("parse vmess url: %w", err)
	}

	uuid := parsed.User.Username()
	if uuid == "" {
		return option.VMessOutboundOptions{}, errors.New("vmess uri missing uuid")
	}

	server, port, err := hostPort(parsed, 443)
	if err != nil {
		return option.VMessOutboundOptions{}, err
	}

	query := parsed.Query()
	opts := option.VMessOutboundOptions{
		ServerOptions: option.ServerOptions{
			Server:     server,
			ServerPort: uint16(port),
		},
		UUID:     uuid,
		Security: query.Get("encryption"),
	}

	if opts.Security == "" {
		opts.Security = "auto"
	}

	if aid := query.Get("alterId"); aid != "" {
		opts.AlterId, _ = strconv.Atoi(aid)
	}

	// Build transport
	if transport, err := buildV2RayTransport(query); err != nil {
		return option.VMessOutboundOptions{}, err
	} else if transport != nil {
		opts.Transport = transport
	}

	// Build TLS
	if tlsOptions, err := buildTLSOptions(query, skipCertVerify); err != nil {
		return option.VMessOutboundOptions{}, err
	} else if tlsOptions != nil {
		opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tlsOptions}
	}

	return opts, nil
}

func buildTrojanTLSOptions(query url.Values, skipCertVerify bool) (*option.OutboundTLSOptions, error) {
	// Trojan always uses TLS by default
	tlsOptions := &option.OutboundTLSOptions{Enabled: true, Insecure: skipCertVerify}

	if sni := query.Get("sni"); sni != "" {
		tlsOptions.ServerName = sni
	}
	if peer := query.Get("peer"); peer != "" && tlsOptions.ServerName == "" {
		tlsOptions.ServerName = peer
	}

	insecure := query.Get("allowInsecure")
	if insecure == "" {
		insecure = query.Get("insecure")
	}
	if insecure != "" {
		tlsOptions.Insecure = insecure == "1" || strings.EqualFold(insecure, "true")
	}

	if alpn := query.Get("alpn"); alpn != "" {
		tlsOptions.ALPN = badoption.Listable[string](strings.Split(alpn, ","))
	}

	if fp := query.Get("fp"); fp != "" {
		tlsOptions.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: fp}
	}

	return tlsOptions, nil
}

func hostPort(u *url.URL, defaultPort int) (string, int, error) {
	host := u.Hostname()
	if host == "" {
		return "", 0, errors.New("missing host")
	}
	portStr := u.Port()
	if portStr == "" {
		portStr = strconv.Itoa(defaultPort)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid port %q", portStr)
	}
	return host, port, nil
}

func parseAddr(value string) (*badoption.Addr, error) {
	addr := strings.TrimSpace(value)
	if addr == "" {
		return nil, nil
	}
	parsed, err := netip.ParseAddr(addr)
	if err != nil {
		return nil, err
	}
	bad := badoption.Addr(parsed)
	return &bad, nil
}

func sanitizeTag(name string) string {
	lower := strings.ToLower(name)
	lower = strings.TrimSpace(lower)
	if lower == "" {
		return ""
	}
	segments := strings.FieldsFunc(lower, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	result := strings.Join(segments, "-")
	result = strings.Trim(result, "-")
	return result
}

func atoiDefault(value string) int {
	if strings.HasSuffix(value, "mbps") {
		value = strings.TrimSuffix(value, "mbps")
	}
	if strings.HasSuffix(value, "Mbps") {
		value = strings.TrimSuffix(value, "Mbps")
	}
	v, _ := strconv.Atoi(value)
	return v
}

// printProxyLinks prints all proxy connection information at startup
func printProxyLinks(cfg *config.Config, metadata map[string]poolout.MemberMeta) {
	log.Println("")
	log.Println("📡 Proxy Links:")
	log.Println("═══════════════════════════════════════════════════════════════")

	showPoolEntry := cfg.Mode == "pool" || cfg.Mode == "hybrid"
	showMultiPort := cfg.Mode == "multi-port" || cfg.Mode == "hybrid"

	if showPoolEntry {
		// Pool mode: single entry point for all nodes
		var auth string
		if cfg.Listener.Username != "" {
			auth = fmt.Sprintf("%s:%s@", cfg.Listener.Username, cfg.Listener.Password)
		}
		log.Printf("🌐 Pool Entry Point:")
		for _, host := range proxyDisplayHosts(cfg.Listener.Address, cfg.ExternalIP) {
			proxyURL := fmt.Sprintf("http://%s%s:%d", auth, host, cfg.Listener.Port)
			log.Printf("   %s", proxyURL)
		}
		if (cfg.Listener.Address == "0.0.0.0" || cfg.Listener.Address == "::") && strings.TrimSpace(cfg.ExternalIP) == "" {
			log.Printf("   (tip) 0.0.0.0 is not a usable client address; set external_ip to print a public/LAN address")
		}
		log.Println("")
		log.Printf("   Nodes in pool (%d):", len(metadata))
		for _, meta := range metadata {
			log.Printf("   • %s", meta.Name)
		}
		if showMultiPort {
			log.Println("")
		}
	}

	if showMultiPort {
		// Multi-port mode: each node has its own port
		log.Printf("🔌 Multi-Port Entry Points (%d nodes):", len(cfg.Nodes))
		log.Println("")
		multiHosts := proxyDisplayHosts(cfg.MultiPort.Address, cfg.ExternalIP)
		multiHost := ""
		if len(multiHosts) > 0 {
			multiHost = multiHosts[0]
		}
		if (cfg.MultiPort.Address == "0.0.0.0" || cfg.MultiPort.Address == "::") && strings.TrimSpace(cfg.ExternalIP) == "" {
			log.Printf("   (tip) set external_ip for a stable address; using %q for display", multiHost)
		}
		for _, node := range cfg.Nodes {
			var auth string
			username := node.Username
			password := node.Password
			if username == "" {
				username = cfg.MultiPort.Username
				password = cfg.MultiPort.Password
			}
			if username != "" {
				auth = fmt.Sprintf("%s:%s@", username, password)
			}
			log.Printf("   [%d] %s", node.Port, node.Name)
			if multiHost != "" {
				proxyURL := fmt.Sprintf("http://%s%s:%d", auth, multiHost, node.Port)
				log.Printf("       %s", proxyURL)
			} else {
				log.Printf("       (no usable listen address)")
			}
		}
	}

	log.Println("═══════════════════════════════════════════════════════════════")
	log.Println("")
}

func proxyDisplayHosts(listenAddr string, externalIP string) []string {
	listenAddr = strings.TrimSpace(listenAddr)
	externalIP = strings.TrimSpace(externalIP)

	if listenAddr == "" {
		listenAddr = "0.0.0.0"
	}

	// If binding to a specific address, only print that (do not guess).
	if listenAddr != "0.0.0.0" && listenAddr != "::" {
		return []string{listenAddr}
	}

	hosts := make([]string, 0, 8)
	seen := make(map[string]struct{}, 8)
	add := func(h string) {
		h = strings.TrimSpace(h)
		if h == "" {
			return
		}
		if _, ok := seen[h]; ok {
			return
		}
		seen[h] = struct{}{}
		hosts = append(hosts, h)
	}

	if externalIP != "" {
		add(externalIP)
	}
	add("127.0.0.1")

	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 {
				continue
			}
			if iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				ip, _, err := net.ParseCIDR(addr.String())
				if err != nil || ip == nil {
					continue
				}
				if ip.IsLoopback() {
					continue
				}
				v4 := ip.To4()
				if v4 == nil {
					continue
				}
				// Skip link-local IPv4 (169.254.0.0/16)
				if v4[0] == 169 && v4[1] == 254 {
					continue
				}
				add(v4.String())
				// Avoid printing too many candidates.
				if len(hosts) >= 6 {
					return hosts
				}
			}
		}
	}

	return hosts
}
