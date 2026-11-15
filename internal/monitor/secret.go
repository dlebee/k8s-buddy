package monitor

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// secretSizes reports each key's decoded size from a raw Secret before its values are stripped.
func secretSizes(obj map[string]any) map[string]int {
	out := map[string]int{}
	if data, ok := obj["data"].(map[string]any); ok {
		for k, v := range data {
			out[k] = base64.StdEncoding.DecodedLen(len(str(v)))
			if b, err := base64.StdEncoding.DecodeString(str(v)); err == nil {
				out[k] = len(b)
			}
		}
	}
	if sd, ok := obj["stringData"].(map[string]any); ok {
		for k, v := range sd {
			out[k] = len(str(v))
		}
	}
	return out
}

// RevealSecretKey reads one key of one Secret, right now, for someone who explicitly asked to see
// it. Nothing is cached; the rest of the Secret is dropped as soon as the key is picked out.
func (m *Manager) RevealSecretKey(clusterName, ns, name, key string) ([]byte, error) {
	if !validObjectName(name) || !validObjectName(ns) || key == "" || len(key) > 253 {
		return nil, fmt.Errorf("invalid secret reference")
	}
	c, err := m.cluster(clusterName)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, err := c.cs.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if v, ok := s.Data[key]; ok {
		return v, nil
	}
	if v, ok := s.StringData[key]; ok {
		return []byte(v), nil
	}
	return nil, fmt.Errorf("secret %s/%s has no key %q", ns, name, key)
}

// HelmValues are the values a user supplied for one revision of a release (what `helm get values`
// prints), read on explicit request from Helm's storage secret. Rendered manifests, hooks and the
// rest of the record are decoded in memory and dropped, never returned.
type HelmValues struct {
	Revision     int            `json:"revision"`
	Status       string         `json:"status"`
	Chart        string         `json:"chart"`
	ChartVersion string         `json:"chartVersion"`
	AppVersion   string         `json:"appVersion,omitempty"`
	Values       map[string]any `json:"values"`
}

func (m *Manager) HelmReleaseValues(clusterName, ns, name string, revision int) (HelmValues, error) {
	var hv HelmValues
	if !validHelmName(name) || !validObjectName(ns) || revision < 0 {
		return hv, fmt.Errorf("invalid release reference")
	}
	c, err := m.cluster(clusterName)
	if err != nil {
		return hv, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if revision == 0 { // latest: find the highest revision from labels (metadata only)
		d, err := c.helmHistory(ObjectDetail{Namespace: ns, Name: name})
		if err != nil {
			return hv, err
		}
		revision = d.History[0].Revision
	}
	s, err := c.cs.CoreV1().Secrets(ns).Get(ctx, fmt.Sprintf("sh.helm.release.v1.%s.v%d", name, revision), metav1.GetOptions{})
	if err != nil {
		return hv, err
	}
	if s.Labels["owner"] != "helm" || s.Labels["name"] != name {
		return hv, fmt.Errorf("not a Helm release record")
	}
	rel, err := decodeHelmRelease(s.Data["release"])
	if err != nil {
		return hv, err
	}
	hv.Revision, hv.Status = revision, s.Labels["status"]
	hv.Chart = str(dig(rel, "chart", "metadata", "name"))
	hv.ChartVersion = str(dig(rel, "chart", "metadata", "version"))
	hv.AppVersion = str(dig(rel, "chart", "metadata", "appVersion"))
	hv.Values, _ = rel["config"].(map[string]any)
	if hv.Values == nil {
		hv.Values = map[string]any{}
	}
	return hv, nil
}

// decodeHelmRelease undoes Helm's storage encoding: base64, then (usually) gzip, then JSON.
func decodeHelmRelease(data []byte) (map[string]any, error) {
	b, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return nil, fmt.Errorf("release record is not base64: %w", err)
	}
	if len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		if b, err = io.ReadAll(io.LimitReader(zr, 64<<20)); err != nil {
			return nil, err
		}
	}
	var rel map[string]any
	if err := json.Unmarshal(b, &rel); err != nil {
		return nil, fmt.Errorf("release record is not JSON: %w", err)
	}
	return rel, nil
}
