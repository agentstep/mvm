package cli

import "testing"

func TestResolveFormatDefaultsToJSONFlag(t *testing.T) {
	if got, err := resolveFormat("", true); err != nil || !got {
		t.Errorf(`resolveFormat("", true) = %v, %v; want true, nil`, got, err)
	}
	if got, err := resolveFormat("", false); err != nil || got {
		t.Errorf(`resolveFormat("", false) = %v, %v; want false, nil`, got, err)
	}
}

func TestResolveFormatJSON(t *testing.T) {
	got, err := resolveFormat("json", false)
	if err != nil || !got {
		t.Errorf(`resolveFormat("json", false) = %v, %v; want true, nil`, got, err)
	}
}

func TestResolveFormatTable(t *testing.T) {
	got, err := resolveFormat("table", true)
	if err != nil || got {
		t.Errorf(`resolveFormat("table", true) = %v, %v; want false, nil`, got, err)
	}
}

func TestResolveFormatInvalid(t *testing.T) {
	if _, err := resolveFormat("yaml", false); err == nil {
		t.Fatal(`resolveFormat("yaml", false) = nil error, want error`)
	}
}
