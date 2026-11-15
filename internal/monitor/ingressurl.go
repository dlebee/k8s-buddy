package monitor

import (
	"fmt"
	"net/url"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
)

// IngressURL builds the browser URL for one of an ingress's host/path rules. Only hosts and paths
// that are really in the ingress are accepted, so the UI can't be used to open arbitrary URLs.
func (m *Manager) IngressURL(clusterName, ns, name, host, path string) (string, error) {
	c, err := m.cluster(clusterName)
	if err != nil {
		return "", err
	}
	if !c.synced("ingresses") {
		return "", fmt.Errorf("ingresses are still loading on %s", clusterName)
	}
	ing, err := c.ingresses.Ingresses(ns).Get(name)
	if err != nil {
		return "", err
	}
	return ingressURL(ing, host, path)
}

func ingressURL(ing *networkingv1.Ingress, host, path string) (string, error) {
	if host == "" || strings.HasPrefix(host, "*") || !validObjectName(host) {
		return "", fmt.Errorf("host %q can't be visited", host)
	}
	found := false
	for _, r := range ing.Spec.Rules {
		if r.Host != host {
			continue
		}
		if path == "" {
			found = true
			break
		}
		if r.HTTP != nil {
			for _, p := range r.HTTP.Paths {
				if p.Path == path {
					found = true
				}
			}
		}
	}
	if !found {
		return "", fmt.Errorf("%s%s is not a route of ingress %s", host, path, ing.Name)
	}
	scheme := "http"
	for _, t := range ing.Spec.TLS {
		for _, h := range t.Hosts {
			if h == host {
				scheme = "https"
			}
		}
	}
	// Regex paths (nginx rewrite targets like /api(/|$)(.*)) are cut back to their literal prefix.
	if i := strings.IndexAny(path, "([*$^+?|{\\"); i >= 0 {
		path = path[:i]
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return (&url.URL{Scheme: scheme, Host: host, Path: path}).String(), nil
}
