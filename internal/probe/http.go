package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// maxBody is how much of a response body HTTP-01 reads (SPEC §4.3).
const maxBody = 4 << 10

// DialFunc opens a connection; it is the hook tests use to redirect a
// host name to a local server.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// NewHTTPClient returns the client of HTTP-01 (SPEC §4.3, "no proxy"): its
// own Transport with Proxy nil — never http.DefaultTransport, which reads
// HTTP_PROXY/HTTPS_PROXY — and DisableKeepAlives, so every run opens a
// fresh connection and the result reflects the network now. Redirects are
// not followed (the 3xx response is the result) and the whole exchange is
// bounded by timeout. dial may be nil (a plain net.Dialer).
func NewHTTPClient(timeout time.Duration, dial DialFunc) *http.Client {
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:             nil,
			DisableKeepAlives: true,
			DialContext:       dial,
			// HTTP/2 over a fresh connection gains nothing for one GET
			ForceAttemptHTTP2:     false,
			TLSHandshakeTimeout:   timeout,
			ResponseHeaderTimeout: timeout,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// HTTPProbe is HTTP-01: GET a URL without following redirects and compare
// status and body. A redirect, a wrong status or a wrong body is a captive
// result (class captive); a network error is a plain failure.
type HTTPProbe struct {
	URL          string
	ExpectStatus int
	// ExpectBody, if not empty, must equal the body (up to 4 KiB, trimmed).
	// Empty means the body is not compared: a body check against a 200 URL
	// would flag every real page, and a 204 has no body by definition.
	ExpectBody string
	Timeout    time.Duration
	Dial       DialFunc // nil: net.Dialer
}

func (p HTTPProbe) Run(ctx context.Context) Sample {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return Sample{Result: Fail, ErrClass: ClassLocal, Detail: err.Error()}
	}
	req.Header.Set("User-Agent", "netdiag")
	elapsed := stopwatch()
	resp, err := NewHTTPClient(p.Timeout, p.Dial).Do(req)
	if err != nil {
		class, detail := ClassifyNetError(err)
		return Sample{Result: Fail, ErrClass: class, Detail: detail}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	rtt := elapsed()
	if err != nil {
		class, detail := ClassifyNetError(err)
		return Sample{Result: Fail, ErrClass: class, Detail: detail}
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return Sample{Result: Fail, ErrClass: ClassCaptive,
			Detail: fmt.Sprintf("redirect %d to %s", resp.StatusCode, resp.Header.Get("Location"))}
	}
	if resp.StatusCode != p.ExpectStatus {
		return Sample{Result: Fail, ErrClass: ClassCaptive,
			Detail: fmt.Sprintf("status %d, want %d", resp.StatusCode, p.ExpectStatus)}
	}
	if p.ExpectBody != "" && strings.TrimSpace(string(body)) != strings.TrimSpace(p.ExpectBody) {
		return Sample{Result: Fail, ErrClass: ClassCaptive, Detail: "body differs from the expected text"}
	}
	return Sample{Result: OK, RTT: rtt}
}

// TLSProbe is SVC-01: resolve a service name, open a fresh TCP connection
// to port 443 and time the TLS handshake (no proxy, no connection reuse).
// The handshake time is the RTT; the lookup time is in Detail.
type TLSProbe struct {
	Resolver Resolver
	Host     string
	Port     string // default "443"
	Timeout  time.Duration
	Dial     DialFunc       // nil: net.Dialer
	RootCAs  *x509.CertPool // nil: the system roots
}

func (p TLSProbe) Run(ctx context.Context) Sample {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	lookup := stopwatch()
	ips, err := p.Resolver.LookupIPv4(ctx, p.Host)
	if err == nil && len(ips) == 0 {
		err = noAddress(p.Host)
	}
	if err != nil {
		return resolveFailure(p.Host, err)
	}
	dnsTime := lookup()
	port := p.Port
	if port == "" {
		port = "443"
	}
	dial := p.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	// every resolved address is tried in order within the one timeout, like
	// TCPProbe: a dead first address of a multi-A name must not fail the check
	var last Sample
	for _, ip := range ips {
		last = p.handshake(ctx, dial, ip, port)
		if last.Result == OK || ctx.Err() != nil {
			break
		}
	}
	if last.Result == OK {
		last.Detail = fmt.Sprintf("DNS %.1f ms", float64(dnsTime)/float64(time.Millisecond))
	}
	return last
}

// handshake opens a connection to one address and times the TLS handshake.
func (p TLSProbe) handshake(ctx context.Context, dial DialFunc, ip netip.Addr, port string) Sample {
	conn, err := dial(ctx, "tcp4", net.JoinHostPort(ip.String(), port))
	if err != nil {
		class, detail := ClassifyNetError(err)
		return Sample{Result: Fail, ErrClass: class, Detail: detail, From: ip}
	}
	defer func() { _ = conn.Close() }()
	tc := tls.Client(conn, &tls.Config{ServerName: p.Host, RootCAs: p.RootCAs, MinVersion: tls.VersionTLS12})
	elapsed := stopwatch()
	if err := tc.HandshakeContext(ctx); err != nil {
		class, detail := ClassifyNetError(err)
		if class == ClassOther {
			class = ClassTLS
		}
		return Sample{Result: Fail, ErrClass: class, Detail: "TLS: " + detail, From: ip}
	}
	return Sample{Result: OK, RTT: elapsed(), From: ip}
}
