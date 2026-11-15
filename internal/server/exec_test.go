package server

import (
	"strings"
	"testing"
)

func TestCheckCommand(t *testing.T) {
	cases := []struct {
		cmd      string
		mutating string
		err      string
	}{
		{"kubectl --context c -n ns describe pod web-0", "", ""},
		{"kubectl --context 'my ctx' -n ns logs web-0 -c app --previous", "", ""},
		{`kubectl --context c -n ns get secret s -o go-template='{{index .data "a.b" | base64decode}}'`, "", ""},
		{"kubectl --context c -n ns delete pod web-0", "delete", ""},
		{"kubectl --context c -n ns rollout restart deploy/web", "rollout restart", ""},
		{"kubectl --context c rollout status deploy/web", "", ""},
		{"helm --kube-context c -n ns rollback web 3", "rollback", ""},
		{"helm --kube-context c -n ns status web", "", ""},
		{"kubectl get pods | grep web", "", "shell syntax"},
		{"kubectl get pods; rm -rf /", "", "shell syntax"},
		{"kubectl get pods $(whoami)", "", "shell syntax"},
		{"rm -rf /", "", "only kubectl"},
		{"bash -c 'kubectl get pods'", "", "only kubectl"},
		{"kubectl -n ns exec -it web-0 -- sh", "", "interactive"},
		{"kubectl -n ns edit deploy/web", "", "interactive"},
		{"kubectl -n ns logs -f web-0", "", "streams forever"},
		{"kubectl get pods --watch", "", "streams forever"},
		{"kubectl get pods 'unterminated", "", "unterminated"},
	}
	for _, c := range cases {
		got, err := checkCommand(c.cmd)
		switch {
		case c.err != "":
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%q: want error containing %q, got %v", c.cmd, c.err, err)
			}
		case err != nil:
			t.Errorf("%q: unexpected error %v", c.cmd, err)
		case got.mutating != c.mutating:
			t.Errorf("%q: mutating = %q, want %q", c.cmd, got.mutating, c.mutating)
		}
	}
	got, _ := checkCommand(`kubectl --context 'my ctx' get secret s -o go-template='{{index .data "k"}}'`)
	if got.argv[2] != "my ctx" || got.argv[len(got.argv)-1] != `go-template={{index .data "k"}}` {
		t.Errorf("quoting: %q", got.argv)
	}
}
