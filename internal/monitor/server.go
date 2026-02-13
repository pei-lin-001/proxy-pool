package monitor

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sort"
	"strings"
	"sync"
	"time"

	"easy_proxies/internal/config"
)

//go:embed assets/index.html
var embeddedFS embed.FS

// NodeManager exposes config node CRUD and reload operations.
type NodeManager interface {
	ListConfigNodes(ctx context.Context) ([]config.NodeConfig, error)
	CreateNode(ctx context.Context, node config.NodeConfig) (config.NodeConfig, error)
	UpdateNode(ctx context.Context, name string, node config.NodeConfig) (config.NodeConfig, error)
	DeleteNode(ctx context.Context, name string) error
	TriggerReload(ctx context.Context) error
}

// Sentinel errors for node operations.
var (
	ErrNodeNotFound = errors.New("节点不存在")
	ErrNodeConflict = errors.New("节点名称或端口已存在")
	ErrInvalidNode  = errors.New("无效的节点配置")
)

// SubscriptionRefresher interface for subscription manager.
type SubscriptionRefresher interface {
	RefreshNow() error
	Status() SubscriptionStatus
	UpdateSettings(subscriptions []string, refreshCfg config.SubscriptionRefreshConfig)
}

// SubscriptionStatus represents subscription refresh status.
type SubscriptionStatus struct {
	LastRefresh   time.Time `json:"last_refresh"`
	NextRefresh   time.Time `json:"next_refresh"`
	NodeCount     int       `json:"node_count"`
	LastError     string    `json:"last_error,omitempty"`
	RefreshCount  int       `json:"refresh_count"`
	IsRefreshing  bool      `json:"is_refreshing"`
	NodesModified bool      `json:"nodes_modified"` // True if nodes.txt was modified since last refresh
}

// Server exposes HTTP endpoints for monitoring.
type Server struct {
	cfg          Config
	cfgMu        sync.RWMutex   // 保护动态配置字段
	cfgSrc       *config.Config // 可持久化的配置对象
	mgr          *Manager
	srv          *http.Server
	logger       *log.Logger
	sessionToken string // 简单的 session token，重启后失效
	subRefresher SubscriptionRefresher
	nodeMgr      NodeManager
}

// NewServer constructs a server; it can be nil when disabled.
func NewServer(cfg Config, mgr *Manager, logger *log.Logger) *Server {
	if !cfg.Enabled || mgr == nil {
		return nil
	}
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{cfg: cfg, mgr: mgr, logger: logger}

	// 生成随机 session token
	tokenBytes := make([]byte, 32)
	rand.Read(tokenBytes)
	s.sessionToken = hex.EncodeToString(tokenBytes)

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/auth", s.handleAuth)
	mux.HandleFunc("/api/settings", s.withAuth(s.handleSettings))
	mux.HandleFunc("/api/geo/options", s.withAuth(s.handleGeoOptions))
	mux.HandleFunc("/api/nodes", s.withAuth(s.handleNodes))
	mux.HandleFunc("/api/nodes/config", s.withAuth(s.handleConfigNodes))
	mux.HandleFunc("/api/nodes/config/", s.withAuth(s.handleConfigNodeItem))
	mux.HandleFunc("/api/nodes/probe-all", s.withAuth(s.handleProbeAll))
	mux.HandleFunc("/api/nodes/", s.withAuth(s.handleNodeAction))
	mux.HandleFunc("/api/debug", s.withAuth(s.handleDebug))
	mux.HandleFunc("/api/export", s.withAuth(s.handleExport))
	mux.HandleFunc("/api/subscription/status", s.withAuth(s.handleSubscriptionStatus))
	mux.HandleFunc("/api/subscription/refresh", s.withAuth(s.handleSubscriptionRefresh))
	mux.HandleFunc("/api/reload", s.withAuth(s.handleReload))
	s.srv = &http.Server{Addr: cfg.Listen, Handler: mux}
	return s
}

// SetSubscriptionRefresher sets the subscription refresher for API endpoints.
func (s *Server) SetSubscriptionRefresher(sr SubscriptionRefresher) {
	if s != nil {
		s.subRefresher = sr
	}
}

// SetNodeManager enables config-node CRUD endpoints.
func (s *Server) SetNodeManager(nm NodeManager) {
	if s != nil {
		s.nodeMgr = nm
	}
}

