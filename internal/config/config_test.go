package config

import "testing"

func TestNormalizeTags(t *testing.T) {
	got := normalizeTags(map[string][]string{"a": {" Prod ", "prod", "East US", "", "x<y>"}, "b": {"  "}})
	if len(got) != 1 || len(got["a"]) != 3 || got["a"][0] != "east-us" || got["a"][1] != "prod" || got["a"][2] != "x-y" {
		t.Fatalf("got %v", got)
	}
}

func TestBetaFeaturesNormalized(t *testing.T) {
	s := Settings{Beta: []string{"cost", "nope", "cost"}}
	s.normalize()
	if len(s.Beta) != 1 || !s.BetaOn("cost") || s.BetaOn("nope") {
		t.Fatalf("beta: %v", s.Beta)
	}
	if d := Defaults(); d.BetaOn("cost") {
		t.Fatal("cost must be off by default")
	}
}
