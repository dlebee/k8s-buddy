package monitor

import "testing"

func TestObjectPathRefusesTraversalAndCollections(t *testing.T) {
	ing := APIResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses", Kind: "Ingress", Namespaced: true}
	sec := genericBuiltins["secrets"].res
	role := APIResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles", Kind: "ClusterRole"}

	bad := []struct {
		res      APIResource
		ns, name string
	}{
		{sec, "buddy-demo", ""}, // the whole SecretList of a namespace
		{sec, "", "db"},         // namespaced kind without a namespace
		{ing, "buddy-demo", "../../../../api/v1/secrets"},
		{ing, "buddy-demo", ".."},
		{ing, "../..", "web"},
		{ing, "buddy-demo", "web%2F..%2Fsecrets"},
		{ing, "buddy-demo", "web?watch=1"},
		{role, "", ""},
	}
	for _, c := range bad {
		if p, err := c.res.objectPath(c.ns, c.name); err == nil {
			t.Errorf("objectPath(%q, %q) = %q, want an error", c.ns, c.name, p)
		}
	}

	good := []struct {
		res      APIResource
		ns, name string
		want     string
	}{
		{sec, "buddy-demo", "db", "/api/v1/namespaces/buddy-demo/secrets/db"},
		{ing, "buddy-demo", "web", "/apis/networking.k8s.io/v1/namespaces/buddy-demo/ingresses/web"},
		{role, "", "system:controller:web", "/apis/rbac.authorization.k8s.io/v1/clusterroles/system:controller:web"},
		{role, "ignored", "admin", "/apis/rbac.authorization.k8s.io/v1/clusterroles/admin"},
	}
	for _, c := range good {
		p, err := c.res.objectPath(c.ns, c.name)
		if err != nil || p != c.want {
			t.Errorf("objectPath(%q, %q) = %q, %v; want %q", c.ns, c.name, p, err, c.want)
		}
	}
}

func TestSingleObjectRefusesLists(t *testing.T) {
	list := map[string]any{"kind": "SecretList", "items": []any{map[string]any{"data": map[string]any{"password": "aHVudGVyMg=="}}}}
	if singleObject(list) == nil {
		t.Error("a SecretList must be refused")
	}
	if singleObject(map[string]any{"metadata": map[string]any{"resourceVersion": "1"}, "items": []any{}}) == nil {
		t.Error("a nameless response must be refused")
	}
	if err := singleObject(map[string]any{"kind": "Secret", "metadata": map[string]any{"name": "db"}}); err != nil {
		t.Errorf("a single object was refused: %v", err)
	}
}
