// Package knocknoc is a Traefik middleware that lets a request through
// only if the caller's IP is on the allowlist published by a Knocknoc
// server.
package knocknoc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const defaultPollInterval = 2 * time.Second

const minPollInterval = 1 * time.Second

const defaultRequestTimeout = 5 * time.Second

const defaultRejectStatusCode = http.StatusForbidden

const defaultRejectContentType = "text/html; charset=utf-8"

// IPStrategyConfig controls how the caller's IP is derived from the request.
type IPStrategyConfig struct {
	Depth       int      `json:"depth,omitempty"`
	ExcludedIPs []string `json:"excludedIPs,omitempty"`
}

// Config is the plugin configuration.
type Config struct {
	SourceURL          string            `json:"sourceURL,omitempty"`
	Username           string            `json:"username,omitempty"`
	Secret             string            `json:"secret,omitempty"`
	PollInterval       string            `json:"pollInterval,omitempty"`
	RequestTimeout     string            `json:"requestTimeout,omitempty"`
	RejectStatusCode   int               `json:"rejectStatusCode,omitempty"`
	RejectBody         string            `json:"rejectBody,omitempty"`
	RejectContentType  string            `json:"rejectContentType,omitempty"`
	InsecureSkipVerify bool              `json:"insecureSkipVerify,omitempty"`
	IPStrategy         *IPStrategyConfig `json:"ipStrategy,omitempty"`
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	return &Config{
		PollInterval:       defaultPollInterval.String(),
		RequestTimeout:     defaultRequestTimeout.String(),
		RejectStatusCode:   defaultRejectStatusCode,
		InsecureSkipVerify: false,
	}
}

type allowlistState struct {
	nets        []*net.IPNet
	lastAttempt time.Time
	loadedOnce  bool
	lastErr     error
}

// Knocknoc is the middleware instance.
type Knocknoc struct {
	next http.Handler
	name string

	sourceURL string
	username  string
	secret    string

	pollInterval      time.Duration
	rejectStatusCode  int
	rejectBody        []byte
	rejectContentType string
	ipStrategy        *IPStrategyConfig

	excludedNets []*net.IPNet

	httpClient *http.Client

	state atomic.Value

	refreshMu sync.Mutex
}

// New creates a new Knocknoc middleware instance.
//
//nolint:revive // ctx is part of the constructor signature Traefik requires.
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if config == nil {
		return nil, errors.New("knocknoc: config is required")
	}

	sourceURL, parseErr := parseSource(config)
	if parseErr != nil {
		return nil, parseErr
	}

	pollInterval, parseErr := parseInterval("pollInterval", config.PollInterval, defaultPollInterval)
	if parseErr != nil {
		return nil, parseErr
	}
	if pollInterval < minPollInterval {
		return nil, fmt.Errorf("knocknoc: pollInterval %s is below the minimum of %s", pollInterval, minPollInterval)
	}

	requestTimeout, parseErr := parseInterval("requestTimeout", config.RequestTimeout, defaultRequestTimeout)
	if parseErr != nil {
		return nil, parseErr
	}

	rejectStatusCode := config.RejectStatusCode
	if rejectStatusCode == 0 {
		rejectStatusCode = defaultRejectStatusCode
	}
	if rejectStatusCode < 400 || rejectStatusCode > 599 {
		return nil, fmt.Errorf("knocknoc: rejectStatusCode %d is not a valid 4xx/5xx status", rejectStatusCode)
	}

	rejectContentType := strings.TrimSpace(config.RejectContentType)
	if config.RejectBody == "" && rejectContentType != "" {
		return nil, errors.New("knocknoc: rejectContentType is set but rejectBody is empty")
	}
	if config.RejectBody != "" && rejectContentType == "" {
		rejectContentType = defaultRejectContentType
	}

	excludedNets, parseErr := validateIPStrategy(config.IPStrategy)
	if parseErr != nil {
		return nil, parseErr
	}

	transport := &http.Transport{}
	if config.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in via config
	}

	k := &Knocknoc{
		next:              next,
		name:              name,
		sourceURL:         sourceURL,
		username:          config.Username,
		secret:            config.Secret,
		pollInterval:      pollInterval,
		rejectStatusCode:  rejectStatusCode,
		rejectBody:        []byte(config.RejectBody),
		rejectContentType: rejectContentType,
		ipStrategy:        config.IPStrategy,
		excludedNets:      excludedNets,
		httpClient: &http.Client{
			Timeout:   requestTimeout,
			Transport: transport,
		},
	}
	k.state.Store(&allowlistState{})

	k.refresh()

	return k, nil
}

