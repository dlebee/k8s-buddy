package monitor

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// Describing a Secret never prints a value: not a service account token (kubectl's describer
// prints that one in full), and not the copy kept in the last-applied annotation.
func TestDescribeSecretHidesValues(t *testing.T) {
	const token, applied = "made-up-token-value", "made-up-applied-value"
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "api-token", Namespace: "shop",
			Labels:      map[string]string{"app": "api"},
			Annotations: map[string]string{corev1.LastAppliedConfigAnnotation: `{"data":{"token":"` + applied + `"}}`, "owner": "team-a"}},
		Type: corev1.SecretTypeServiceAccountToken,
		Data: map[string][]byte{"token": []byte(token), "ca.crt": []byte("made-up-ca")},
	}
	c := newCluster("a", "https://a.example.com", nil, nil)
	c.cs = fake.NewSimpleClientset(sec)
	out, err := c.describeSecret("shop", "api-token")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{token, applied, "made-up-ca"} {
		if strings.Contains(out, secret) {
			t.Fatalf("a value is in the output:\n%s", out)
		}
	}
	for _, want := range []string{"api-token", "shop", "app=api", "owner=team-a", string(corev1.SecretTypeServiceAccountToken), "token:  19 bytes", "ca.crt:  10 bytes"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if _, err := c.describeSecret("shop", "../api-token"); err == nil {
		t.Fatal("an odd name was accepted")
	}
}
