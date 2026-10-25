package probe

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"

	"netdiag/internal/clock"
)

// Resolver resolves a host name to IPv4 addresses. IP literals are
// returned as they are.
type Resolver interface {
	LookupIPv4(ctx context.Context, host string) ([]netip.Addr, error)
}

// SystemResolver resolves through the system resolver (GetAddrInfoW, so
// with the DNS client cache). Probes other than DNS-01 use it (SPEC §5).
type SystemResolver struct{}

func (SystemResolver) LookupIPv4(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip}, nil
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
}

// DefaultResolveTTL is the re-resolution interval of custom targets
// (SPEC §4.2).
const DefaultResolveTTL = 60 * time.Second

// CachedResolver re-resolves a host once per TTL and returns the cached
// addresses in between. Failures are not cached.
type CachedResolver struct {
	Inner Resolver
	Clock clock.Clock
	TTL   time.Duration // 0 = DefaultResolveTTL

	mu sync.Mutex
	m  map[string]cacheEntry
}

type cacheEntry struct {
	ips     []netip.Addr
	expires time.Time
}

func (r *CachedResolver) LookupIPv4(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip}, nil
	}
	now := r.Clock.Now()
	r.mu.Lock()
	e, ok := r.m[host]
	r.mu.Unlock()
	if ok && now.Before(e.expires) {
		return append([]netip.Addr(nil), e.ips...), nil
	}
	ips, err := r.Inner.LookupIPv4(ctx, host)
	if err != nil {
		return nil, err
	}
	ttl := r.TTL
	if ttl <= 0 {
		ttl = DefaultResolveTTL
	}
	r.mu.Lock()
	if r.m == nil {
		r.m = map[string]cacheEntry{}
	}
	r.m[host] = cacheEntry{ips: ips, expires: now.Add(ttl)}
	r.mu.Unlock()
	return append([]netip.Addr(nil), ips...), nil
}

// FakeResolver answers from a map; an unknown host is a DNS error.
type FakeResolver struct {
	mu    sync.Mutex
	hosts map[string][]netip.Addr
	calls map[string]int
}

func (f *FakeResolver) LookupIPv4(_ context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip}, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[host]++
	ips, ok := f.hosts[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return append([]netip.Addr(nil), ips...), nil
}

// Set replaces the addresses of a host; no addresses removes it.
func (f *FakeResolver) Set(host string, ips ...netip.Addr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hosts == nil {
		f.hosts = map[string][]netip.Addr{}
	}
	if len(ips) == 0 {
		delete(f.hosts, host)
		return
	}
	f.hosts[host] = ips
}

// Lookups returns how often a host was looked up.
func (f *FakeResolver) Lookups(host string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[host]
}

// resolveFailure is the failed sample of a name that did not resolve: a
// timeout stays a timeout, everything else is class dns.
func resolveFailure(host string, err error) Sample {
	class, detail := ClassifyNetError(err)
	if class != ClassTimeout {
		class = ClassDNS
	}
	return Sample{Result: Fail, ErrClass: class, Detail: detail + " (" + host + ")"}
}

func noAddress(host string) error {
	return &net.DNSError{Err: "no IPv4 address", Name: host, IsNotFound: true}
}
