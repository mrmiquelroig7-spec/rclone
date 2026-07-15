package correos

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/rest"
)

const (
	apiIdentity    = "https://apicorreosidservices.correos.es/Api/"
	oauthIdentity  = "https://apioauthcid.correos.es/Api/"
	applicationOID = "066a6ffb-c90c-4f3e-98ec-0f56cfa5643e"
)

type OAuthTokens struct {
	IDToken      string `json:"idToken"`
	RefreshToken string `json:"refreshToken"`
	TokenType    string `json:"tokenType"`
	ExpiresIn    int    `json:"expiresIn"`
}

func (f *Fs) identityClient() *rest.Client {
	c := rest.NewClient(f.httpClient)
	c.SetRoot(apiIdentity)
	return c
}

func (f *Fs) oauthClient() *rest.Client {
	c := rest.NewClient(f.httpClient)
	c.SetRoot(oauthIdentity)
	return c
}

func (f *Fs) getRedirectURL(ctx context.Context) (string, error) {
	params := url.Values{}
	params.Set("applicationOid", applicationOID)

	opts := rest.Opts{
		Method:     "GET",
		Path:       "UtilitiesCorreosId/GetUrlRedirectOauth",
		Parameters: params,
		ExtraHeaders: map[string]string{
			"Accept":         "application/json",
			"Origin":         "https://identidad.correos.es",
			"Referer":        "https://identidad.correos.es/",
			"applicationOid": applicationOID,
		},
	}

	var result []string

	_, err := f.identityClient().CallJSON(ctx, &opts, nil, &result)
	if err != nil {
		return "", fmt.Errorf("get redirect URL: %w", err)
	}

	if len(result) == 0 {
		return "", fmt.Errorf("empty redirect URL response")
	}

	return result[0], nil
}

func extractCode(response string) (string, error) {
	_, after, ok := strings.Cut(response, "code=")
	if !ok {
		return "", fmt.Errorf("authorization code not found")
	}

	code := after

	if j := strings.Index(code, "&"); j != -1 {
		code = code[:j]
	}

	code, err := url.QueryUnescape(code)
	if err != nil {
		return "", fmt.Errorf("failed to unescape authorization code: %w", err)
	}

	return code, nil
}

type authorizeRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (f *Fs) authorize(ctx context.Context, username, password, redirectURL string) (string, error) {
	params := url.Values{}
	params.Set("redirect_uri", redirectURL)
	params.Set("response_type", "code")
	params.Set("state", uuid.NewString())
	params.Set("scope", "openid")
	params.Set("client_id", applicationOID)

	req := authorizeRequest{
		Username: username,
		Password: password,
	}

	opts := rest.Opts{
		Method: "POST",
		Path:   "Authorize?" + params.Encode(),
		Body:   strings.NewReader(`{"username":"` + username + `","password":"` + password + `"}`),
		ExtraHeaders: map[string]string{
			"Accept":          "application/json",
			"Content-Type":    "application/json",
			"ApplicationOid":  applicationOID,
			"Accept-Language": "es-ES,es;q=0.9",
		},
	}

	var response string

	_, err := f.oauthClient().CallJSON(ctx, &opts, req, &response)
	if err != nil {
		return "", fmt.Errorf("authorize: %w", err)
	}

	fs.Debugf(f, "Authorization response: %q", response)

	return response, nil
}

type tokenResponse struct {
	IDToken      string `json:"idToken"`
	RefreshToken string `json:"refreshToken"`
	Language     any    `json:"language"`
	TokenType    string `json:"tokenType"`
	ExpiresIn    int    `json:"expiresIn"`
}

func (f *Fs) getToken(ctx context.Context, code, redirectURL string) (string, error) {
	form := url.Values{}
	form.Set("redirect_uri", redirectURL)
	form.Set("code", code)
	form.Set("client_id", applicationOID)
	form.Set("grant_type", "authorization_code")

	opts := rest.Opts{
		Method:      "POST",
		Path:        "Authorize/token",
		Body:        strings.NewReader(form.Encode()),
		ContentType: "application/x-www-form-urlencoded",
		ExtraHeaders: map[string]string{
			"Accept":         "application/json",
			"Origin":         "https://identidad.correos.es",
			"Referer":        "https://identidad.correos.es/",
			"applicationOid": applicationOID,
		},
	}

	var response tokenResponse

	_, err := f.oauthClient().CallJSON(ctx, &opts, nil, &response)
	if err != nil {
		return "", fmt.Errorf("get token: %w", err)
	}

	fs.Debugf(f, "Token response: %#v", response)
	b, _ := json.MarshalIndent(response, "", "  ")
	fs.Debugf(f, "Token response JSON: \n%s", b)

	if response.IDToken == "" {
		return "", fmt.Errorf("idToken not found in token response")
	}

	return response.IDToken, nil

}

func (f *Fs) login(ctx context.Context, username, password string) (string, error) {
	redirectURL, err := f.getRedirectURL(ctx)
	if err != nil {
		return "", err
	}

	response, err := f.authorize(ctx, username, password, redirectURL)
	if err != nil {
		return "", err
	}

	code, err := extractCode(response)
	if err != nil {
		return "", fmt.Errorf("extract authorization code: %w", err)
	}

	return f.getToken(ctx, code, redirectURL)
}
