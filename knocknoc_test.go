package knocknoc

import (
	"bytes"
	"context"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func nextOK() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("ok"))
	})
}

func TestParseAllowlist(t *testing.T) {
	data := []byte(strings.Join([]string{
		"# a comment",
		"",
		"10.0.0.0/8",
		"192.168.1.5",
		"2001:db8::1",
		"2001:db8::/32",
		"   ",
		"not-an-ip",
		"999.999.999.999",
	}, "\n"))

	nets, invalid := parseAllowlist(data)

	if len(nets) != 4 {
		t.Fatalf("expected 4 parsed entries, got %d: %v", len(nets), nets)
	}
	if len(invalid) != 2 {
		t.Fatalf("expected 2 invalid entries, got %d: %v", len(invalid), invalid)
	}

	if !nets[1].Contains(net.ParseIP("192.168.1.5")) {
		t.Errorf("expected 192.168.1.5/32 to contain 192.168.1.5")
	}
	if nets[1].Contains(net.ParseIP("192.168.1.6")) {
		t.Errorf("did not expect 192.168.1.5/32 to contain 192.168.1.6")
	}
	if !nets[2].Contains(net.ParseIP("2001:db8::1")) {
		t.Errorf("expected 2001:db8::1/128 to contain itself")
	}
}

func newTestServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNewRejectsMissingSourceURL(t *testing.T) {
	cfg := CreateConfig()
	_, err := New(context.Background(), nextOK(), cfg, "test")
	if err == nil {
		t.Fatal("expected an error for missing SourceURL, got nil")
	}
}

func TestNewRejectsTooSmallPollInterval(t *testing.T) {
	cfg := CreateConfig()
	cfg.SourceURL = "http://example.invalid/list.txt"
	cfg.PollInterval = "500ms"
	_, err := New(context.Background(), nextOK(), cfg, "test")
	if err == nil {
		t.Fatal("expected an error for a too-small pollInterval, got nil")
	}
}

func TestServeHTTPAllowsAndBlocks(t *testing.T) {
	srv := newTestServer(t, "203.0.113.0/24\n")

	cfg := CreateConfig()
	cfg.SourceURL = srv.URL
	cfg.PollInterval = "15s"

	h, err := New(context.Background(), nextOK(), cfg, "test")
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}

	allowed := httptest.NewRequest(http.MethodGet, "/", nil)
	allowed.RemoteAddr = "203.0.113.7:12345"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, allowed)
	if rr.Code != http.StatusOK {
		t.Errorf("expected allowed IP to get 200, got %d", rr.Code)
	}

	blocked := httptest.NewRequest(http.MethodGet, "/", nil)
	blocked.RemoteAddr = "198.51.100.7:12345"
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, blocked)
	if rr2.Code != http.StatusForbidden {
		t.Errorf("expected non-allowed IP to get 403, got %d", rr2.Code)
	}
}

func TestServeHTTPFailsClosedWhenSourceNeverReachable(t *testing.T) {
	cfg := CreateConfig()
	cfg.SourceURL = "http://127.0.0.1:1/unreachable"
	cfg.PollInterval = "15s"
	cfg.RequestTimeout = "200ms"

	h, err := New(context.Background(), nextOK(), cfg, "test")
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.7:12345"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("expected fail-closed 403 when the source has never loaded, got %d", rr.Code)
	}
}

func TestServeHTTPKeepsLastGoodListWhenRefreshFails(t *testing.T) {
	var body atomic.Value
	body.Store("203.0.113.0/24\n")

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		b := body.Load().(string)
		if b == "FAIL" {
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(b))
	}))
	t.Cleanup(srv.Close)

	cfg := CreateConfig()
	cfg.SourceURL = srv.URL
	cfg.PollInterval = "1s"

	handler, err := New(context.Background(), nextOK(), cfg, "test")
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}
	k := handler.(*Knocknoc)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.7:12345"
	rr := httptest.NewRecorder()
	k.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on first good fetch, got %d", rr.Code)
	}

	body.Store("FAIL")
	st := k.state.Load().(*allowlistState)
	backdated := *st
	backdated.lastAttempt = time.Now().Add(-time.Hour)
	k.state.Store(&backdated)

	rr2 := httptest.NewRecorder()
	k.ServeHTTP(rr2, req)
	if rr2.Code != http.StatusOK {
		t.Errorf("expected the last known-good list to still allow the request, got %d", rr2.Code)
	}

	st2 := k.state.Load().(*allowlistState)
	if !st2.loadedOnce || len(st2.nets) == 0 {
		t.Errorf("expected the previous good list to be retained after a failed refresh")
	}
	if st2.lastErr == nil {
		t.Errorf("expected lastErr to be recorded after a failed refresh")
	}
}