// SetConfig binds the persistable config object for settings API.
func (s *Server) SetConfig(cfg *config.Config) {
	if s == nil {
		return
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	s.cfgSrc = cfg
	if cfg != nil {
		s.cfg.ExternalIP = cfg.ExternalIP
		s.cfg.ProbeTarget = cfg.Management.ProbeTarget
		s.cfg.SkipCertVerify = cfg.SkipCertVerify
		s.cfg.Password = cfg.Management.Password
	}
}

type settingsSnapshot struct {
	Mode     string `json:"mode"`
	LogLevel string `json:"log_level"`

	ConnectTimeout string        `json:"connect_timeout"`
	Listener       listenerView  `json:"listener"`
	MultiPort      multiPortView `json:"multi_port"`
	Pool           poolView      `json:"pool"`
	Management     managementView `json:"management"`

	ExternalIP          string                  `json:"external_ip"`
	ProbeTarget         string                  `json:"probe_target"`
	SkipCertVerify      bool                    `json:"skip_cert_verify"`
	Subscriptions       []string                `json:"subscriptions"`
	SubscriptionRefresh subscriptionRefreshView `json:"subscription_refresh"`
	NodeFilter          config.NodeFilterConfig `json:"node_filter"`
}

type listenerView struct {
	Address  string `json:"address"`
	Port     uint16 `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type multiPortView struct {
	Address  string `json:"address"`
	BasePort uint16 `json:"base_port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type poolView struct {
	Mode              string `json:"mode"`
	FailureThreshold  int    `json:"failure_threshold"`
	BlacklistDuration string `json:"blacklist_duration"`
}

type managementView struct {
	Enabled     bool   `json:"enabled"`
	Listen      string `json:"listen"`
	PasswordSet bool   `json:"password_set"`
}

type subscriptionRefreshView struct {
	Enabled            bool   `json:"enabled"`
	Interval           string `json:"interval"`
	Timeout            string `json:"timeout"`
	HealthCheckTimeout string `json:"health_check_timeout"`
	DrainTimeout       string `json:"drain_timeout"`
	MinAvailableNodes  int    `json:"min_available_nodes"`
}

func (s *Server) getSettingsSnapshot() settingsSnapshot {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()

	snap := settingsSnapshot{
		Mode:           "",
		LogLevel:       "",
		ConnectTimeout: "",
		Listener:       listenerView{},
		MultiPort:      multiPortView{},
		Pool:           poolView{},
		Management: managementView{
			Enabled:     s.cfg.Enabled,
			Listen:      s.cfg.Listen,
			PasswordSet: s.cfg.Password != "",
		},
		ExternalIP:     s.cfg.ExternalIP,
		ProbeTarget:    s.cfg.ProbeTarget,
		SkipCertVerify: s.cfg.SkipCertVerify,
		Subscriptions:  make([]string, 0),
	}
	if s.cfgSrc == nil {
		return snap
	}
	snap.Mode = s.cfgSrc.Mode
	snap.ExternalIP = s.cfgSrc.ExternalIP
	snap.ProbeTarget = s.cfgSrc.Management.ProbeTarget
	snap.SkipCertVerify = s.cfgSrc.SkipCertVerify
	snap.LogLevel = s.cfgSrc.LogLevel
	if s.cfgSrc.ConnectTimeout > 0 {
		snap.ConnectTimeout = s.cfgSrc.ConnectTimeout.String()
	}
	snap.Listener = listenerView{
		Address:  s.cfgSrc.Listener.Address,
		Port:     s.cfgSrc.Listener.Port,
		Username: s.cfgSrc.Listener.Username,
		Password: s.cfgSrc.Listener.Password,
	}
	snap.MultiPort = multiPortView{
		Address:  s.cfgSrc.MultiPort.Address,
		BasePort: s.cfgSrc.MultiPort.BasePort,
		Username: s.cfgSrc.MultiPort.Username,
		Password: s.cfgSrc.MultiPort.Password,
	}
	snap.Pool = poolView{
		Mode:              s.cfgSrc.Pool.Mode,
		FailureThreshold:  s.cfgSrc.Pool.FailureThreshold,
		BlacklistDuration: s.cfgSrc.Pool.BlacklistDuration.String(),
	}
	snap.Management = managementView{
		Enabled:     s.cfgSrc.ManagementEnabled(),
		Listen:      s.cfgSrc.Management.Listen,
		PasswordSet: strings.TrimSpace(s.cfgSrc.Management.Password) != "",
	}
	snap.Subscriptions = make([]string, 0, len(s.cfgSrc.Subscriptions))
	snap.Subscriptions = append(snap.Subscriptions, s.cfgSrc.Subscriptions...)
	snap.NodeFilter = s.cfgSrc.NodeFilter
	snap.SubscriptionRefresh = subscriptionRefreshView{
		Enabled:            s.cfgSrc.SubscriptionRefresh.Enabled,
		Interval:           s.cfgSrc.SubscriptionRefresh.Interval.String(),
		Timeout:            s.cfgSrc.SubscriptionRefresh.Timeout.String(),
		HealthCheckTimeout: s.cfgSrc.SubscriptionRefresh.HealthCheckTimeout.String(),
		DrainTimeout:       s.cfgSrc.SubscriptionRefresh.DrainTimeout.String(),
		MinAvailableNodes:  s.cfgSrc.SubscriptionRefresh.MinAvailableNodes,
	}
	return snap
}

type settingsUpdate struct {
	Mode           *string `json:"mode"`
	LogLevel       *string `json:"log_level"`
	ConnectTimeout *string `json:"connect_timeout"`

	Listener *struct {
		Address  *string `json:"address"`
		Port     *uint16 `json:"port"`
		Username *string `json:"username"`
		Password *string `json:"password"`
	} `json:"listener"`

	MultiPort *struct {
		Address  *string `json:"address"`
		BasePort *uint16 `json:"base_port"`
		Username *string `json:"username"`
		Password *string `json:"password"`
	} `json:"multi_port"`

	Pool *struct {
		Mode              *string `json:"mode"`
		FailureThreshold  *int    `json:"failure_threshold"`
		BlacklistDuration *string `json:"blacklist_duration"`
	} `json:"pool"`

	Management *struct {
		Enabled  *bool   `json:"enabled"`
		Listen   *string `json:"listen"`
		Password *string `json:"password"`
	} `json:"management"`

	ExternalIP     *string `json:"external_ip"`
	ProbeTarget    *string `json:"probe_target"`
	SkipCertVerify *bool   `json:"skip_cert_verify"`

	Subscriptions *[]string `json:"subscriptions"`

	SubscriptionRefresh *struct {
		Enabled            *bool   `json:"enabled"`
		Interval           *string `json:"interval"`
		Timeout            *string `json:"timeout"`
		HealthCheckTimeout *string `json:"health_check_timeout"`
		DrainTimeout       *string `json:"drain_timeout"`
		MinAvailableNodes  *int    `json:"min_available_nodes"`
	} `json:"subscription_refresh"`

	NodeFilter *struct {
		Target   *string   `json:"target"`
		Include  *[]string `json:"include"`
		Exclude  *[]string `json:"exclude"`
		UseRegex *bool     `json:"use_regex"`
	} `json:"node_filter"`
}

type httpError struct {
	status  int
	message string
}

func (e httpError) Error() string { return e.message }

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

func normalizeStringList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	return out
}

