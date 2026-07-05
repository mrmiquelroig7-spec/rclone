package correos

import (
	"encoding/json"
	"testing"
)

func TestCorreosItemDisplayNameFallsBackToEitherField(t *testing.T) {
	var item CorreosItem
	if err := json.Unmarshal([]byte(`{"name":"doc.pdf"}`), &item); err != nil {
		t.Fatalf("unmarshal name field: %v", err)
	}
	if got := item.DisplayName(); got != "doc.pdf" {
		t.Fatalf("expected name field to be used, got %q", got)
	}

	var fallback CorreosItem
	if err := json.Unmarshal([]byte(`{"fileName":"doc2.pdf"}`), &fallback); err != nil {
		t.Fatalf("unmarshal fileName field: %v", err)
	}
	if got := fallback.DisplayName(); got != "doc2.pdf" {
		t.Fatalf("expected fileName field to be used, got %q", got)
	}
}

func TestSplitRemotePathHandlesWindowsSeparators(t *testing.T) {
	got := splitRemotePath(`Contratos\zeleris_REACON00005174_20260602.pdf`)
	want := []string{"Contratos", "zeleris_REACON00005174_20260602.pdf"}
	if len(got) != len(want) {
		t.Fatalf("expected %d parts, got %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected part %d to be %q, got %q", i, want[i], got[i])
		}
	}
}

func TestCorreosItemUsesSizeFromListPayload(t *testing.T) {
	var item CorreosItem
	if err := json.Unmarshal([]byte(`{"size":1234}`), &item); err != nil {
		t.Fatalf("unmarshal size field: %v", err)
	}
	if got := parseSize(item.RawSize); got != 1234 {
		t.Fatalf("expected size 1234 from list payload, got %d", got)
	}
}