// ServeHTTP implements http.Handler.
func (k *Knocknoc) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	k.ensureFresh()

	st, _ := k.state.Load().(*allowlistState)
	if st == nil || !st.loadedOnce || len(st.nets) == 0 {
		k.reject(rw)
		return
	}

	ip, err := k.clientIP(req)
	if err != nil {
		log.Printf("knocknoc(%s): could not determine client IP: %v", k.name, err)
		k.reject(rw)
		return
	}

	for _, n := range st.nets {
		if n.Contains(ip) {
			k.next.ServeHTTP(rw, req)
			return
		}
	}

	k.reject(rw)
}

func (k *Knocknoc) reject(rw http.ResponseWriter) {
	if len(k.rejectBody) == 0 {
		http.Error(rw, "Forbidden", k.rejectStatusCode)
		return
	}

	header := rw.Header()
	header.Set("Content-Type", k.rejectContentType)
	header.Set("Content-Length", strconv.Itoa(len(k.rejectBody)))
	header.Set("X-Content-Type-Options", "nosniff")
	rw.WriteHeader(k.rejectStatusCode)
	_, _ = rw.Write(k.rejectBody)
}

func (k *Knocknoc) ensureFresh() {
	st, _ := k.state.Load().(*allowlistState)
	if st != nil && time.Since(st.lastAttempt) < k.pollInterval {
		return
	}

	k.refreshMu.Lock()
	defer k.refreshMu.Unlock()

	st, _ = k.state.Load().(*allowlistState)
	if st != nil && time.Since(st.lastAttempt) < k.pollInterval {
		return
	}

	k.refresh()
}

func (k *Knocknoc) refresh() {
	prev, _ := k.state.Load().(*allowlistState)

	nets, err := k.fetchAndParse()

	next := &allowlistState{
		lastAttempt: time.Now(),
	}

	if err != nil {
		log.Printf("knocknoc(%s): refresh failed, keeping previous list: %v", k.name, err)
		if prev != nil {
			next.nets = prev.nets
			next.loadedOnce = prev.loadedOnce
		}
		next.lastErr = err
		k.state.Store(next)
		return
	}

	next.nets = nets
	next.loadedOnce = true
	k.state.Store(next)
	log.Printf("knocknoc(%s): loaded %d entries from %s", k.name, len(nets), k.sourceURL)
}

func (k *Knocknoc) fetchAndParse() ([]*net.IPNet, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, k.sourceURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", redactURLErr(err))
	}

	if k.username != "" {
		req.SetBasicAuth(k.username, k.secret)
	}

	resp, err := k.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", k.sourceURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("unexpected status %d from %s: %s", resp.StatusCode, k.sourceURL, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	nets, invalid := parseAllowlist(body)
	if len(invalid) > 0 {
		log.Printf("knocknoc(%s): ignoring %d invalid line(s) from the Knocknoc allowlist, e.g. %q", k.name, len(invalid), invalid[0])
	}

	return nets, nil
}

func parseInterval(name, value string, def time.Duration) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return def, nil
	}

	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("knocknoc: invalid %s %q: %w", name, value, err)
	}

	return d, nil
}

func validateIPStrategy(strategy *IPStrategyConfig) ([]*net.IPNet, error) {
	if strategy == nil {
		return nil, nil
	}

	if strategy.Depth < 0 {
		return nil, errors.New("knocknoc: ipStrategy.depth must be >= 0")
	}

	nets := make([]*net.IPNet, 0, len(strategy.ExcludedIPs))
	for _, entry := range strategy.ExcludedIPs {
		if strings.TrimSpace(entry) == "" {
			continue
		}

		n, err := parseNetwork(entry)
		if err != nil {
			return nil, fmt.Errorf("knocknoc: ipStrategy.excludedIPs: %w", err)
		}
		nets = append(nets, n)
	}

	return nets, nil
}

