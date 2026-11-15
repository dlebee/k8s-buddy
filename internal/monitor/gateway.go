package monitor

import (
	"fmt"
	"slices"
)

// Gateway API (gateway.networking.k8s.io) support. Gateways, routes and classes are standard across
// implementations (Envoy Gateway, NGINX Gateway Fabric, Istio, Cilium, Traefik, Kong, Contour…), so
// everything here works from the spec; only finding a gateway's own Service has per-vendor labels,
// with a generic fallback on the addresses the Gateway reports.

const gatewayGroup = "gateway.networking.k8s.io"

// Route kinds we map, in the order they're listed.
var gatewayRouteKinds = []struct{ id, kind, resource string }{
	{"gw-httproutes", "HTTPRoute", "httproutes"},
	{"gw-grpcroutes", "GRPCRoute", "grpcroutes"},
	{"gw-tlsroutes", "TLSRoute", "tlsroutes"},
	{"gw-tcproutes", "TCPRoute", "tcproutes"},
	{"gw-udproutes", "UDPRoute", "udproutes"},
}

type gwListener struct {
	Name, Protocol, Hostname string
	Port                     int64
}

type gwGateway struct {
	Namespace, Name, Class string
	Listeners              []gwListener
	Addresses              []string
	Problem                string // not Accepted / not Programmed, from status
}

type gwParent struct{ Namespace, Name, Section string }

type gwBackend struct {
	Namespace, Name, Kind string
	Port                  int64
}

type gwRule struct {
	Paths    []string // "/api", "/pkg.Svc/Method"; empty = everything
	Backends []gwBackend
}

type gwRoute struct {
	Kind, Namespace, Name string
	Hostnames             []string
	Parents               []gwParent
	Rules                 []gwRule
	// Problems per parent ("ns/name"), from status.parents[].conditions.
	Problems map[string]string
}

type gatewayData struct {
	Gateways []gwGateway
	Routes   []gwRoute
	// GatewayClass name → controllerName.
	Classes map[string]string
}

func (g gatewayData) empty() bool { return len(g.Gateways) == 0 && len(g.Routes) == 0 }

// gatewayData reads the Gateway API objects the cluster's informers hold (none when the CRDs are absent).
func (c *cluster) gatewayData() gatewayData {
	d := gatewayData{Classes: map[string]string{}}
	for _, o := range c.unstructuredList("gw-gatewayclasses") {
		d.Classes[str(dig(o, "metadata", "name"))] = str(dig(o, "spec", "controllerName"))
	}
	for _, o := range c.unstructuredList("gw-gateways") {
		d.Gateways = append(d.Gateways, parseGateway(o))
	}
	for _, rk := range gatewayRouteKinds {
		for _, o := range c.unstructuredList(rk.id) {
			d.Routes = append(d.Routes, parseRoute(rk.kind, o))
		}
	}
	return d
}

func asList(v any) []any { l, _ := v.([]any); return l }

func int64Of(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	}
	return 0
}

// conditionProblem returns a message for the first of the wanted conditions that isn't True.
func conditionProblem(conds []any, want ...string) string {
	for _, w := range want {
		for _, c := range conds {
			m := asMap(c)
			if str(m["type"]) != w || str(m["status"]) == "True" {
				continue
			}
			msg := firstNonEmpty(str(m["message"]), str(m["reason"]), "status "+str(m["status"]))
			return fmt.Sprintf("not %s: %s", w, truncate(msg, 160))
		}
	}
	return ""
}

func parseGateway(o map[string]any) gwGateway {
	g := gwGateway{Namespace: str(dig(o, "metadata", "namespace")), Name: str(dig(o, "metadata", "name")), Class: str(dig(o, "spec", "gatewayClassName"))}
	for _, l := range asList(dig(o, "spec", "listeners")) {
		m := asMap(l)
		g.Listeners = append(g.Listeners, gwListener{Name: str(m["name"]), Protocol: str(m["protocol"]), Hostname: str(m["hostname"]), Port: int64Of(m["port"])})
	}
	for _, a := range asList(dig(o, "status", "addresses")) {
		if v := str(asMap(a)["value"]); v != "" {
			g.Addresses = append(g.Addresses, v)
		}
	}
	// Status is only written once a controller picks the Gateway up.
	conds := asList(dig(o, "status", "conditions"))
	if len(conds) == 0 {
		g.Problem = "no controller has accepted this Gateway (is its GatewayClass installed?)"
	} else {
		g.Problem = conditionProblem(conds, "Accepted", "Programmed")
	}
	return g
}