// updateSettings applies a partial update, persists to config file, and returns whether reload is needed.
func (s *Server) updateSettings(update settingsUpdate) (needReload bool, err error) {
	var (
		shouldUpdateProbeTarget     bool
		newProbeTarget              string
		subscriptionSettingsChanged bool
	)

	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	if s.cfgSrc == nil {
		return false, httpError{status: http.StatusInternalServerError, message: "配置存储未初始化"}
	}

	// Backup for rollback on save failure
	prevMode := s.cfgSrc.Mode
	prevLogLevel := s.cfgSrc.LogLevel
	prevConnectTimeout := s.cfgSrc.ConnectTimeout
	prevListener := s.cfgSrc.Listener
	prevMultiPort := s.cfgSrc.MultiPort
	prevPool := s.cfgSrc.Pool
	prevMgmtEnabled := s.cfgSrc.Management.Enabled
	prevMgmtListen := s.cfgSrc.Management.Listen
	prevMgmtPassword := s.cfgSrc.Management.Password

	prevExternalIP := s.cfgSrc.ExternalIP
	prevProbeTarget := s.cfgSrc.Management.ProbeTarget
	prevSkip := s.cfgSrc.SkipCertVerify
	prevSubs := append([]string(nil), s.cfgSrc.Subscriptions...)
	prevRefresh := s.cfgSrc.SubscriptionRefresh
	prevFilter := s.cfgSrc.NodeFilter
	prevFilter.Include = append([]string(nil), prevFilter.Include...)
	prevFilter.Exclude = append([]string(nil), prevFilter.Exclude...)

	// Apply updates
	if update.Mode != nil {
		next := strings.ToLower(strings.TrimSpace(*update.Mode))
		if next == "" {
			next = "pool"
		}
		if next == "multi_port" {
			next = "multi-port"
		}
		switch next {
		case "pool", "multi-port", "hybrid":
		default:
			return false, httpError{status: http.StatusBadRequest, message: "mode 只能是 pool / multi-port / hybrid"}
		}
		if next != prevMode {
			needReload = true
		}
		s.cfgSrc.Mode = next
	}

	if update.LogLevel != nil {
		next := strings.ToLower(strings.TrimSpace(*update.LogLevel))
		if next == "" {
			next = "info"
		}
		if next == "warning" {
			next = "warn"
		}
		switch next {
		case "trace", "debug", "info", "warn", "error", "fatal", "panic":
		default:
			return false, httpError{status: http.StatusBadRequest, message: "log_level 只能是 trace/debug/info/warn/error/fatal/panic"}
		}
		prevNorm := strings.ToLower(strings.TrimSpace(prevLogLevel))
		if prevNorm == "" {
			prevNorm = "info"
		}
		if next != prevNorm {
			needReload = true
		}
		s.cfgSrc.LogLevel = next
	}

	if update.ConnectTimeout != nil {
		raw := strings.TrimSpace(*update.ConnectTimeout)
		if raw != "" {
			d, parseErr := time.ParseDuration(raw)
			if parseErr != nil {
				return false, httpError{status: http.StatusBadRequest, message: fmt.Sprintf("无效 connect_timeout: %v", parseErr)}
			}
			if d != prevConnectTimeout {
				needReload = true
			}
			s.cfgSrc.ConnectTimeout = d
		}
	}

	if update.Listener != nil {
		if update.Listener.Address != nil {
			next := strings.TrimSpace(*update.Listener.Address)
			if next != "" && net.ParseIP(next) == nil {
				return false, httpError{status: http.StatusBadRequest, message: "listener.address 必须是 IP（例如 0.0.0.0 或 127.0.0.1）"}
			}
			if next != prevListener.Address {
				needReload = true
			}
			s.cfgSrc.Listener.Address = next
		}
		if update.Listener.Port != nil {
			next := *update.Listener.Port
			if next == 0 {
				return false, httpError{status: http.StatusBadRequest, message: "listener.port 必须在 1-65535"}
			}
			if next != prevListener.Port {
				needReload = true
			}
			s.cfgSrc.Listener.Port = next
		}
		if update.Listener.Username != nil {
			next := strings.TrimSpace(*update.Listener.Username)
			if next != prevListener.Username {
				needReload = true
			}
			s.cfgSrc.Listener.Username = next
		}
		if update.Listener.Password != nil {
			next := *update.Listener.Password
			if next != prevListener.Password {
				needReload = true
			}
			s.cfgSrc.Listener.Password = next
		}
	}

	if update.MultiPort != nil {
		if update.MultiPort.Address != nil {
			next := strings.TrimSpace(*update.MultiPort.Address)
			if next != "" && net.ParseIP(next) == nil {
				return false, httpError{status: http.StatusBadRequest, message: "multi_port.address 必须是 IP（例如 0.0.0.0 或 127.0.0.1）"}
			}
			if next != prevMultiPort.Address {
				needReload = true
			}
			s.cfgSrc.MultiPort.Address = next
		}
		if update.MultiPort.BasePort != nil {
			next := *update.MultiPort.BasePort
			if next == 0 {
				return false, httpError{status: http.StatusBadRequest, message: "multi_port.base_port 必须在 1-65535"}
			}
			if next != prevMultiPort.BasePort {
				needReload = true
			}
			s.cfgSrc.MultiPort.BasePort = next
		}
		if update.MultiPort.Username != nil {
			next := strings.TrimSpace(*update.MultiPort.Username)
			if next != prevMultiPort.Username {
				needReload = true
			}
			s.cfgSrc.MultiPort.Username = next
		}
		if update.MultiPort.Password != nil {
			next := *update.MultiPort.Password
			if next != prevMultiPort.Password {
				needReload = true
			}
			s.cfgSrc.MultiPort.Password = next
		}
	}

	if update.Pool != nil {
		if update.Pool.Mode != nil {
			next := strings.ToLower(strings.TrimSpace(*update.Pool.Mode))
			if next == "" {
				next = "sequential"
			}
			switch next {
			case "sequential", "random", "balance":
			default:
				return false, httpError{status: http.StatusBadRequest, message: "pool.mode 只能是 sequential / random / balance"}
			}
			if next != strings.ToLower(strings.TrimSpace(prevPool.Mode)) {
				needReload = true
			}
			s.cfgSrc.Pool.Mode = next
		}
		if update.Pool.FailureThreshold != nil {
			next := *update.Pool.FailureThreshold
			if next <= 0 {
				return false, httpError{status: http.StatusBadRequest, message: "pool.failure_threshold 必须大于 0"}
			}
			if next != prevPool.FailureThreshold {
				needReload = true
			}
			s.cfgSrc.Pool.FailureThreshold = next
		}
		if update.Pool.BlacklistDuration != nil {
			raw := strings.TrimSpace(*update.Pool.BlacklistDuration)
			if raw != "" {
				d, parseErr := time.ParseDuration(raw)
				if parseErr != nil {
					return false, httpError{status: http.StatusBadRequest, message: fmt.Sprintf("无效 pool.blacklist_duration: %v", parseErr)}
				}
				if d != prevPool.BlacklistDuration {
					needReload = true
				}
				s.cfgSrc.Pool.BlacklistDuration = d
			}
		}
	}

	if update.Management != nil {
		if update.Management.Enabled != nil {
			next := *update.Management.Enabled
			if prevMgmtEnabled == nil || next != *prevMgmtEnabled {
				// WebUI enable/disable does not require sing-box reload.
			}
			s.cfgSrc.Management.Enabled = &next
		}
		if update.Management.Listen != nil {
			next := strings.TrimSpace(*update.Management.Listen)
			if next != prevMgmtListen {
				// Changing management.listen requires restarting the monitor server to take effect.
			}
			s.cfgSrc.Management.Listen = next
		}
		if update.Management.Password != nil {
			next := *update.Management.Password
			if next != prevMgmtPassword {
				// No reload needed; affects auth only.
			}
			s.cfgSrc.Management.Password = next
			s.cfg.Password = next
		}
	}

	if update.ExternalIP != nil {
		s.cfgSrc.ExternalIP = strings.TrimSpace(*update.ExternalIP)
		s.cfg.ExternalIP = s.cfgSrc.ExternalIP
	}
	if update.ProbeTarget != nil {
		newProbeTarget = strings.TrimSpace(*update.ProbeTarget)
		s.cfgSrc.Management.ProbeTarget = newProbeTarget
		s.cfg.ProbeTarget = newProbeTarget
		shouldUpdateProbeTarget = true
	}
	if update.SkipCertVerify != nil {
		next := *update.SkipCertVerify
		if next != prevSkip {
			needReload = true
		}
		s.cfgSrc.SkipCertVerify = next
		s.cfg.SkipCertVerify = s.cfgSrc.SkipCertVerify
	}

	if update.Subscriptions != nil {
		subscriptionSettingsChanged = true
		unique := make(map[string]struct{})
		subs := make([]string, 0, len(*update.Subscriptions))
		for _, raw := range *update.Subscriptions {
			raw = config.NormalizeSubscriptionURL(raw)
			if raw == "" {
				continue
			}
			if _, ok := unique[raw]; ok {
				continue
			}
			unique[raw] = struct{}{}
			subs = append(subs, raw)
		}
		s.cfgSrc.Subscriptions = subs
	}

	if update.SubscriptionRefresh != nil {
		subscriptionSettingsChanged = true
		if update.SubscriptionRefresh.Enabled != nil {
			next := *update.SubscriptionRefresh.Enabled
			s.cfgSrc.SubscriptionRefresh.Enabled = next
		}
		if update.SubscriptionRefresh.Interval != nil {
			d, parseErr := time.ParseDuration(strings.TrimSpace(*update.SubscriptionRefresh.Interval))
			if parseErr != nil {
				return false, httpError{status: http.StatusBadRequest, message: fmt.Sprintf("无效 interval: %v", parseErr)}
			}
			s.cfgSrc.SubscriptionRefresh.Interval = d
		}
		if update.SubscriptionRefresh.Timeout != nil {
			d, parseErr := time.ParseDuration(strings.TrimSpace(*update.SubscriptionRefresh.Timeout))
			if parseErr != nil {
				return false, httpError{status: http.StatusBadRequest, message: fmt.Sprintf("无效 timeout: %v", parseErr)}
			}
			s.cfgSrc.SubscriptionRefresh.Timeout = d
		}
		if update.SubscriptionRefresh.HealthCheckTimeout != nil {
			d, parseErr := time.ParseDuration(strings.TrimSpace(*update.SubscriptionRefresh.HealthCheckTimeout))
			if parseErr != nil {
				return false, httpError{status: http.StatusBadRequest, message: fmt.Sprintf("无效 health_check_timeout: %v", parseErr)}
			}
			s.cfgSrc.SubscriptionRefresh.HealthCheckTimeout = d
		}
		if update.SubscriptionRefresh.DrainTimeout != nil {
			d, parseErr := time.ParseDuration(strings.TrimSpace(*update.SubscriptionRefresh.DrainTimeout))
			if parseErr != nil {
				return false, httpError{status: http.StatusBadRequest, message: fmt.Sprintf("无效 drain_timeout: %v", parseErr)}
			}
			s.cfgSrc.SubscriptionRefresh.DrainTimeout = d
		}
		if update.SubscriptionRefresh.MinAvailableNodes != nil {
			next := *update.SubscriptionRefresh.MinAvailableNodes
			s.cfgSrc.SubscriptionRefresh.MinAvailableNodes = next
		}
	}

	if update.NodeFilter != nil {
		if update.NodeFilter.Target != nil {
			next := strings.ToLower(strings.TrimSpace(*update.NodeFilter.Target))
			if next == "" {
				next = "name"
			}
			switch next {
			case "name", "geo", "name_or_geo":
			default:
				return false, httpError{status: http.StatusBadRequest, message: "node_filter.target 只能是 name / geo / name_or_geo"}
			}
			prevTarget := strings.ToLower(strings.TrimSpace(prevFilter.Target))
			if prevTarget == "" {
				prevTarget = "name"
			}
			if next != prevTarget {
				needReload = true
			}
			s.cfgSrc.NodeFilter.Target = next
		}
		if update.NodeFilter.Include != nil {
			next := normalizeStringList(*update.NodeFilter.Include)
			if !equalStringSlices(next, prevFilter.Include) {
				needReload = true
			}
			s.cfgSrc.NodeFilter.Include = next
		}
		if update.NodeFilter.Exclude != nil {
			next := normalizeStringList(*update.NodeFilter.Exclude)
			if !equalStringSlices(next, prevFilter.Exclude) {
				needReload = true
			}
			s.cfgSrc.NodeFilter.Exclude = next
		}
		if update.NodeFilter.UseRegex != nil {
			next := *update.NodeFilter.UseRegex
			if next != prevFilter.UseRegex {
				needReload = true
			}
			s.cfgSrc.NodeFilter.UseRegex = next
		}
	}

	// Persist changes
	if saveErr := s.cfgSrc.SaveSettings(); saveErr != nil {
		// Rollback
		s.cfgSrc.Mode = prevMode
		s.cfgSrc.LogLevel = prevLogLevel
		s.cfgSrc.ConnectTimeout = prevConnectTimeout
		s.cfgSrc.Listener = prevListener
		s.cfgSrc.MultiPort = prevMultiPort
		s.cfgSrc.Pool = prevPool
		s.cfgSrc.Management.Enabled = prevMgmtEnabled
		s.cfgSrc.Management.Listen = prevMgmtListen
		s.cfgSrc.Management.Password = prevMgmtPassword

		s.cfgSrc.ExternalIP = prevExternalIP
		s.cfgSrc.Management.ProbeTarget = prevProbeTarget
		s.cfgSrc.SkipCertVerify = prevSkip
		s.cfgSrc.Subscriptions = prevSubs
		s.cfgSrc.SubscriptionRefresh = prevRefresh
		s.cfgSrc.NodeFilter = prevFilter

		s.cfg.Password = prevMgmtPassword
		s.cfg.ExternalIP = prevExternalIP
		s.cfg.ProbeTarget = prevProbeTarget
		s.cfg.SkipCertVerify = prevSkip

		return false, httpError{status: http.StatusInternalServerError, message: fmt.Sprintf("保存配置失败: %v", saveErr)}
	}

	if subscriptionSettingsChanged && s.subRefresher != nil {
		s.subRefresher.UpdateSettings(s.cfgSrc.Subscriptions, s.cfgSrc.SubscriptionRefresh)
	}

	// Apply probe target to monitor manager immediately (no reload needed).
	if shouldUpdateProbeTarget && s.mgr != nil {
		s.mgr.SetProbeTarget(newProbeTarget)
	}

	return needReload, nil
}

