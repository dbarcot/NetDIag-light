package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// dialTo returns a DialFunc that sends every connection to addr, recording
// the address the client asked for.
func dialTo(addr string, asked *[]string) DialFunc {
	return func(ctx context.Context, network, a string) (net.Conn, error) {
		*asked = append(*asked, a)
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
}

func newHTTPServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	var conns atomic.Int32
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &conns
}

func TestHTTPProbe_Classification(t *testing.T) {
	t.Parallel()
	srv, _ := newHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte("Microsoft Connect Test\n"))
		case "/page":
			_, _ = w.Write([]byte("<html>please log in</html>"))
		case "/204":
			w.WriteHeader(http.StatusNoContent)
		case "/redirect":
			http.Redirect(w, r, "http://login.portal.test/", http.StatusFound)
		case "/err":
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	})
	tests := []struct {
		name         string
		path         string
		status       int
		body         string
		wantResult   Result
		wantClass    string
		wantInDetail string
	}{
		{"right status and body", "/ok", 200, "Microsoft Connect Test", OK, "", ""},
		{"wrong body is captive", "/page", 200, "Microsoft Connect Test", Fail, ClassCaptive, "body"},
		{"redirect is captive and not followed", "/redirect", 200, "", Fail, ClassCaptive, "login.portal.test"},
		{"wrong status is captive", "/err", 200, "", Fail, ClassCaptive, "status 500"},
		{"204 without body", "/204", 204, "", OK, "", ""},
		{"200 where 204 is expected is captive", "/page", 204, "", Fail, ClassCaptive, "status 200"},
		{"body not compared when none is expected", "/page", 200, "", OK, "", ""},
	}
	for _, tt := range tests {
		var asked []string
		s := HTTPProbe{URL: "http://netdiag.test" + tt.path, ExpectStatus: tt.status, ExpectBody: tt.body,
			Timeout: 2 * time.Second, Dial: dialTo(srv.Listener.Addr().String(), &asked)}.Run(context.Background())
		if s.Result != tt.wantResult || s.ErrClass != tt.wantClass {
			t.Errorf("%s: %+v, want %s/%q", tt.name, s, tt.wantResult, tt.wantClass)
		}
		if tt.wantInDetail != "" && !contains(s.Detail, tt.wantInDetail) {
			t.Errorf("%s: detail %q lacks %q", tt.name, s.Detail, tt.wantInDetail)
		}
		if s.Result == OK && s.RTT <= 0 {
			t.Errorf("%s: no RTT", tt.name)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestHTTPProbe_NetworkErrorsAreNotCaptive(t *testing.T) {
	t.Parallel()
	// connection refused
	var asked []string
	s := HTTPProbe{URL: "http://netdiag.test/x", ExpectStatus: 200, Timeout: time.Second,
		Dial: dialTo(closedPort(t), &asked)}.Run(context.Background())
	if s.Result != Fail || s.ErrClass != ClassRefused {
		t.Errorf("refused: %+v", s)
	}
	// name that does not resolve (default dialer, reserved .invalid TLD)
	s = HTTPProbe{URL: "http://netdiag.invalid/x", ExpectStatus: 200, Timeout: 2 * time.Second}.Run(context.Background())
	if s.Result != Fail || s.ErrClass != ClassDNS {
		t.Errorf("unresolvable name: %+v, want fail/dns (never captive)", s)
	}
	// server that never answers
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hang }))
	defer srv.Close()
	defer close(hang)
	s = HTTPProbe{URL: srv.URL, ExpectStatus: 200, Timeout: 100 * time.Millisecond}.Run(context.Background())
	if s.Result != Fail || s.ErrClass != ClassTimeout {
		t.Errorf("timeout: %+v", s)
	}
}

// Every run opens a new connection (no keep-alive reuse): a result must
// reflect the network now, not a pooled connection from before an outage.
func TestHTTPProbe_FreshConnectionEveryRun(t *testing.T) {
	t.Parallel()
	srv, conns := newHTTPServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hi")) })
	var asked []string
	p := HTTPProbe{URL: "http://netdiag.test/", ExpectStatus: 200, Timeout: time.Second, Dial: dialTo(srv.Listener.Addr().String(), &asked)}
	for range 3 {
		if s := p.Run(context.Background()); s.Result != OK {
			t.Fatalf("%+v", s)
		}
	}
	if conns.Load() != 3 {
		t.Errorf("%d connections for 3 runs, want 3", conns.Load())
	}
}

func TestNewHTTPClient_Settings(t *testing.T) {
	t.Parallel()
	c := NewHTTPClient(3*time.Second, nil)
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr == http.DefaultTransport {
		t.Fatal("HTTP-01 must have its own Transport, not http.DefaultTransport")
	}
	if tr.Proxy != nil {
		t.Error("Transport.Proxy must be nil (no proxy handling)")
	}
	if !tr.DisableKeepAlives {
		t.Error("DisableKeepAlives must be set")
	}
	if c.Timeout != 3*time.Second {
		t.Errorf("client timeout %v", c.Timeout)
	}
	if err := c.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Errorf("CheckRedirect = %v, want http.ErrUseLastResponse", err)
	}
}

