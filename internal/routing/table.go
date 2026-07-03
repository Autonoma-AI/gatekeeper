// Package routing maps incoming request hostnames to in-cluster upstream Services.
package routing

import (
	"fmt"
	"net"
	"strings"
)

// Upstream is a single app's in-cluster Service target. It is also the shape of
// each value in the ROUTES_JSON config map (host -> upstream). Namespace is the
// namespace the Service lives in; config fills it from NAMESPACE when omitted.
type Upstream struct {
	Namespace string `json:"namespace,omitempty"`
	Service   string `json:"service"`
	Port      int    `json:"port"`
}

// URL returns the upstream's in-cluster Service URL, e.g.
// http://web.preview-acme-pr-12.svc.cluster.local:3000
func (u Upstream) URL() string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", u.Service, u.Namespace, u.Port)
}

// Table resolves request hostnames to upstreams.
type Table struct {
	routes map[string]Upstream
}

// NewTable builds a routing table. Hostnames are stored lowercased so lookups
// are case-insensitive regardless of how the Host header is cased.
func NewTable(routes map[string]Upstream) *Table {
	normalized := make(map[string]Upstream, len(routes))
	for host, up := range routes {
		normalized[strings.ToLower(host)] = up
	}
	return &Table{routes: normalized}
}

// Resolve maps a request's Host header to its upstream. Any port suffix is
// stripped and the host lowercased before lookup. The second return value is
// false when no route matches the host.
func (t *Table) Resolve(hostHeader string) (Upstream, bool) {
	up, ok := t.routes[normalizeHost(hostHeader)]
	return up, ok
}

func normalizeHost(hostHeader string) string {
	host := strings.ToLower(strings.TrimSpace(hostHeader))
	// Host may carry a :port suffix (e.g. "app.example.com:443"); strip it.
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