// Start launches the HTTP server.
func (s *Server) Start(ctx context.Context) {
	if s == nil || s.srv == nil {
		return
	}
	s.logger.Printf("Starting monitor server on %s", s.cfg.Listen)
	go func() {
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Printf("❌ Monitor server error: %v", err)
		}
	}()
	// Give server a moment to start and check for immediate errors
	time.Sleep(100 * time.Millisecond)
	s.logger.Printf("✅ Monitor server started on http://%s", s.cfg.Listen)

	go func() {
		<-ctx.Done()
		s.Shutdown(context.Background())
	}()
}

// Shutdown stops the server gracefully.
func (s *Server) Shutdown(ctx context.Context) {
	if s == nil || s.srv == nil {
		return
	}
	_ = s.srv.Shutdown(ctx)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := embeddedFS.ReadFile("assets/index.html")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// 返回所有节点（包含不可用节点），避免“节点已加载但全部探测失败”时前端误判为“没有节点”。
	// 前端会根据 failure_count / blacklisted 展示状态；导出接口仍只导出可用节点。
	payload := map[string]any{"nodes": s.mgr.SnapshotFiltered(false)}
	writeJSON(w, payload)
}

func (s *Server) handleDebug(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	snapshots := s.mgr.Snapshot()
	var totalCalls, totalSuccess int64
	debugNodes := make([]map[string]any, 0, len(snapshots))
	for _, snap := range snapshots {
		totalCalls += snap.SuccessCount + int64(snap.FailureCount)
		totalSuccess += snap.SuccessCount
		debugNodes = append(debugNodes, map[string]any{
			"tag":                snap.Tag,
			"name":               snap.Name,
			"mode":               snap.Mode,
			"port":               snap.Port,
			"failure_count":      snap.FailureCount,
			"success_count":      snap.SuccessCount,
			"active_connections": snap.ActiveConnections,
			"last_latency_ms":    snap.LastLatencyMs,
			"last_success":       snap.LastSuccess,
			"last_failure":       snap.LastFailure,
			"last_error":         snap.LastError,
			"blacklisted":        snap.Blacklisted,
			"timeline":           snap.Timeline,
		})
	}
	var successRate float64
	if totalCalls > 0 {
		successRate = float64(totalSuccess) / float64(totalCalls) * 100
	}
	writeJSON(w, map[string]any{
		"nodes":         debugNodes,
		"total_calls":   totalCalls,
		"total_success": totalSuccess,
		"success_rate":  successRate,
	})
}

