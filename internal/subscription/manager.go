package subscription

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"easy_proxies/internal/boxmgr"
	"easy_proxies/internal/config"
	"easy_proxies/internal/monitor"
)

// Logger defines logging interface.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// Option configures the Manager.
type Option func(*Manager)

// WithLogger sets a custom logger.
func WithLogger(l Logger) Option {
	return func(m *Manager) { m.logger = l }
}

// Manager handles periodic subscription refresh.
type Manager struct {
	mu sync.RWMutex

	baseCfg *config.Config
	boxMgr  *boxmgr.Manager
	logger  Logger

	status        monitor.SubscriptionStatus
	ctx           context.Context
	cancel        context.CancelFunc
	refreshMu     sync.Mutex // prevents concurrent refreshes
	manualRefresh chan struct{}
	configChanged chan struct{}
	started       bool

	subscriptions []string
	refreshCfg    config.SubscriptionRefreshConfig

	// Track nodes.txt content hash to detect modifications
	lastSubHash      string    // Hash of nodes.txt content after last subscription refresh
	lastNodesModTime time.Time // Last known modification time of nodes.txt
}

// New creates a SubscriptionManager.
func New(cfg *config.Config, boxMgr *boxmgr.Manager, opts ...Option) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		baseCfg:       cfg,
		boxMgr:        boxMgr,
		ctx:           ctx,
		cancel:        cancel,
		manualRefresh: make(chan struct{}, 1),
		configChanged: make(chan struct{}, 1),
	}
	if cfg != nil {
		m.subscriptions = append([]string(nil), cfg.Subscriptions...)
		m.refreshCfg = cfg.SubscriptionRefresh
	}
	for _, opt := range opts {
		opt(m)
	}
	if m.logger == nil {
		m.logger = defaultLogger{}
	}

	// Initialize status/hash from existing nodes file (e.g., nodes.txt written on startup).
	// This prevents the UI from showing 0 nodes until the first timed refresh happens and
	// enables "nodes_modified" warnings immediately.
	m.initNodesStateFromFile()
	return m
}

// Start begins the periodic refresh loop.
func (m *Manager) Start() {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.mu.Unlock()

	m.logger.Infof("starting subscription manager loop")
	go m.refreshLoop()
}

// Stop stops the periodic refresh.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
}

// UpdateSettings updates subscriptions and refresh settings at runtime (thread-safe).
// This is used by the settings API so manual refresh / periodic refresh can take effect without restart.
func (m *Manager) UpdateSettings(subscriptions []string, refreshCfg config.SubscriptionRefreshConfig) {
	if m == nil {
		return
	}

	// Track changes to decide whether to trigger an immediate refresh.
	m.mu.Lock()
	prevSubs := append([]string(nil), m.subscriptions...)
	prevEnabled := m.refreshCfg.Enabled
	m.subscriptions = append([]string(nil), subscriptions...)
	m.refreshCfg = refreshCfg
	m.mu.Unlock()

	subsChanged := !equalStringSlices(prevSubs, subscriptions)
	enabledTurnedOn := !prevEnabled && refreshCfg.Enabled

	// If the user changed subscriptions (or just enabled auto-refresh), refresh once immediately.
	//
	// Note: We refresh even when auto-refresh is disabled so users can paste a subscription URL,
	// save settings, and see nodes without needing to enable the scheduler.
	if (subsChanged || enabledTurnedOn) && len(subscriptions) > 0 {
		select {
		case m.manualRefresh <- struct{}{}:
		default:
		}
	}

	select {
	case m.configChanged <- struct{}{}:
	default:
	}
}

// initNodesStateFromFile initializes node_count / last_refresh and nodes.txt hash baseline.
// This is best-effort and silently ignores errors.
func (m *Manager) initNodesStateFromFile() {
	if m == nil || m.baseCfg == nil {
		return
	}

	nodesFilePath := m.getNodesFilePath()
	if nodesFilePath == "" {
		return
	}

	info, err := os.Stat(nodesFilePath)
	if err != nil {
		return
	}

	data, err := os.ReadFile(nodesFilePath)
	if err != nil {
		return
	}

	var nodes []config.NodeConfig
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if isProxyURI(line) {
			nodes = append(nodes, config.NodeConfig{URI: line})
		}
	}

	hash := m.computeNodesHash(nodes)

	m.mu.Lock()
	m.lastSubHash = hash
	m.lastNodesModTime = info.ModTime()
	if len(nodes) > 0 {
		m.status.NodeCount = len(nodes)
		m.status.LastRefresh = info.ModTime()
	}
	m.status.NodesModified = false
	m.mu.Unlock()
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// RefreshNow triggers an immediate refresh.
func (m *Manager) RefreshNow() error {
	select {
	case m.manualRefresh <- struct{}{}:
	default:
		// Already a refresh pending
	}

	// Wait for refresh to complete or timeout
	refreshCfg := m.getRefreshConfig()
	timeout := refreshCfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	healthTimeout := refreshCfg.HealthCheckTimeout
	ctx, cancel := context.WithTimeout(m.ctx, timeout+healthTimeout)
	defer cancel()

	// Poll status until refresh completes
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	startCount := m.Status().RefreshCount
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("refresh timeout")
		case <-ticker.C:
			status := m.Status()
			if status.RefreshCount > startCount {
				if status.LastError != "" {
					return fmt.Errorf("refresh failed: %s", status.LastError)
				}
				return nil
			}
		}
	}
}

