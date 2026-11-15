package kube

import (
	"testing"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestUsesAzureCLI(t *testing.T) {
	cases := map[string]struct {
		cmd  string
		args []string
		want bool
	}{
		"azurecli":        {"kubelogin", []string{"get-token", "--login", "azurecli", "--server-id", "x"}, true},
		"azurecli equals": {"/opt/homebrew/bin/kubelogin", []string{"get-token", "--login=azurecli"}, true},
		"devicecode":      {"kubelogin", []string{"get-token", "--login", "devicecode"}, false},
		"aws":             {"aws", []string{"eks", "get-token", "--login", "azurecli"}, false},
	}
	for name, c := range cases {
		ai := &clientcmdapi.AuthInfo{Exec: &clientcmdapi.ExecConfig{Command: c.cmd, Args: c.args}}
		if got := usesAzureCLI(ai); got != c.want {
			t.Errorf("%s: got %v", name, got)
		}
	}
	if usesAzureCLI(&clientcmdapi.AuthInfo{Token: "t"}) || usesAzureCLI(nil) {
		t.Error("non-exec users are not azurecli")
	}
}