func TestServeHTTPBlocksUntilRefreshCompletesAndAppliesItImmediately(t *testing.T) {
	const fetchDelay = 150 * time.Millisecond
	var includeNewIP atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		time.Sleep(fetchDelay)
		rw.WriteHeader(http.StatusOK)
		if includeNewIP.Load() {
			_, _ = rw.Write([]byte("203.0.113.0/24\n198.51.100.9/32\n"))
		} else {
			_, _ = rw.Write([]byte("203.0.113.0/24\n"))
		}
	}))
	t.Cleanup(srv.Close)

	cfg := CreateConfig()
	cfg.SourceURL = srv.URL
	cfg.PollInterval = "1s"

	handler, err := New(context.Background(), nextOK(), cfg, "test")
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}
	k := handler.(*Knocknoc)

	newIPReq := httptest.NewRequest(http.MethodGet, "/", nil)
	newIPReq.RemoteAddr = "198.51.100.9:1234"

	rr := httptest.NewRecorder()
	k.ServeHTTP(rr, newIPReq)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected the not-yet-allowed IP to be rejected before the source is updated, got %d", rr.Code)
	}

	includeNewIP.Store(true)
	st := k.state.Load().(*allowlistState)
	backdated := *st
	backdated.lastAttempt = time.Now().Add(-time.Hour)
	k.state.Store(&backdated)

	start := time.Now()
	rr2 := httptest.NewRecorder()
	k.ServeHTTP(rr2, newIPReq)
	elapsed := time.Since(start)

	if elapsed < fetchDelay {
		t.Errorf("expected ServeHTTP to block for the refresh (>= %s), took %s", fetchDelay, elapsed)
	}
	if rr2.Code != http.StatusOK {
		t.Errorf("expected the newly-allowed IP to be let through on the very same request that triggered the refresh, got %d", rr2.Code)
	}
}

func TestEnsureFreshSerializesConcurrentRefreshes(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(50 * time.Millisecond)
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("203.0.113.0/24\n"))
	}))
	t.Cleanup(srv.Close)

	cfg := CreateConfig()
	cfg.SourceURL = srv.URL
	cfg.PollInterval = "1s"

	handler, err := New(context.Background(), nextOK(), cfg, "test")
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}
	k := handler.(*Knocknoc)

	st := k.state.Load().(*allowlistState)
	backdated := *st
	backdated.lastAttempt = time.Now().Add(-time.Hour)
	k.state.Store(&backdated)

	before := atomic.LoadInt32(&calls)

	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "203.0.113.7:1234"
			rr := httptest.NewRecorder()
			k.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Errorf("expected an allowed request to succeed, got %d", rr.Code)
			}
		}()
	}
	wg.Wait()

	got := atomic.LoadInt32(&calls) - before
	if got != 1 {
		t.Errorf("expected exactly 1 additional fetch across %d concurrent due requests, got %d", n, got)
	}
}

func TestClientIPWithForwardedForStrategy(t *testing.T) {
	cfg := CreateConfig()
	cfg.SourceURL = "http://example.invalid/list.txt"
	cfg.IPStrategy = &IPStrategyConfig{Depth: 1}

	k := &Knocknoc{ipStrategy: cfg.IPStrategy}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:5678"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.5")

	ip, err := k.clientIP(req)
	if err != nil {
		t.Fatalf("clientIP returned an error: %v", err)
	}

	if ip.String() != "10.0.0.5" {
		t.Errorf("expected 10.0.0.5, got %s", ip.String())
	}
}