func (s *Server) handleNodeAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/nodes/"), "/")
	if len(parts) < 1 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	tag := parts[0]
	if tag == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	switch action {
	case "probe":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		// Some nodes (especially with Reality/XTLS) can take longer to complete handshake on poor networks.
		// Use a slightly longer timeout for manual probe to reduce false negatives.
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		latency, err := s.mgr.Probe(ctx, tag)
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		latencyMs := latency.Milliseconds()
		if latencyMs == 0 && latency > 0 {
			latencyMs = 1 // Round up sub-millisecond latencies to 1ms
		}
		writeJSON(w, map[string]any{"message": "探测成功", "latency_ms": latencyMs})
	case "release":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := s.mgr.Release(tag); err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"message": "已解除拉黑"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// handleProbeAll probes all nodes in batches and returns results via SSE
func (s *Server) handleProbeAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	// Get all nodes
	snapshots := s.mgr.Snapshot()
	total := len(snapshots)
	if total == 0 {
		fmt.Fprintf(w, "data: %s\n\n", `{"type":"complete","total":0,"success":0,"failed":0}`)
		flusher.Flush()
		return
	}

	// Send start event
	fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(`{"type":"start","total":%d}`, total))
	flusher.Flush()

	// Probe all nodes concurrently
	type probeResult struct {
		tag     string
		name    string
		latency int64
		err     string
	}
	results := make(chan probeResult, total)

	// Launch all probes concurrently
	for _, snap := range snapshots {
		go func(snap Snapshot, mgr *Manager) {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			latency, err := mgr.Probe(ctx, snap.Tag)
			if err != nil {
				results <- probeResult{tag: snap.Tag, name: snap.Name, latency: -1, err: err.Error()}
			} else {
				results <- probeResult{tag: snap.Tag, name: snap.Name, latency: latency.Milliseconds(), err: ""}
			}
		}(snap, s.mgr)
	}

	// Collect results as they come in with overall timeout
	successCount := 0
	failedCount := 0
	timeout := time.After(30 * time.Second) // Overall timeout for all probes

	for i := 0; i < total; i++ {
		select {
		case result := <-results:
			if result.err != "" {
				failedCount++
			} else {
				successCount++
			}
			current := successCount + failedCount
			progress := float64(current) / float64(total) * 100
			eventData := fmt.Sprintf(`{"type":"progress","tag":"%s","name":"%s","latency":%d,"error":"%s","current":%d,"total":%d,"progress":%.1f}`,
				result.tag, result.name, result.latency, result.err, current, total, progress)
			fmt.Fprintf(w, "data: %s\n\n", eventData)
			flusher.Flush()
		case <-timeout:
			// Overall timeout reached, report remaining nodes as timed out
			remaining := total - (successCount + failedCount)
			for j := 0; j < remaining; j++ {
				failedCount++
				current := successCount + failedCount
				progress := float64(current) / float64(total) * 100
				eventData := fmt.Sprintf(`{"type":"progress","tag":"unknown","name":"超时节点","latency":-1,"error":"overall timeout","current":%d,"total":%d,"progress":%.1f}`,
					current, total, progress)
				fmt.Fprintf(w, "data: %s\n\n", eventData)
				flusher.Flush()
			}
			goto complete
		}
	}