func parseRoute(kind string, o map[string]any) gwRoute {
	r := gwRoute{Kind: kind, Namespace: str(dig(o, "metadata", "namespace")), Name: str(dig(o, "metadata", "name")), Problems: map[string]string{}}
	for _, h := range asList(dig(o, "spec", "hostnames")) {
		r.Hostnames = append(r.Hostnames, str(h))
	}
	for _, p := range asList(dig(o, "spec", "parentRefs")) {
		m := asMap(p)
		if k := str(m["kind"]); k != "" && k != "Gateway" {
			continue // ListenerSet, Service (mesh) … aren't entry points
		}
		r.Parents = append(r.Parents, gwParent{Namespace: firstNonEmpty(str(m["namespace"]), r.Namespace), Name: str(m["name"]), Section: str(m["sectionName"])})
	}
	for _, rule := range asList(dig(o, "spec", "rules")) {
		rm := asMap(rule)
		var gr gwRule
		for _, mt := range asList(rm["matches"]) {
			mm := asMap(mt)
			if p := str(dig(mm, "path", "value")); p != "" {
				gr.Paths = append(gr.Paths, p)
			} else if svc := str(dig(mm, "method", "service")); svc != "" {
				gr.Paths = append(gr.Paths, "/"+svc+"/"+firstNonEmpty(str(dig(mm, "method", "method")), "*"))
			}
		}
		for _, b := range asList(rm["backendRefs"]) {
			bm := asMap(b)
			gr.Backends = append(gr.Backends, gwBackend{
				Namespace: firstNonEmpty(str(bm["namespace"]), r.Namespace), Name: str(bm["name"]),
				Kind: firstNonEmpty(str(bm["kind"]), "Service"), Port: int64Of(bm["port"]),
			})
		}
		r.Rules = append(r.Rules, gr)
	}
	for _, ps := range asList(dig(o, "status", "parents")) {
		pm := asMap(ps)
		ref := asMap(pm["parentRef"])
		key := firstNonEmpty(str(ref["namespace"]), r.Namespace) + "/" + str(ref["name"])
		if msg := conditionProblem(asList(pm["conditions"]), "Accepted", "ResolvedRefs"); msg != "" {
			r.Problems[key] = msg
		}
	}
	return r
}

// routeStrings lists "host/path" for every rule of a route, given the hostnames it effectively serves.
func (r gwRoute) routeStrings(hosts []string, rule gwRule) []string {
	paths := rule.Paths
	if len(paths) == 0 {
		paths = []string{"/"}
	}
	var out []string
	for _, h := range hosts {
		for _, p := range paths {
			out = append(out, h+p)
		}
	}
	return out
}

// effectiveHosts is what a route answers to on a gateway: its own hostnames, else the hostnames of
// the listeners it attaches to, else any host.
func effectiveHosts(r gwRoute, g *gwGateway, section string) []string {
	if len(r.Hostnames) > 0 {
		return r.Hostnames
	}
	var hs []string
	if g != nil {
		for _, l := range g.Listeners {
			if (section == "" || l.Name == section) && l.Hostname != "" && !slices.Contains(hs, l.Hostname) {
				hs = append(hs, l.Hostname)
			}
		}
	}
	if len(hs) == 0 {
		return []string{"*"}
	}
	return hs
}

// gatewayTLS reports whether the listeners a route attaches to terminate TLS.
func gatewayTLS(g *gwGateway, section string) bool {
	if g == nil {
		return false
	}
	for _, l := range g.Listeners {
		if (section == "" || l.Name == section) && (l.Protocol == "HTTPS" || l.Protocol == "TLS") {
			return true
		}
	}
	return false
}

func listenerPorts(g gwGateway) []string {
	out := []string{}
	for _, l := range g.Listeners {
		s := fmt.Sprintf("%d/%s", l.Port, l.Protocol)
		if l.Hostname != "" {
			s += " " + l.Hostname
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// Labels implementations put on the Service they create for a Gateway.
var gatewayServiceLabels = []struct{ name, namespace string }{
	{"gateway.networking.k8s.io/gateway-name", ""},                                                  // NGINX Gateway Fabric, Istio, Cilium, kgateway
	{"gateway.envoyproxy.io/owning-gateway-name", "gateway.envoyproxy.io/owning-gateway-namespace"}, // Envoy Gateway
	{"istio.io/gateway-name", ""},
}

// gatewayOfService names the Gateway ("ns/name") a Service fronts, or "".
func gatewayOfService(svcLabels map[string]string, svcNamespace string) string {
	for _, l := range gatewayServiceLabels {
		if n := svcLabels[l.name]; n != "" {
			ns := svcNamespace
			if l.namespace != "" && svcLabels[l.namespace] != "" {
				ns = svcLabels[l.namespace]
			}
			return ns + "/" + n
		}
	}
	return ""
}