// HTTP_PROXY / HTTPS_PROXY in the environment are ignored: the connection
// goes to the target, never to the proxy (SPEC §4.3). The host name is not
// localhost, which ProxyFromEnvironment would skip anyway.
func TestHTTPProbe_IgnoresProxyEnvironment(t *testing.T) {
	proxy, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxy.Close() }()
	var proxied atomic.Int32
	go func() {
		for {
			c, err := proxy.Accept()
			if err != nil {
				return
			}
			proxied.Add(1)
			_ = c.Close()
		}
	}()
	t.Setenv("HTTP_PROXY", "http://"+proxy.Addr().String())
	t.Setenv("HTTPS_PROXY", "http://"+proxy.Addr().String())
	t.Setenv("http_proxy", "http://"+proxy.Addr().String())
	t.Setenv("https_proxy", "http://"+proxy.Addr().String())
	t.Setenv("NO_PROXY", "")

	srv, _ := newHTTPServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hi")) })
	var asked []string
	s := HTTPProbe{URL: "http://netdiag.test:8080/", ExpectStatus: 200, ExpectBody: "hi", Timeout: time.Second,
		Dial: dialTo(srv.Listener.Addr().String(), &asked)}.Run(context.Background())
	if s.Result != OK {
		t.Fatalf("%+v", s)
	}
	if len(asked) != 1 || asked[0] != "netdiag.test:8080" {
		t.Errorf("dialed %v, want only the target netdiag.test:8080 (a proxy would be dialed instead)", asked)
	}
	if proxied.Load() != 0 {
		t.Errorf("the proxy got %d connections", proxied.Load())
	}
	// and the sanity check that the environment would have been used by the default transport
	req, _ := http.NewRequest("GET", "http://netdiag.test:8080/", nil)
	if u, _ := http.ProxyFromEnvironment(req); u == nil {
		t.Log("note: ProxyFromEnvironment already cached an empty environment in this process")
	}
}

func TestTLSProbe(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	pool := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	res := &FakeResolver{}
	res.Set("example.com", netip.MustParseAddr("127.0.0.1")) // example.com is in httptest's certificate

	var asked []string
	p := TLSProbe{Resolver: res, Host: "example.com", Port: port, Timeout: 2 * time.Second, RootCAs: pool,
		Dial: dialTo(srv.Listener.Addr().String(), &asked)}
	s := p.Run(context.Background())
	if s.Result != OK || s.RTT <= 0 || s.From != netip.MustParseAddr("127.0.0.1") || !contains(s.Detail, "DNS") {
		t.Errorf("handshake: %+v", s)
	}

	p.Host = "other.test"
	res.Set("other.test", netip.MustParseAddr("127.0.0.1"))
	s = p.Run(context.Background())
	if s.Result != Fail || s.ErrClass != ClassTLS {
		t.Errorf("certificate for another name: %+v, want fail/tls", s)
	}

	p.Host = "nowhere.test"
	if s = p.Run(context.Background()); s.Result != Fail || s.ErrClass != ClassDNS {
		t.Errorf("unresolvable: %+v", s)
	}

	p.Host, p.Dial = "example.com", dialTo(closedPort(t), &asked)
	if s = p.Run(context.Background()); s.Result != Fail || s.ErrClass != ClassRefused {
		t.Errorf("refused: %+v", s)
	}
}

// A name with several A records is tried address by address within the one
// timeout: a dead first address must not fail the service (SVC-01).
func TestTLSProbe_TriesEveryResolvedAddress(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	pool := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	dead, alive := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("127.0.0.1")
	res := &FakeResolver{}
	res.Set("example.com", dead, alive)

	var asked []string
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		asked = append(asked, addr)
		if strings.HasPrefix(addr, "192.0.2.") { // TEST-NET: nothing there
			return nil, &net.OpError{Op: "dial", Err: wsaeHostUnreach}
		}
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	p := TLSProbe{Resolver: res, Host: "example.com", Port: port, Timeout: 2 * time.Second, RootCAs: pool, Dial: dial}
	s := p.Run(context.Background())
	if s.Result != OK || s.From != alive {
		t.Errorf("got %+v, want ok via the second address", s)
	}
	if len(asked) != 2 || !strings.HasPrefix(asked[0], dead.String()) || !strings.HasPrefix(asked[1], alive.String()) {
		t.Errorf("dialed %v, want the dead address first, then the live one", asked)
	}

	// every address fails: the last failure is reported
	res.Set("example.com", dead, netip.MustParseAddr("192.0.2.2"))
	asked = nil
	s = p.Run(context.Background())
	if s.Result != Fail || s.ErrClass != ClassUnreachable || s.From != netip.MustParseAddr("192.0.2.2") || len(asked) != 2 {
		t.Errorf("all dead: %+v after %v", s, asked)
	}

	// a good first address does not try the second
	res.Set("example.com", alive, dead)
	asked = nil
	if s = p.Run(context.Background()); s.Result != OK || len(asked) != 1 {
		t.Errorf("first address works: %+v after %v", s, asked)
	}

	// the whole list shares the one timeout
	res.Set("example.com", dead, alive)
	hang := func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }
	p.Dial, p.Timeout = hang, 80*time.Millisecond
	start := time.Now()
	if s = p.Run(context.Background()); s.Result != Fail || s.ErrClass != ClassTimeout || time.Since(start) > time.Second {
		t.Errorf("hanging dial: %+v after %v", s, time.Since(start))
	}
}