// Status returns the current refresh status.
func (m *Manager) Status() monitor.SubscriptionStatus {
	m.mu.RLock()
	status := m.status
	m.mu.RUnlock()

	// Check if nodes have been modified since last refresh
	status.NodesModified = m.CheckNodesModified()
	return status
}

func (m *Manager) refreshLoop() {
	for {
		subs, refreshCfg := m.getSettingsSnapshot()
		enabled := refreshCfg.Enabled && len(subs) > 0
		interval := refreshCfg.Interval
		if interval <= 0 {
			interval = 1 * time.Hour
		}

		if !enabled {
			m.mu.Lock()
			m.status.NextRefresh = time.Time{}
			m.mu.Unlock()
			select {
			case <-m.ctx.Done():
				return
			case <-m.manualRefresh:
				// Allow manual refresh even when auto refresh is disabled,
				// as long as subscriptions are configured.
				m.doRefresh()
			case <-m.configChanged:
				// Re-check settings.
			}
			continue
		}

		m.logger.Infof("subscription refresh enabled, interval: %s, sources: %d", interval, len(subs))

		ticker := time.NewTicker(interval)
		m.mu.Lock()
		m.status.NextRefresh = time.Now().Add(interval)
		m.mu.Unlock()

		for {
			select {
			case <-m.ctx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
				m.doRefresh()
				m.mu.Lock()
				m.status.NextRefresh = time.Now().Add(interval)
				m.mu.Unlock()
			case <-m.manualRefresh:
				m.doRefresh()
				ticker.Reset(interval)
				m.mu.Lock()
				m.status.NextRefresh = time.Now().Add(interval)
				m.mu.Unlock()
			case <-m.configChanged:
				ticker.Stop()
				goto recheck
			}
		}

	recheck:
		continue
	}
}

func (m *Manager) getRefreshConfig() config.SubscriptionRefreshConfig {
	m.mu.RLock()
	cfg := m.refreshCfg
	m.mu.RUnlock()
	return cfg
}

func (m *Manager) getSettingsSnapshot() ([]string, config.SubscriptionRefreshConfig) {
	m.mu.RLock()
	subs := append([]string(nil), m.subscriptions...)
	cfg := m.refreshCfg
	m.mu.RUnlock()
	return subs, cfg
}