complete:

	// Send complete event
	fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(`{"type":"complete","total":%d,"success":%d,"failed":%d}`, total, successCount, failedCount))
	flusher.Flush()
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

// withAuth 认证中间件，如果配置了密码则需要验证
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 如果没有配置密码，直接放行
		if s.cfg.Password == "" {
			next(w, r)
			return
		}

		// 检查 Cookie 中的 session token
		cookie, err := r.Cookie("session_token")
		if err == nil && cookie.Value == s.sessionToken {
			next(w, r)
			return
		}

		// 检查 Authorization header (Bearer token)
		authHeader := r.Header.Get("Authorization")
		if authHeader != "" {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if token == s.sessionToken {
				next(w, r)
				return
			}
		}

		// 未授权
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": "未授权，请先登录"})
	}
}

// handleAuth 处理登录认证
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	// 如果没有配置密码，直接返回成功（不需要token）
	if s.cfg.Password == "" {
		writeJSON(w, map[string]any{"message": "无需密码", "no_password": true})
		return
	}

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Password string `json:"password"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "请求格式错误"})
		return
	}

	// 验证密码
	if req.Password != s.cfg.Password {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": "密码错误"})
		return
	}

	// 设置 cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    s.sessionToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   86400 * 7, // 7天
	})

	writeJSON(w, map[string]any{
		"message": "登录成功",
		"token":   s.sessionToken,
	})
}

// handleExport 导出所有可用代理池节点的 HTTP 代理 URI，每行一个
// 在 hybrid 模式下，只导出 multi-port 格式（每节点独立端口）
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// 只导出初始检查通过的可用节点
	snapshots := s.mgr.SnapshotFiltered(true)
	var lines []string

	settings := s.getSettingsSnapshot()
	requestHost := extractHost(r)

	authUser := ""
	authPass := ""
	if settings.Mode == "multi-port" || settings.Mode == "hybrid" {
		authUser = settings.MultiPort.Username
		authPass = settings.MultiPort.Password
	} else {
		authUser = settings.Listener.Username
		authPass = settings.Listener.Password
	}

	for _, snap := range snapshots {
		// 只导出有监听地址和端口的节点
		if snap.ListenAddress == "" || snap.Port == 0 {
			continue
		}

		// 在 hybrid 和 multi-port 模式下，导出每节点独立端口
		// 在 pool 模式下，所有节点共享同一端口，也正常导出
		listenAddr := snap.ListenAddress
		if listenAddr == "0.0.0.0" || listenAddr == "::" {
			if settings.ExternalIP != "" {
				listenAddr = settings.ExternalIP
			} else if requestHost != "" {
				// When listening on all interfaces, exporting 0.0.0.0 is not usable.
				// Prefer the host that the user used to access the WebUI.
				listenAddr = requestHost
			}
		}

		proxyURL := &url.URL{
			Scheme: "http",
			Host:   net.JoinHostPort(listenAddr, strconv.Itoa(int(snap.Port))),
		}
		if strings.TrimSpace(authUser) != "" {
			proxyURL.User = url.UserPassword(authUser, authPass)
		}
		lines = append(lines, proxyURL.String())
	}

	// 返回纯文本，每行一个 URI
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=proxy_pool.txt")
	_, _ = w.Write([]byte(strings.Join(lines, "\n")))
}

func extractHost(r *http.Request) string {
	if r == nil {
		return ""
	}
	host := strings.TrimSpace(r.Host)
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if host == "0.0.0.0" || host == "::" {
		return ""
	}
	return host
}

// handleSettings handles GET/PUT for dynamic settings (external_ip, probe_target, skip_cert_verify).
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.getSettingsSnapshot())
	case http.MethodPut:
		var req settingsUpdate
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "请求格式错误"})
			return
		}
		needReload, err := s.updateSettings(req)
		if err != nil {
			status := http.StatusInternalServerError
			var he httpError
			if errors.As(err, &he) && he.status != 0 {
				status = he.status
			}
			w.WriteHeader(status)
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}

		snap := s.getSettingsSnapshot()
		writeJSON(w, map[string]any{
			"message":     "设置已保存",
			"settings":    snap,
			"need_reload": needReload,
		})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

type geoOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

type geoOptionsResponse struct {
	TotalNodes    int         `json:"total_nodes"`
	ResolvedNodes int         `json:"resolved_nodes"`
	UnknownNodes  int         `json:"unknown_nodes"`
	Countries     []geoOption `json:"countries"`
	Regions       []geoOption `json:"regions"`
	Cities        []geoOption `json:"cities"`
}

func (s *Server) handleGeoOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.ensureNodeManager(w) {
		return
	}

	nodes, err := s.nodeMgr.ListConfigNodes(r.Context())
	if err != nil {
		s.respondNodeError(w, err)
		return
	}

	// Resolve GeoIP in-memory (best effort). This does not persist anything.
	tmp := &config.Config{Nodes: nodes}
	tmp.PopulateNodeGeo(r.Context())
	nodes = tmp.Nodes

	type counter struct {
		label string
		count int
	}

	countryMap := make(map[string]*counter)
	regionMap := make(map[string]*counter)
	cityMap := make(map[string]*counter)

	resolved := 0
	unknown := 0
	for _, n := range nodes {
		if n.Geo == nil || strings.TrimSpace(n.Geo.Error) != "" {
			unknown++
			continue
		}
		cc := strings.TrimSpace(n.Geo.CountryCode)
		country := strings.TrimSpace(n.Geo.Country)
		region := strings.TrimSpace(n.Geo.Region)
		city := strings.TrimSpace(n.Geo.City)

		if cc == "" && country == "" && region == "" && city == "" {
			unknown++
			continue
		}
		resolved++

		if cc != "" || country != "" {
			key := cc
			if key == "" {
				key = country
			}
			label := strings.TrimSpace(strings.Join([]string{cc, country}, " "))
			label = strings.TrimSpace(strings.ReplaceAll(label, "  ", " "))
			if label == "" {
				label = key
			}
			if c, ok := countryMap[key]; ok {
				c.count++
			} else {
				countryMap[key] = &counter{label: label, count: 1}
			}
		}

		if region != "" {
			key := region
			label := region
			if cc != "" {
				label = fmt.Sprintf("%s · %s", cc, region)
			}
			if c, ok := regionMap[key]; ok {
				c.count++
			} else {
				regionMap[key] = &counter{label: label, count: 1}
			}
		}

		if city != "" {
			key := city
			label := city
			if cc != "" {
				label = fmt.Sprintf("%s · %s", cc, city)
			}
			if c, ok := cityMap[key]; ok {
				c.count++
			} else {
				cityMap[key] = &counter{label: label, count: 1}
			}
		}
	}

	toSorted := func(m map[string]*counter) []geoOption {
		out := make([]geoOption, 0, len(m))
		for value, c := range m {
			out = append(out, geoOption{Value: value, Label: c.label, Count: c.count})
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Count != out[j].Count {
				return out[i].Count > out[j].Count
			}
			return out[i].Label < out[j].Label
		})
		return out
	}

	resp := geoOptionsResponse{
		TotalNodes:    len(nodes),
		ResolvedNodes: resolved,
		UnknownNodes:  unknown,
		Countries:     toSorted(countryMap),
		Regions:       toSorted(regionMap),
		Cities:        toSorted(cityMap),
	}
	writeJSON(w, resp)
}

// handleSubscriptionStatus returns the current subscription refresh status.
func (s *Server) handleSubscriptionStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if s.subRefresher == nil {
		writeJSON(w, map[string]any{
			"enabled": false,
			"message": "订阅刷新未启用",
		})
		return
	}

	hasSubscriptions := false
	autoEnabled := false
	s.cfgMu.RLock()
	if s.cfgSrc != nil {
		hasSubscriptions = len(s.cfgSrc.Subscriptions) > 0
		autoEnabled = hasSubscriptions && s.cfgSrc.SubscriptionRefresh.Enabled
	}
	s.cfgMu.RUnlock()

	status := s.subRefresher.Status()
	writeJSON(w, map[string]any{
		// enabled means "subscription feature is configured" so UI can show the refresh button.
		"enabled":        hasSubscriptions,
		"auto_enabled":   autoEnabled,
		"last_refresh":   status.LastRefresh,
		"next_refresh":   status.NextRefresh,
		"node_count":     status.NodeCount,
		"last_error":     status.LastError,
		"refresh_count":  status.RefreshCount,
		"is_refreshing":  status.IsRefreshing,
		"nodes_modified": status.NodesModified,
	})
}

// handleSubscriptionRefresh triggers an immediate subscription refresh.
func (s *Server) handleSubscriptionRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if s.subRefresher == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]any{"error": "订阅刷新未启用"})
		return
	}
	s.cfgMu.RLock()
	hasSubscriptions := s.cfgSrc != nil && len(s.cfgSrc.Subscriptions) > 0
	s.cfgMu.RUnlock()
	if !hasSubscriptions {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]any{"error": "未配置订阅链接"})
		return
	}

	if err := s.subRefresher.RefreshNow(); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}

	status := s.subRefresher.Status()
	writeJSON(w, map[string]any{
		"message":    "刷新成功",
		"node_count": status.NodeCount,
	})
}

// nodePayload is the JSON request body for node CRUD operations.
type nodePayload struct {
	Name     string `json:"name"`
	URI      string `json:"uri"`
	Port     uint16 `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (p nodePayload) toConfig() config.NodeConfig {
	return config.NodeConfig{
		Name:     p.Name,
		URI:      p.URI,
		Port:     p.Port,
		Username: p.Username,
		Password: p.Password,
	}
}