func TestClientIPDepthCountsWithinForwardedForOnly(t *testing.T) {
	tests := []struct {
		name    string
		depth   int
		want    string
		wantErr bool
	}{
		{name: "depth 1 is the last entry", depth: 1, want: "10.0.0.5"},
		{name: "depth 2 is the entry before it", depth: 2, want: "203.0.113.7"},
		{name: "depth beyond the chain is an error", depth: 3, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k := &Knocknoc{ipStrategy: &IPStrategyConfig{Depth: tc.depth}}

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "10.0.0.1:5678"
			req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.5")

			ip, err := k.clientIP(req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %s", ip)
				}
				return
			}
			if err != nil {
				t.Fatalf("clientIP returned an error: %v", err)
			}
			if ip.String() != tc.want {
				t.Errorf("expected %s, got %s", tc.want, ip.String())
			}
		})
	}
}

func TestClientIPDepthWithoutForwardedFor(t *testing.T) {
	k := &Knocknoc{ipStrategy: &IPStrategyConfig{Depth: 1}}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:5678"

	if ip, err := k.clientIP(req); err == nil {
		t.Fatalf("expected an error when X-Forwarded-For is absent, got %s", ip)
	}
}

func TestClientIPDefaultsToRemoteAddr(t *testing.T) {
	k := &Knocknoc{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.9:5678"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")

	ip, err := k.clientIP(req)
	if err != nil {
		t.Fatalf("clientIP returned an error: %v", err)
	}
	if ip.String() != "203.0.113.9" {
		t.Errorf("expected 203.0.113.9, got %s", ip.String())
	}
}

const (
	testUsername = "apiuser"
	testSecret   = "sup3rs3cr3t"
)

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	flags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(flags)
	})
	return &buf
}

func withCredentials(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing %q: %v", rawURL, err)
	}
	u.User = url.UserPassword(testUsername, testSecret)
	return u.String()
}

func TestNewRejectsCredentialsInSourceURL(t *testing.T) {
	cfg := CreateConfig()
	cfg.SourceURL = withCredentials(t, "http://example.invalid/list.txt")

	_, err := New(context.Background(), nextOK(), cfg, "test")
	if err == nil {
		t.Fatal("expected an error for credentials embedded in sourceURL, got nil")
	}

	if strings.Contains(err.Error(), testSecret) {
		t.Errorf("the secret leaked into the config error: %v", err)
	}
}

func TestNewRejectsInvalidSourceURL(t *testing.T) {
	cfg := CreateConfig()
	cfg.SourceURL = "http://apiuser:" + testSecret + "@exa mple.com/list.txt"

	_, err := New(context.Background(), nextOK(), cfg, "test")
	if err == nil {
		t.Fatal("expected an error for a malformed sourceURL, got nil")
	}

	if strings.Contains(err.Error(), testSecret) {
		t.Errorf("the secret leaked into the config error: %v", err)
	}
}

func TestNewRejectsSecretWithoutUsername(t *testing.T) {
	cfg := CreateConfig()
	cfg.SourceURL = "http://example.invalid/list.txt"
	cfg.Secret = testSecret

	_, err := New(context.Background(), nextOK(), cfg, "test")
	if err == nil {
		t.Fatal("expected an error for a secret with no username, got nil")
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Errorf("the secret leaked into the config error: %v", err)
	}
}

func TestFetchSendsBasicAuth(t *testing.T) {
	var authed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		user, secret, ok := req.BasicAuth()
		switch {
		case !ok:
			t.Errorf("expected the fetch to carry basic auth, got Authorization %q", req.Header.Get("Authorization"))
		case user != testUsername || secret != testSecret:
			t.Errorf("expected credentials %q/%q, got %q/%q", testUsername, testSecret, user, secret)
		default:
			authed.Add(1)
		}
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("203.0.113.0/24\n"))
	}))
	t.Cleanup(srv.Close)

	cfg := CreateConfig()
	cfg.SourceURL = srv.URL
	cfg.Username = testUsername
	cfg.Secret = testSecret
	cfg.PollInterval = "15s"

	_, err := New(context.Background(), nextOK(), cfg, "test")
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}

	if authed.Load() != 1 {
		t.Errorf("expected exactly 1 authenticated fetch, got %d", authed.Load())
	}
}

