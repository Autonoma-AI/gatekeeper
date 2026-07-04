// Package discovery watches Namespaces matching a label selector and keeps
// the registry in sync with their route annotations: label a namespace and
// annotate its routes, and Gatekeeper manages it; delete or unlabel it, and
// the routes go with it. One namespace's bad annotation never affects the
// others.
package discovery

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/autonoma-ai/gatekeeper/internal/routing"
)

// issue is a per-namespace problem found while building desired state; it is
// logged and (when events are enabled) recorded on the namespace, and the
// affected namespace or host is skipped - never the whole rebuild.
type issue struct {
	namespace *corev1.Namespace
	reason    string // Event reason (CamelCase)
	message   string
}

// desired is the complete routing state derived from the labeled namespaces.
type desired struct {
	routes       map[string]routing.Upstream
	idleTimeouts map[string]time.Duration
	issues       []issue
}

// buildDesired turns the labeled namespaces into the full desired routing
// state. Namespaces are considered oldest-first (creation time, then name),
// so a host claimed by two namespaces deterministically goes to the older
// one. A terminating namespace is skipped: its workloads are already being
// deleted. defaultIdle applies unless the namespace overrides it via
// idleAnnotation.
func buildDesired(namespaces []*corev1.Namespace, routesAnnotation, idleAnnotation string, defaultIdle time.Duration) desired {
	sorted := make([]*corev1.Namespace, len(namespaces))
	copy(sorted, namespaces)
	sort.Slice(sorted, func(i, j int) bool {
		ti, tj := sorted[i].CreationTimestamp, sorted[j].CreationTimestamp
		if !ti.Equal(&tj) {
			return ti.Before(&tj)
		}
		return sorted[i].Name < sorted[j].Name
	})

	d := desired{
		routes:       map[string]routing.Upstream{},
		idleTimeouts: map[string]time.Duration{},
	}
	owner := map[string]string{} // host -> owning namespace, for collision reporting
	for _, ns := range sorted {
		if ns.DeletionTimestamp != nil {
			continue
		}
		ups, err := parseRoutesAnnotation(ns.Annotations[routesAnnotation], ns.Name)
		if err != nil {
			d.issues = append(d.issues, issue{ns, "InvalidRoutes",
				fmt.Sprintf("namespace is labeled for gatekeeper but not managed: %v", err)})
			continue
		}

		d.idleTimeouts[ns.Name] = defaultIdle
		if raw, ok := ns.Annotations[idleAnnotation]; ok {
			if t, err := time.ParseDuration(raw); err == nil {
				d.idleTimeouts[ns.Name] = t
			} else {
				d.issues = append(d.issues, issue{ns, "InvalidIdleTimeout",
					fmt.Sprintf("ignoring %s=%q (want a Go duration like \"45m\"); using the default %s", idleAnnotation, raw, defaultIdle)})
			}
		}

		for host, up := range ups {
			if _, taken := d.routes[host]; taken {
				d.issues = append(d.issues, issue{ns, "HostCollision",
					fmt.Sprintf("host %q is already routed to namespace %q (oldest namespace wins); ignoring this namespace's entry", host, owner[host])})
				continue
			}
			owner[host] = ns.Name
			d.routes[host] = up
		}
	}
	return d
}

// parseRoutesAnnotation parses one namespace's routes annotation. It applies
// the same validation as ROUTES_JSON with one extra rule: entries must NOT
// name a namespace. The annotation always routes into its own namespace -
// anything else would let whoever can annotate one namespace steer traffic
// into another.
func parseRoutesAnnotation(raw, namespace string) (map[string]routing.Upstream, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("missing or empty routes annotation")
	}
	var parsed map[string]routing.Upstream
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("routes annotation is not valid JSON: %w", err)
	}
	if len(parsed) == 0 {
		return nil, fmt.Errorf("routes annotation defines no host -> upstream mapping")
	}
	routes := make(map[string]routing.Upstream, len(parsed))
	for host, up := range parsed {
		if up.Namespace != "" {
			return nil, fmt.Errorf("entry %q names a namespace (%q): annotation routes always target their own namespace", host, up.Namespace)
		}
		if up.Service == "" {
			return nil, fmt.Errorf("entry %q has an empty service", host)
		}
		if up.Port <= 0 || up.Port > 65535 {
			return nil, fmt.Errorf("entry %q has an invalid port %d", host, up.Port)
		}
		up.Namespace = namespace
		routes[strings.ToLower(host)] = up
	}
	return routes, nil
}
