package correos

import (
	"context"

	"github.com/rclone/rclone/lib/rest"
)

const (
	apiIdentity = "https://apicorreosidservices.correos.es/Api/"
	appOID      = "066a6ffb-c90c-4f3e-98ec-0f56cfa5643e"
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

func (f *Fs) getRedirectURL(ctx context.Context) (string, error) {
	return "", nil
}

func (f *Fs) authorize(ctx context.Context, user, pass, redirectURL string) (string, error) {
	return "", nil
}

func (f *Fs) exchangeCode(ctx context.Context, code, redirectURL string) (*OAuthTokens, error) {
	return nil, nil
}

func (f *Fs) jwtLogin(ctx context.Context, tokens *OAuthTokens) (string, error) {
	return "", nil
}
