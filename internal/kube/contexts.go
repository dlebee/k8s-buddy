package kube

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type Context struct {
	Name        string `json:"name"`
	Cluster     string `json:"cluster"`
	Server      string `json:"server"`
	User        string `json:"user"`
	Fingerprint string `json:"-"`
}

type Kubeconfig struct {
	rules *clientcmd.ClientConfigLoadingRules
	raw   *clientcmdapi.Config
}

// Load reads the merged kubeconfig (explicit path, else $KUBECONFIG, else ~/.kube/config).
func Load(explicit string) (*Kubeconfig, []Context, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if explicit != "" {
		rules.ExplicitPath = explicit
	}
	raw, err := rules.Load()
	if err != nil {
		return nil, nil, err
	}
	out := make([]Context, 0, len(raw.Contexts))
	for name, c := range raw.Contexts {
		ctx := Context{Name: name, Cluster: c.Cluster, User: c.AuthInfo}
		cl := raw.Clusters[c.Cluster]
		if cl != nil {
			ctx.Server = cl.Server
		}
		ctx.Fingerprint = fingerprint(c, cl, raw.AuthInfos[c.AuthInfo])
		out = append(out, ctx)
	}
	slices.SortFunc(out, func(a, b Context) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return &Kubeconfig{rules: rules, raw: raw}, out, nil
}

func (k *Kubeconfig) RESTConfig(context string) (*rest.Config, error) {
	cc := clientcmd.NewNonInteractiveClientConfig(*k.raw, context, &clientcmd.ConfigOverrides{}, k.rules)
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, err
	}
	cfg.QPS = 20
	cfg.Burst = 40
	cfg.UserAgent = "k8s-buddy"
	return cfg, nil
}

func fingerprint(parts ...any) string {
	b, _ := json.Marshal(parts)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}