// doRefresh performs a single refresh operation.
func (m *Manager) doRefresh() {
	// Prevent concurrent refreshes
	if !m.refreshMu.TryLock() {
		m.logger.Warnf("refresh already in progress, skipping")
		return
	}
	defer m.refreshMu.Unlock()

	m.mu.Lock()
	m.status.IsRefreshing = true
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		m.status.IsRefreshing = false
		m.status.RefreshCount++
		m.mu.Unlock()
	}()

	m.logger.Infof("starting subscription refresh")

	subs, refreshCfg := m.getSettingsSnapshot()
	if len(subs) == 0 {
		m.logger.Warnf("no subscriptions configured, skipping refresh")
		m.mu.Lock()
		m.status.LastError = "no subscriptions configured"
		m.status.LastRefresh = time.Now()
		m.mu.Unlock()
		return
	}

	// Fetch nodes from all subscriptions
	timeout := refreshCfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	nodes, err := m.fetchAllSubscriptions(subs, timeout)
	if err != nil {
		m.logger.Errorf("fetch subscriptions failed: %v", err)
		m.mu.Lock()
		m.status.LastError = err.Error()
		m.status.LastRefresh = time.Now()
		m.mu.Unlock()
		return
	}

	if len(nodes) == 0 {
		m.logger.Warnf("no nodes fetched from subscriptions")
		m.mu.Lock()
		m.status.LastError = "no nodes fetched"
		m.status.LastRefresh = time.Now()
		m.mu.Unlock()
		return
	}

	m.logger.Infof("fetched %d nodes from subscriptions", len(nodes))

	// Write subscription nodes to nodes.txt
	nodesFilePath := m.getNodesFilePath()
	if err := m.writeNodesToFile(nodesFilePath, nodes); err != nil {
		m.logger.Errorf("failed to write nodes.txt: %v", err)
		m.mu.Lock()
		m.status.LastError = fmt.Sprintf("write nodes.txt: %v", err)
		m.status.LastRefresh = time.Now()
		m.mu.Unlock()
		return
	}
	m.logger.Infof("written %d nodes to %s", len(nodes), nodesFilePath)

	// Update hash and mod time after writing
	newHash := m.computeNodesHash(nodes)
	m.mu.Lock()
	m.lastSubHash = newHash
	if info, err := os.Stat(nodesFilePath); err == nil {
		m.lastNodesModTime = info.ModTime()
	} else {
		m.lastNodesModTime = time.Now()
	}
	m.status.NodesModified = false
	m.mu.Unlock()

	// Get current port mapping to preserve existing node ports
	portMap := m.boxMgr.CurrentPortMap()

	// Create new config with updated nodes
	newCfg := m.createNewConfig(nodes)

	// Trigger BoxManager reload with port preservation
	if err := m.boxMgr.ReloadWithPortMap(newCfg, portMap); err != nil {
		m.logger.Errorf("reload failed: %v", err)
		m.mu.Lock()
		m.status.LastError = err.Error()
		m.status.LastRefresh = time.Now()
		m.mu.Unlock()
		return
	}

	m.mu.Lock()
	m.status.LastRefresh = time.Now()
	m.status.NodeCount = len(nodes)
	m.status.LastError = ""
	m.mu.Unlock()

	m.logger.Infof("subscription refresh completed, %d nodes active", len(nodes))
}

// getNodesFilePath returns the path to nodes.txt.
func (m *Manager) getNodesFilePath() string {
	if m.baseCfg.NodesFile != "" {
		return m.baseCfg.NodesFile
	}
	return filepath.Join(filepath.Dir(m.baseCfg.FilePath()), "nodes.txt")
}