func TestFetchOmitsAuthorizationWithoutCredentials(t *testing.T) {
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if got := req.Header.Get("Authorization"); got != "" {
			t.Errorf("expected no Authorization header when no credentials are configured, got %q", got)
		}
		fetches.Add(1)
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("203.0.113.0/24\n"))
	}))
	t.Cleanup(srv.Close)

	cfg := CreateConfig()
	cfg.SourceURL = srv.URL
	cfg.PollInterval = "15s"

	_, err := New(context.Background(), nextOK(), cfg, "test")
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}

	if fetches.Load() != 1 {
		t.Errorf("expected exactly 1 fetch, got %d", fetches.Load())
	}
}

func TestSecretIsNotLoggedOnSuccess(t *testing.T) {
	srv := newTestServer(t, "203.0.113.0/24\n")
	logs := captureLogs(t)

	cfg := CreateConfig()
	cfg.SourceURL = srv.URL
	cfg.Username = testUsername
	cfg.Secret = testSecret
	cfg.PollInterval = "15s"

	handler, err := New(context.Background(), nextOK(), cfg, "test")
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}
	k := handler.(*Knocknoc)

	if strings.Contains(k.sourceURL, testSecret) {
		t.Errorf("expected sourceURL to be free of credentials, got %q", k.sourceURL)
	}

	if !strings.Contains(logs.String(), "loaded 1 entries") {
		t.Fatalf("expected a successful-load log line, got %q", logs.String())
	}
	if strings.Contains(logs.String(), testSecret) {
		t.Errorf("the secret leaked into the logs: %q", logs.String())
	}
}

func TestSecretIsNotInUnreachableSourceError(t *testing.T) {
	logs := captureLogs(t)

	cfg := CreateConfig()
	cfg.SourceURL = "http://127.0.0.1:1/unreachable"
	cfg.Username = testUsername
	cfg.Secret = testSecret
	cfg.PollInterval = "15s"
	cfg.RequestTimeout = "200ms"

	handler, err := New(context.Background(), nextOK(), cfg, "test")
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}
	k := handler.(*Knocknoc)

	st := k.state.Load().(*allowlistState)
	if st.lastErr == nil {
		t.Fatal("expected the initial fetch against an unreachable source to fail")
	}
	if strings.Contains(st.lastErr.Error(), testSecret) {
		t.Errorf("the secret leaked into the fetch error: %v", st.lastErr)
	}
	if strings.Contains(logs.String(), testSecret) {
		t.Errorf("the secret leaked into the logs: %q", logs.String())
	}
}

func TestSecretIsNotInBadStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusUnauthorized)
		_, _ = rw.Write([]byte("unauthorized"))
	}))
	t.Cleanup(srv.Close)

	logs := captureLogs(t)

	cfg := CreateConfig()
	cfg.SourceURL = srv.URL
	cfg.Username = testUsername
	cfg.Secret = testSecret
	cfg.PollInterval = "15s"

	handler, err := New(context.Background(), nextOK(), cfg, "test")
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}
	k := handler.(*Knocknoc)

	st := k.state.Load().(*allowlistState)
	if st.lastErr == nil {
		t.Fatal("expected a non-2xx response to be recorded as an error")
	}
	if strings.Contains(st.lastErr.Error(), testSecret) {
		t.Errorf("the secret leaked into the status error: %v", st.lastErr)
	}
	if strings.Contains(logs.String(), testSecret) {
		t.Errorf("the secret leaked into the logs: %q", logs.String())
	}
}

func TestParseAllowlistIPv6Forms(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		network string
		in      string
		out     string
	}{
		{"bare address", "2001:db8::1", "2001:db8::1/128", "2001:db8::1", "2001:db8::2"},
		{"uppercase", "2001:DB8::1", "2001:db8::1/128", "2001:db8:0:0:0:0:0:1", "2001:db8::2"},
		{"bracketed", "[2001:db8::1]", "2001:db8::1/128", "2001:db8::1", "2001:db8::2"},
		{"zone is dropped", "fe80::1%eth0", "fe80::1/128", "fe80::1", "fe80::2"},
		{"cidr", "2001:db8::/32", "2001:db8::/32", "2001:db8:dead::1", "2001:db9::1"},
		{"bracketed cidr", "[2001:db8::]/32", "2001:db8::/32", "2001:db8:dead::1", "2001:db9::1"},

		{"ipv4-mapped", "::ffff:198.51.100.7", "198.51.100.7/32", "198.51.100.7", "::1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nets, invalid := parseAllowlist([]byte(tc.line + "\n"))
			if len(invalid) != 0 {
				t.Fatalf("expected %q to parse, got invalid=%v", tc.line, invalid)
			}
			if len(nets) != 1 {
				t.Fatalf("expected 1 network from %q, got %d", tc.line, len(nets))
			}
			if got := nets[0].String(); got != tc.network {
				t.Fatalf("expected %q to parse as %s, got %s", tc.line, tc.network, got)
			}
			if !nets[0].Contains(net.ParseIP(tc.in)) {
				t.Errorf("expected %s to contain %s", tc.network, tc.in)
			}
			if nets[0].Contains(net.ParseIP(tc.out)) {
				t.Errorf("did not expect %s to contain %s", tc.network, tc.out)
			}
		})
	}
}