// handleConfigNodes handles GET (list) and POST (create) for config nodes.
func (s *Server) handleConfigNodes(w http.ResponseWriter, r *http.Request) {
	if !s.ensureNodeManager(w) {
		return
	}

	switch r.Method {
	case http.MethodGet:
		nodes, err := s.nodeMgr.ListConfigNodes(r.Context())
		if err != nil {
			s.respondNodeError(w, err)
			return
		}
		writeJSON(w, map[string]any{"nodes": nodes})
	case http.MethodPost:
		var payload nodePayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "请求格式错误"})
			return
		}
		node, err := s.nodeMgr.CreateNode(r.Context(), payload.toConfig())
		if err != nil {
			s.respondNodeError(w, err)
			return
		}
		writeJSON(w, map[string]any{"node": node, "message": "节点已添加，请点击重载使配置生效"})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleConfigNodeItem handles PUT (update) and DELETE for a specific config node.
func (s *Server) handleConfigNodeItem(w http.ResponseWriter, r *http.Request) {
	if !s.ensureNodeManager(w) {
		return
	}

	namePart := strings.TrimPrefix(r.URL.Path, "/api/nodes/config/")
	nodeName, err := url.PathUnescape(namePart)
	if err != nil || nodeName == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "节点名称无效"})
		return
	}

	switch r.Method {
	case http.MethodPut:
		var payload nodePayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "请求格式错误"})
			return
		}
		node, err := s.nodeMgr.UpdateNode(r.Context(), nodeName, payload.toConfig())
		if err != nil {
			s.respondNodeError(w, err)
			return
		}
		writeJSON(w, map[string]any{"node": node, "message": "节点已更新，请点击重载使配置生效"})
	case http.MethodDelete:
		if err := s.nodeMgr.DeleteNode(r.Context(), nodeName); err != nil {
			s.respondNodeError(w, err)
			return
		}
		writeJSON(w, map[string]any{"message": "节点已删除，请点击重载使配置生效"})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleReload triggers a configuration reload.
func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.ensureNodeManager(w) {
		return
	}

	if err := s.nodeMgr.TriggerReload(r.Context()); err != nil {
		s.respondNodeError(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"message": "重载成功，现有连接已被中断",
	})
}

func (s *Server) ensureNodeManager(w http.ResponseWriter) bool {
	if s.nodeMgr == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]any{"error": "节点管理未启用"})
		return false
	}
	return true
}

func (s *Server) respondNodeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrNodeNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrNodeConflict), errors.Is(err, ErrInvalidNode):
		status = http.StatusBadRequest
	}
	w.WriteHeader(status)
	writeJSON(w, map[string]any{"error": err.Error()})
}