// writeNodesToFile writes nodes to a file (one URI per line).
func (m *Manager) writeNodesToFile(path string, nodes []config.NodeConfig) error {
	var lines []string
	for _, node := range nodes {
		lines = append(lines, node.URI)
	}
	content := strings.Join(lines, "\n")
	if len(lines) > 0 {
		content += "\n"
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// computeNodesHash computes a hash of node URIs for change detection.
func (m *Manager) computeNodesHash(nodes []config.NodeConfig) string {
	var uris []string
	for _, node := range nodes {
		uris = append(uris, node.URI)
	}
	content := strings.Join(uris, "\n")
	hash := sha256.Sum256([]byte(content))
	return hex.EncodeToString(hash[:])
}

// CheckNodesModified checks if nodes.txt has been modified since last refresh.
// Uses file modification time as a fast path to avoid unnecessary file reads.
func (m *Manager) CheckNodesModified() bool {
	m.mu.RLock()
	lastHash := m.lastSubHash
	lastMod := m.lastNodesModTime
	m.mu.RUnlock()

	if lastHash == "" {
		return false // No previous refresh, can't determine modification
	}

	nodesFilePath := m.getNodesFilePath()

	// Fast path: check modification time first
	info, err := os.Stat(nodesFilePath)
	if err != nil {
		return false // File doesn't exist or can't stat
	}
	modTime := info.ModTime()
	if !modTime.After(lastMod) {
		return false // File hasn't been modified
	}

	// Slow path: file was modified, compute hash
	data, err := os.ReadFile(nodesFilePath)
	if err != nil {
		return false // File doesn't exist or can't read
	}

	// Parse nodes from file content
	var nodes []config.NodeConfig
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if isProxyURI(line) {
			nodes = append(nodes, config.NodeConfig{URI: line})
		}
	}

	currentHash := m.computeNodesHash(nodes)
	changed := currentHash != lastHash

	// Update cached mod time
	m.mu.Lock()
	m.lastNodesModTime = modTime
	m.mu.Unlock()

	return changed
}

// MarkNodesModified updates the modification status.
func (m *Manager) MarkNodesModified() {
	m.mu.Lock()
	m.status.NodesModified = true
	m.mu.Unlock()
}

// fetchAllSubscriptions fetches nodes from all configured subscription URLs.
func (m *Manager) fetchAllSubscriptions(subscriptions []string, timeout time.Duration) ([]config.NodeConfig, error) {
	var allNodes []config.NodeConfig
	var lastErr error

	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	for _, subURL := range subscriptions {
		nodes, err := m.fetchSubscription(subURL, timeout)
		if err != nil {
			m.logger.Warnf("failed to fetch %s: %v", subURL, err)
			lastErr = err
			continue
		}
		m.logger.Infof("fetched %d nodes from subscription", len(nodes))
		allNodes = append(allNodes, nodes...)
	}

	if len(allNodes) == 0 && lastErr != nil {
		return nil, lastErr
	}

	return allNodes, nil
}

// fetchSubscription fetches and parses a single subscription URL.
func (m *Manager) fetchSubscription(subURL string, timeout time.Duration) ([]config.NodeConfig, error) {
	subURL = config.NormalizeSubscriptionURL(subURL)
	if subURL == "" {
		return nil, fmt.Errorf("empty subscription url")
	}
	ctx, cancel := context.WithTimeout(m.ctx, timeout)
	defer cancel()

	userAgents := []string{
		"",
		"clash",
		"curl/8",
	}

	var lastErr error
	for _, ua := range userAgents {
		req, err := http.NewRequestWithContext(ctx, "GET", subURL, nil)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("fetch: %w", err)
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("read body: %w", readErr)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
			continue
		}

		nodes, parseErr := config.ParseSubscriptionContent(string(body))
		if parseErr != nil {
			lastErr = parseErr
			continue
		}
		if len(nodes) == 0 {
			lastErr = fmt.Errorf("no proxy nodes found in subscription response")
			continue
		}
		return nodes, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, nil
}

// createNewConfig creates a new config with updated nodes while preserving other settings.
func (m *Manager) createNewConfig(nodes []config.NodeConfig) *config.Config {
	// Deep copy base config
	newCfg := *m.baseCfg

	// Assign port numbers to nodes in multi-port mode
	if newCfg.Mode == "multi-port" {
		portCursor := newCfg.MultiPort.BasePort
		for i := range nodes {
			nodes[i].Port = portCursor
			portCursor++
			// Apply default credentials
			if nodes[i].Username == "" {
				nodes[i].Username = newCfg.MultiPort.Username
				nodes[i].Password = newCfg.MultiPort.Password
			}
		}
	}

	// Process node names
	for i := range nodes {
		nodes[i].Name = strings.TrimSpace(nodes[i].Name)
		nodes[i].URI = strings.TrimSpace(nodes[i].URI)

		// Extract name from URI fragment if not provided
		if nodes[i].Name == "" {
			if parsed, err := url.Parse(nodes[i].URI); err == nil {
				if parsed.Fragment != "" {
					if decoded, err := url.QueryUnescape(parsed.Fragment); err == nil {
						nodes[i].Name = decoded
					} else {
						nodes[i].Name = parsed.Fragment
					}
				}
				// Shadowrocket style: use remarks= as display name.
				if nodes[i].Name == "" {
					q := parsed.Query()
					if remarks := strings.TrimSpace(q.Get("remarks")); remarks != "" {
						nodes[i].Name = remarks
					} else if ps := strings.TrimSpace(q.Get("ps")); ps != "" {
						nodes[i].Name = ps
					}
				}
			}
		}
		if nodes[i].Name == "" && strings.HasPrefix(strings.ToLower(nodes[i].URI), "vmess://") {
			if ps := extractNameFromVMessURI(nodes[i].URI); ps != "" {
				nodes[i].Name = ps
			}
		}
		if nodes[i].Name == "" {
			nodes[i].Name = fmt.Sprintf("node-%d", i)
		}
	}

	newCfg.Nodes = nodes
	return &newCfg
}

func isProxyURI(s string) bool {
	schemes := []string{"vmess://", "vless://", "trojan://", "ss://", "ssr://", "hysteria://", "hysteria2://", "hy2://"}
	lower := strings.ToLower(s)
	for _, scheme := range schemes {
		if strings.HasPrefix(lower, scheme) {
			return true
		}
	}
	return false
}

type vmessJSONName struct {
	PS string `json:"ps"`
}

func extractNameFromVMessURI(uri string) string {
	encoded := strings.TrimSpace(strings.TrimPrefix(uri, "vmess://"))
	if encoded == "" {
		return ""
	}
	decoded, err := decodeBase64Any(encoded)
	if err != nil {
		return ""
	}
	var payload vmessJSONName
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.PS)
}

func decodeBase64Any(value string) ([]byte, error) {
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

type defaultLogger struct{}

func (defaultLogger) Infof(format string, args ...any) {
	log.Printf("[subscription] "+format, args...)
}

func (defaultLogger) Warnf(format string, args ...any) {
	log.Printf("[subscription] WARN: "+format, args...)
}

func (defaultLogger) Errorf(format string, args ...any) {
	log.Printf("[subscription] ERROR: "+format, args...)
}