func TestParseAllowlistRejectsMalformedIPv6(t *testing.T) {
	data := []byte(strings.Join([]string{
		"2001:db8::gg",
		"2001:db8::/999",
		"[2001:db8::1",
	}, "\n"))

	nets, invalid := parseAllowlist(data)
	if len(nets) != 0 {
		t.Fatalf("expected no networks, got %v", nets)
	}
	if len(invalid) != 3 {
		t.Fatalf("expected 3 invalid lines, got %d: %v", len(invalid), invalid)
	}
}

func TestClientIPIPv6RemoteAddrForms(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		want       string
	}{
		{"bracketed with port", "[2001:db8::1]:443", "2001:db8::1"},
		{"bracketed without port", "[2001:db8::1]", "2001:db8::1"},
		{"bare", "2001:db8::1", "2001:db8::1"},
		{"zone", "[fe80::1%eth0]:443", "fe80::1"},
		{"ipv4-mapped", "[::ffff:203.0.113.9]:443", "203.0.113.9"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k := &Knocknoc{}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remoteAddr

			ip, err := k.clientIP(req)
			if err != nil {
				t.Fatalf("clientIP(%q) returned an error: %v", tc.remoteAddr, err)
			}
			if ip.String() != tc.want {
				t.Errorf("expected %s, got %s", tc.want, ip.String())
			}
		})
	}
}

func TestClientIPIPv6ForwardedForStrategy(t *testing.T) {
	k := &Knocknoc{ipStrategy: &IPStrategyConfig{Depth: 1}}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "[2001:db8:0:1::1]:5678"
	req.Header.Set("X-Forwarded-For", "[2001:db8::7]:1234, 2001:db8:0:1::5")

	ip, err := k.clientIP(req)
	if err != nil {
		t.Fatalf("clientIP returned an error: %v", err)
	}

	if ip.String() != "2001:db8:0:1::5" {
		t.Errorf("expected 2001:db8:0:1::5, got %s", ip.String())
	}
}

func TestExcludedIPsMatchByAddressNotText(t *testing.T) {
	tests := []struct {
		name     string
		excluded []string
	}{
		{"different case", []string{"2001:DB8::A"}},
		{"expanded form", []string{"2001:db8:0:0:0:0:0:a"}},
		{"cidr", []string{"2001:db8::/64"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := CreateConfig()
			cfg.SourceURL = "http://example.invalid/list.txt"
			cfg.IPStrategy = &IPStrategyConfig{Depth: 1, ExcludedIPs: tc.excluded}

			handler, err := New(context.Background(), nextOK(), cfg, "test")
			if err != nil {
				t.Fatalf("New returned an error: %v", err)
			}
			k := handler.(*Knocknoc)

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "[2001:db8:0:1::1]:5678"
			req.Header.Set("X-Forwarded-For", "2001:db8:0:1::7, [2001:db8::a]:443")

			ip, err := k.clientIP(req)
			if err != nil {
				t.Fatalf("clientIP returned an error: %v", err)
			}

			if ip.String() != "2001:db8:0:1::7" {
				t.Errorf("expected 2001:db8:0:1::7, got %s", ip.String())
			}
		})
	}
}

func TestNewRejectsInvalidExcludedIP(t *testing.T) {
	cfg := CreateConfig()
	cfg.SourceURL = "http://example.invalid/list.txt"
	cfg.IPStrategy = &IPStrategyConfig{Depth: 1, ExcludedIPs: []string{"2001:db8::zz"}}

	_, err := New(context.Background(), nextOK(), cfg, "test")
	if err == nil {
		t.Fatal("expected New to reject an unparseable excludedIPs entry")
	}
}

