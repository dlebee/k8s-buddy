package monitor

import "testing"

func TestMinorOf(t *testing.T) {
	cases := map[string]string{"v1.28.5": "1.28", "v1.31.2-gke.1000": "1.31", "1.30.0+k3s1": "1.30", "": "", "garbage": ""}
	for in, want := range cases {
		if got := minorOf(in); got != want {
			t.Errorf("minorOf(%q) = %q, want %q", in, got, want)
		}
	}
}