func parseSource(config *Config) (string, error) {
	sourceURL := strings.TrimSpace(config.SourceURL)
	if sourceURL == "" {
		return "", errors.New("knocknoc: sourceURL is required")
	}

	parsed, err := url.Parse(sourceURL)
	if err != nil {
		return "", fmt.Errorf("knocknoc: sourceURL is not a valid URL: %w", redactURLErr(err))
	}
	if parsed.User != nil {
		return "", errors.New("knocknoc: sourceURL must not embed credentials in its userinfo; set username and secret instead")
	}

	if config.Username == "" && config.Secret != "" {
		return "", errors.New("knocknoc: secret is set but username is empty")
	}

	return sourceURL, nil
}

func redactURLErr(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

func parseAllowlist(data []byte) (nets []*net.IPNet, invalid []string) {
	scanner := bufio.NewScanner(bytes.NewReader(data))

	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		n, err := parseNetwork(line)
		if err != nil {
			invalid = append(invalid, line)
			continue
		}
		nets = append(nets, n)
	}

	return nets, invalid
}

func parseNetwork(entry string) (*net.IPNet, error) {
	addr, bits, hasBits := strings.Cut(strings.TrimSpace(entry), "/")
	addr = unbracket(addr)
	if i := strings.IndexByte(addr, '%'); i >= 0 {
		addr = addr[:i]
	}

	if hasBits {
		_, ipNet, err := net.ParseCIDR(addr + "/" + bits)
		if err != nil {
			return nil, fmt.Errorf("%q is not a valid CIDR block", entry)
		}
		return ipNet, nil
	}

	ip := net.ParseIP(addr)
	if ip == nil {
		return nil, fmt.Errorf("%q is not a valid IP address or CIDR block", entry)
	}
	if v4 := ip.To4(); v4 != nil {
		return &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)}, nil
	}

	return &net.IPNet{IP: ip.To16(), Mask: net.CIDRMask(128, 128)}, nil
}

func parseIPToken(token string) net.IP {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}

	host, _, err := net.SplitHostPort(token)
	if err == nil {
		token = host
	} else {
		token = unbracket(token)
	}

	if i := strings.IndexByte(token, '%'); i >= 0 {
		token = token[:i]
	}

	return net.ParseIP(token)
}

func unbracket(s string) string {
	if len(s) > 1 && s[0] == '[' && s[len(s)-1] == ']' {
		return s[1 : len(s)-1]
	}
	return s
}

func (k *Knocknoc) clientIP(req *http.Request) (net.IP, error) {
	if k.ipStrategy == nil || k.ipStrategy.Depth <= 0 {
		ip := parseIPToken(req.RemoteAddr)
		if ip == nil {
			return nil, fmt.Errorf("unable to parse remote address %q", req.RemoteAddr)
		}
		return ip, nil
	}

	chain := make([]string, 0, 8)
	if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
		for _, part := range strings.Split(xff, ",") {
			part = strings.TrimSpace(part)
			if part == "" || k.isExcluded(parseIPToken(part)) {
				continue
			}
			chain = append(chain, part)
		}
	}

	depth := k.ipStrategy.Depth
	if depth > len(chain) {
		return nil, fmt.Errorf("ipStrategy.depth %d exceeds the X-Forwarded-For chain length %d (X-Forwarded-For=%q)", depth, len(chain), req.Header.Get("X-Forwarded-For"))
	}

	candidate := chain[len(chain)-depth]
	ip := parseIPToken(candidate)
	if ip == nil {
		return nil, fmt.Errorf("could not parse %q as an IP (from X-Forwarded-For chain)", candidate)
	}
	return ip, nil
}

func (k *Knocknoc) isExcluded(ip net.IP) bool {
	if ip == nil {
		return false
	}

	for _, excluded := range k.excludedNets {
		if excluded.Contains(ip) {
			return true
		}
	}
	return false
}
