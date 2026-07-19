package correos

import "time"

type Item struct {
	Type      string    `json:"type"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	ID        int       `json:"id"`
}

type FolderResponse struct {
	Cursor string `json:"cursor"`
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Items  []Item `json:"items"`
}

type TokenResponse struct {
	RefreshToken string `json:"refreshToken"`
	IDToken      string `json:"idToken"`
	TokenType    string `json:"tokenType"`
	ExpiresIn    int    `json:"expiresIn"`
}

type CreateFolderRequest struct {
	Name string `json:"name"`
}
