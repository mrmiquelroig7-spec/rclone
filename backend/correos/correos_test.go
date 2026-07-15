package correos

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/rclone/rclone/lib/rest"
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

func TestListItemsBuildsExpectedPathAndQuery(t *testing.T) {
	var gotPath string
	var gotQuery url.Values

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ListResponse{Items: []CorreosItem{}})
	}))
	defer server.Close()

	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "buzondigital.correos.es" {
			req.URL.Scheme = "http"
			req.URL.Host = server.Listener.Addr().String()
		}
		return http.DefaultTransport.RoundTrip(req)
	})}

	f := &Fs{httpClient: client}
	f.srv = rest.NewClient(client)
	f.srv.SetRoot("https://buzondigital.correos.es/api/v1.0/")

	_, err := f.listItems(context.Background(), 0)
	if err != nil {
		t.Fatalf("listItems returned error: %v", err)
	}

	if gotPath != "/api/v1.0/folders/items" && gotPath != "/folders/items" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if gotQuery.Get("parameters.order") != "desc" {
		t.Fatalf("unexpected parameters.order: %q", gotQuery.Get("parameters.order"))
	}
	if gotQuery.Get("parameters.sort") != "folder_first" {
		t.Fatalf("unexpected parameters.sort: %q", gotQuery.Get("parameters.sort"))
	}
	if gotQuery.Get("parameters.limit") != "52" {
		t.Fatalf("unexpected parameters.limit: %q", gotQuery.Get("parameters.limit"))
	}
	if gotQuery.Get("parameters.parent") != "0" {
		t.Fatalf("unexpected parameters.parent: %q", gotQuery.Get("parameters.parent"))
	}
}