func TestServeHTTPIPv6Client(t *testing.T) {
	srv := newTestServer(t, "2001:db8::/64\n198.51.100.9\n")

	cfg := CreateConfig()
	cfg.SourceURL = srv.URL

	handler, err := New(context.Background(), nextOK(), cfg, "test")
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}

	tests := []struct {
		name       string
		remoteAddr string
		want       int
	}{
		{"ipv6 in range", "[2001:db8::5]:443", http.StatusOK},
		{"ipv6 out of range", "[2001:db9::5]:443", http.StatusForbidden},
		{"ipv4 still works", "198.51.100.9:443", http.StatusOK},
		{"ipv4-mapped ipv6", "[::ffff:198.51.100.9]:443", http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remoteAddr
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tc.want {
				t.Errorf("expected %d for %s, got %d", tc.want, tc.remoteAddr, rr.Code)
			}
		})
	}
}

func TestRejectDefaultsToPlainTextForbidden(t *testing.T) {
	srv := newTestServer(t, "203.0.113.7/32\n")

	cfg := CreateConfig()
	cfg.SourceURL = srv.URL
	cfg.PollInterval = "15s"

	h, err := New(context.Background(), nextOK(), cfg, "test")
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.7:12345"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("expected the default rejection to stay text/plain, got %q", got)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != "Forbidden" {
		t.Errorf("expected body %q, got %q", "Forbidden", got)
	}
}

func TestRejectServesConfiguredBody(t *testing.T) {
	srv := newTestServer(t, "203.0.113.7/32\n")

	const page = "<!doctype html>\n<html><body><h1>Knock first</h1></body></html>\n"

	cfg := CreateConfig()
	cfg.SourceURL = srv.URL
	cfg.PollInterval = "15s"
	cfg.RejectBody = page

	h, err := New(context.Background(), nextOK(), cfg, "test")
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.7:12345"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
	if rr.Body.String() != page {
		t.Errorf("expected the configured page to be served verbatim, got %q", rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != defaultRejectContentType {
		t.Errorf("expected Content-Type %q, got %q", defaultRejectContentType, got)
	}
	if got := rr.Header().Get("Content-Length"); got != strconv.Itoa(len(page)) {
		t.Errorf("expected Content-Length %d, got %q", len(page), got)
	}
	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("expected X-Content-Type-Options nosniff, got %q", got)
	}
}

func TestRejectBodyHonoursCustomStatusAndContentType(t *testing.T) {
	srv := newTestServer(t, "203.0.113.7/32\n")

	cfg := CreateConfig()
	cfg.SourceURL = srv.URL
	cfg.PollInterval = "15s"
	cfg.RejectBody = `{"error":"not authorized"}`
	cfg.RejectContentType = "application/json"
	cfg.RejectStatusCode = http.StatusUnauthorized

	h, err := New(context.Background(), nextOK(), cfg, "test")
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.7:12345"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", got)
	}
	if rr.Body.String() != cfg.RejectBody {
		t.Errorf("expected body %q, got %q", cfg.RejectBody, rr.Body.String())
	}
}

func TestRejectBodyServedWhenAllowlistNeverLoaded(t *testing.T) {
	cfg := CreateConfig()
	cfg.SourceURL = "http://127.0.0.1:1/unreachable"
	cfg.PollInterval = "15s"
	cfg.RequestTimeout = "200ms"
	cfg.RejectBody = "<p>knock first</p>"

	h, err := New(context.Background(), nextOK(), cfg, "test")
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.7:12345"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
	if rr.Body.String() != cfg.RejectBody {
		t.Errorf("expected the configured page on the fail-closed path, got %q", rr.Body.String())
	}
}

func TestNewRejectsContentTypeWithoutBody(t *testing.T) {
	cfg := CreateConfig()
	cfg.SourceURL = "http://example.invalid/list.txt"
	cfg.RejectContentType = "text/html"

	if _, err := New(context.Background(), nextOK(), cfg, "test"); err == nil {
		t.Fatal("expected New to reject a rejectContentType with no rejectBody")
	}
}
