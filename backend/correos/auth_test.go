package correos

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetRedirectURLIncludesExpectedQueryAndHeaders(t *testing.T) {
	var gotPath string
	var gotQuery string
	var gotHeaders http.Header

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]string{"https://example.com/callback"})
	}))
	defer server.Close()

	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "apicorreosidservices.correos.es" || req.URL.Host == "apioauthcid.correos.es" {
			req.URL.Scheme = "http"
			req.URL.Host = server.Listener.Addr().String()
		}
		return http.DefaultTransport.RoundTrip(req)
	})}

	f := &Fs{httpClient: client}

	_, err := f.getRedirectURL(context.Background())
	if err != nil {
		t.Fatalf("getRedirectURL returned error: %v", err)
	}

	if gotPath != "/Api/UtilitiesCorreosId/GetUrlRedirectOauth" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if gotQuery != "applicationOid="+applicationOID {
		t.Fatalf("unexpected query: %q", gotQuery)
	}

	for _, header := range []string{"Accept", "ApplicationOid"} {
		if _, ok := gotHeaders[http.CanonicalHeaderKey(header)]; !ok {
			t.Fatalf("expected header %q to be sent", header)
		}
	}

	if gotHeaders.Get("Accept") == "" {
		t.Fatalf("expected Accept header to be sent")
	}
	if gotHeaders.Get("ApplicationOid") != applicationOID {
		t.Fatalf("unexpected ApplicationOid header: %q", gotHeaders.Get("ApplicationOid"))
	}
}

func TestAuthorizeUsesExpectedQueryAndJsonBody(t *testing.T) {
	var gotPath string
	var gotQuery string
	var gotHeaders http.Header
	var gotBody map[string]string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotHeaders = r.Header.Clone()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read request body: %v", err)
		}
		if err := json.Unmarshal(body, &gotBody); err != nil {
			gotBody = nil
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`"https://example.com/callback"`))
	}))
	defer server.Close()

	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "apicorreosidservices.correos.es" || req.URL.Host == "apioauthcid.correos.es" {
			req.URL.Scheme = "http"
			req.URL.Host = server.Listener.Addr().String()
		}
		return http.DefaultTransport.RoundTrip(req)
	})}

	f := &Fs{httpClient: client}

	_, err := f.authorize(context.Background(), "user@example.com", "secret", "https://example.com/callback")
	if err != nil {
		t.Fatalf("authorize returned error: %v", err)
	}

	if gotPath != "/Api/Authorize" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if !strings.Contains(gotQuery, "redirect_uri=https%3A%2F%2Fexample.com%2Fcallback") {
		t.Fatalf("redirect_uri missing from query: %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "response_type=code") {
		t.Fatalf("response_type missing from query: %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "scope=openid") {
		t.Fatalf("scope missing from query: %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "client_id="+applicationOID) {
		t.Fatalf("client_id missing from query: %q", gotQuery)
	}

	if gotBody["username"] != "user@example.com" || gotBody["password"] != "secret" {
		t.Fatalf("unexpected request body: %+v", gotBody)
	}
	if gotHeaders.Get("Content-Type") != "application/json" {
		t.Fatalf("unexpected content-type: %q", gotHeaders.Get("Content-Type"))
	}
	if gotHeaders.Get("ApplicationOid") != applicationOID {
		t.Fatalf("unexpected ApplicationOid header: %q", gotHeaders.Get("ApplicationOid"))
	}
	if gotHeaders.Get("Accept") == "" {
		t.Fatalf("expected Accept header to be sent")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
