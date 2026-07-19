package cli

import (
	"strings"
	"testing"
)

func TestGenerateVMNameAvoidsCollisions(t *testing.T) {
	existing := map[string]bool{}
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		name := GenerateVMName(existing)
		if seen[name] {
			t.Fatalf("GenerateVMName returned a duplicate: %q", name)
		}
		seen[name] = true
		existing[name] = true
	}
}

func TestGenerateVMNameShape(t *testing.T) {
	name := GenerateVMName(map[string]bool{})
	parts := strings.Split(name, "-")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Errorf("GenerateVMName() = %q, want two hyphen-separated non-empty words", name)
	}
}

func TestGenerateVMNameFallsBackWhenExhausted(t *testing.T) {
	existing := map[string]bool{}
	for _, a := range runNameAdjectives {
		for _, n := range runNameNouns {
			existing[a+"-"+n] = true
		}
	}
	name := GenerateVMName(existing)
	if !strings.HasPrefix(name, "run-") {
		t.Errorf("GenerateVMName() with exhausted space = %q, want run-<ts> fallback", name)
	}
}
