package correos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/rest"
)

var _ fs.Fs = (*Fs)(nil)
var _ fs.Object = (*Object)(nil)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "correos",
		Description: "Buzón Digital de Correos",
		NewFs:       NewFS,
		Options: []fs.Option{{
			Name:     "username",
			Help:     "CorreosID user",
			Required: false,
		},
			{
				Name:       "password",
				Help:       "CorreosID password",
				IsPassword: true,
				Required:   false,
			},
			{
				Name:     "jwt",
				Help:     "JWT (developer only)",
				Required: false,
				Advanced: true,
			}},
	})
}

type Fs struct {
	name       string
	root       string
	opt        Options
	srv        *rest.Client
	httpClient *http.Client
	mu         sync.Mutex // Protects dirCache
	dirCache   map[string]int64
}

func (f *Fs) Login(ctx context.Context) error {
	jwt, err := f.login(ctx, f.opt.Username, f.opt.Password)
	if err != nil {
		return err
	}

	f.opt.JWT = jwt
	f.srv.SetHeader("Authorization", jwt)

	return nil
}

type Options struct {
	Username string `config:"username"`
	Password string `config:"password"`
	JWT      string `config:"jwt"`
}

func NewFS(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := Options{}

	if v, ok := m.Get("jwt"); ok {
		opt.JWT = v
	}
	if v, ok := m.Get("username"); ok {
		opt.Username = v
	}
	if v, ok := m.Get("password"); ok {
		opt.Password = v
	}

	f := &Fs{
		name:     name,
		root:     root,
		opt:      opt,
		dirCache: make(map[string]int64),
	}
	f.dirCache[""] = 0

	client := &http.Client{Timeout: 60 * time.Second}
	f.httpClient = client

	f.srv = rest.NewClient(client).
		SetRoot("https://buzondigital.correos.es/api/v1.0/")

	f.srv.SetHeader("Accept", "application/json, text/plain, */*")
	f.srv.SetHeader("Origin", "https://buzondigital.correos.es")
	f.srv.SetHeader("Referer", "https://buzondigital.correos.es/")

	switch {
	case opt.JWT != "":
		// Use configured JWT if available
		f.srv.SetHeader("Authorization", opt.JWT)
		f.opt.JWT = opt.JWT

	case opt.Username != "" && opt.Password != "":
		// Regular mode with username and password
		if err := f.Login(ctx); err != nil {
			return nil, fmt.Errorf("login failed: %w", err)
		}

	default:
		return nil, errors.New("configure either 'jwt' or 'username' and 'password'")
	}

	return f, nil
}

type LoginResponse struct {
	IDToken string `json:"idToken"`
}

type CorreosItem struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	FileName  string `json:"fileName"`
	CreatedAt string `json:"createdAt"`
	ID        int64  `json:"id"`
	RawSize   any    `json:"size"`
}

func (i CorreosItem) DisplayName() string {
	switch {
	case strings.TrimSpace(i.Name) != "":
		return i.Name
	case strings.TrimSpace(i.FileName) != "":
		return i.FileName
	default:
		return ""
	}
}

type DocumentResponse struct {
	ID          int64  `json:"id"`
	FileName    string `json:"fileName"`
	Extension   string `json:"extension"`
	FileSize    any    `json:"fileSize"`
	CreatedAt   string `json:"createdAt"`
	DownloadUrl string `json:"downloadUrl"`
	Thumbnail   string `json:"thumbnail"`
	Parent      any    `json:"parent"`
	UserID      any    `json:"userId"`
	IsDeleted   any    `json:"isDeleted"`
	Shown       any    `json:"shown"`
}

func parseSize(value any) int64 {
	switch v := value.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case int32:
		return int64(v)
	case float64:
		return int64(v)
	case float32:
		return int64(v)
	case string:
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			if parsed, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
				return parsed
			}
		}
	case json.Number:
		if parsed, err := v.Int64(); err == nil {
			return parsed
		}
	}
	return 0
}

type ListResponse struct {
	Cursor string        `json:"cursor"`
	Items  []CorreosItem `json:"items"`
}

func (f *Fs) listItems(ctx context.Context, parentID int64) ([]CorreosItem, error) {
	parentStr := fmt.Sprintf("%d", parentID)

	opts := rest.Opts{
		Method: "GET",
		Path:   "folders/items",
		Parameters: url.Values{
			"parameters.order":  {"desc"},
			"parameters.sort":   {"folder_first"},
			"parameters.limit":  {"52"},
			"parameters.parent": {parentStr},
		},
		ExtraHeaders: map[string]string{
			"Accept":          "application/json, text/plain, */*",
			"Accept-Encoding": "gzip, deflate, br, zstd",
			"Accept-Language": "es-ES,es;q=0.9,ca;q=0.8,en-US;q=0.7,en;q=0.6",
			"Authorization":   f.opt.JWT,
			"Host":            "buzondigital.correos.es",
			"Referer":         "https://buzondigital.correos.es/",
			"Connection":      "keep-alive",
		},
	}

	var result ListResponse

	resp, err := f.srv.CallJSON(ctx, &opts, nil, &result)
	if err != nil {
		if resp != nil {
			_, _ = io.ReadAll(resp.Body)
		}
		return nil, err
	}
	return result.Items, nil
}

func splitRemotePath(remote string) []string {
	remote = strings.ReplaceAll(remote, `\`, "/")
	parts := strings.Split(remote, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func (f *Fs) resolvePath(remote string) string {
	remote = strings.ReplaceAll(strings.TrimSpace(remote), `\`, "/")
	root := strings.ReplaceAll(strings.TrimSpace(f.root), `\`, "/")
	if remote == "" {
		return root
	}
	if root == "" {
		return remote
	}
	return path.Join(root, remote)
}

func (f *Fs) resolveParentID(ctx context.Context, dir string) (int64, error) {
	dir = f.resolvePath(dir)
	if dir == "" {
		return 0, nil
	}

	f.mu.Lock()
	if parentID, ok := f.dirCache[dir]; ok {
		f.mu.Unlock()
		return parentID, nil
	}
	f.mu.Unlock()

	currentID := int64(0)
	currentPath := ""
	parts := splitRemotePath(dir)
	for idx, part := range parts {
		if part == "" {
			continue
		}
		items, err := f.listItems(ctx, currentID)
		if err != nil {
			return 0, err
		}

		found := false
		for _, item := range items {
			if !strings.EqualFold(item.DisplayName(), part) {
				continue
			}
			if idx == len(parts)-1 {
				if strings.EqualFold(strings.ToLower(item.Type), "folder") {
					currentID = item.ID
					found = true
					if currentPath == "" {
						currentPath = part
					} else {
						currentPath += "/" + part
					}
					f.mu.Lock()
					f.dirCache[currentPath] = currentID
					f.mu.Unlock()
				}
				return currentID, nil
			}
			if strings.EqualFold(strings.ToLower(item.Type), "folder") {
				currentID = item.ID
				found = true
				if currentPath == "" {
					currentPath = part
				} else {
					currentPath += "/" + part
				}
				f.mu.Lock()
				f.dirCache[currentPath] = currentID
				f.mu.Unlock()
				break
			}
		}
		if !found {
			return 0, fs.ErrorDirNotFound
		}
	}

	return currentID, nil
}

func (f *Fs) resolveItem(ctx context.Context, remote string) (*CorreosItem, error) {
	remote = f.resolvePath(remote)
	parts := splitRemotePath(remote)
	if len(parts) == 0 {
		return nil, fs.ErrorObjectNotFound
	}

	currentID := int64(0)
	for idx, part := range parts {
		items, err := f.listItems(ctx, currentID)
		if err != nil {
			return nil, err
		}

		found := false
		for _, item := range items {
			if !strings.EqualFold(item.DisplayName(), part) {
				continue
			}
			found = true
			if idx == len(parts)-1 {
				return &item, nil
			}
			if !strings.EqualFold(strings.ToLower(item.Type), "folder") {
				return nil, fs.ErrorObjectNotFound
			}
			currentID = item.ID
			break
		}
		if !found {
			if idx == len(parts)-1 {
				return nil, fs.ErrorObjectNotFound
			}
			return nil, fs.ErrorDirNotFound
		}
	}

	return nil, fs.ErrorObjectNotFound
}

func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	parentID, err := f.resolveParentID(ctx, dir)
	if err != nil {
		return nil, err
	}

	items, err := f.listItems(ctx, parentID)
	if err != nil {
		return nil, fmt.Errorf("error listing Correos elements: %w", err)
	}

	for _, item := range items {
		displayName := item.DisplayName()
		var rutaElemento string
		if dir == "" {
			rutaElemento = displayName
		} else {
			rutaElemento = dir + "/" + displayName
		}

		t, parseErr := time.Parse(time.RFC3339, item.CreatedAt)
		if parseErr != nil {
			t = time.Now()
		}

		switch strings.ToLower(item.Type) {
		case "folder":
			f.mu.Lock()
			f.dirCache[rutaElemento] = item.ID
			f.mu.Unlock()

			d := fs.NewDir(displayName, t)
			entries = append(entries, d)

		default:
			o := &Object{
				fs:      f,
				remote:  rutaElemento,
				id:      item.ID,
				doc:     &item,
				size:    parseSize(item.RawSize),
				modTime: t,
				loaded:  false,
			}
			if o.size == 0 {
				if doc, err := o.getDocument(ctx); err == nil && doc != nil {
					o.size = parseSize(doc.FileSize)
				}
			}
			entries = append(entries, o)
		}
	}

	return entries, nil
}

func (f *Fs) String() string                              { return f.name + ":" + f.root }
func (f *Fs) Name() string                                { return f.name }
func (f *Fs) Root() string                                { return f.root }
func (f *Fs) Precision() time.Duration                    { return fs.ModTimeNotSupported }
func (f *Fs) Mkdir(ctx context.Context, dir string) error { return nil }
func (f *Fs) Rmdir(ctx context.Context, dir string) error { return nil }
func (f *Fs) Features() *fs.Features                      { return &fs.Features{} }
func (f *Fs) Hashes() hash.Set                            { return hash.Set(hash.None) }
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	item, err := f.resolveItem(ctx, remote)
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, fs.ErrorObjectNotFound
	}

	route := strings.TrimSpace(remote)
	if route == "" {
		route = item.DisplayName()
	}

	obj := &Object{
		fs:      f,
		remote:  route,
		id:      item.ID,
		doc:     item,
		size:    parseSize(item.RawSize),
		modTime: time.Now(),
		loaded:  false,
	}
	if obj.size == 0 {
		if doc, err := obj.getDocument(ctx); err == nil && doc != nil {
			obj.size = parseSize(doc.FileSize)
		}
	}
	return obj, nil
}
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return nil, errors.New("Put operation not implemented")
}
func (f *Fs) deleteObject(context.Context, string) error {
	return nil
}

type Object struct {
	fs      *Fs
	remote  string
	id      int64
	doc     *CorreosItem
	size    int64
	modTime time.Time
	loaded  bool
}

func (o *Object) String() string                        { return o.remote }
func (o *Object) Remote() string                        { return o.remote }
func (o *Object) Size() int64                           { return o.size }
func (o *Object) ModTime(ctx context.Context) time.Time { return o.modTime }
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	return fs.ErrorCantSetModTime
}
func (o *Object) Fs() fs.Info { return o.fs }
func (o *Object) Hash(ctx context.Context, ty hash.Type) (string, error) {
	return "", hash.ErrUnsupported
}
func (o *Object) Storable() bool { return true }
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	return errors.New("Update operation not implemented")
}
func (o *Object) Remove(ctx context.Context) error { return o.fs.deleteObject(ctx, o.remote) }

func (o *Object) getDocument(ctx context.Context) (*DocumentResponse, error) {
	opts := rest.Opts{Method: http.MethodGet, Path: fmt.Sprintf("documents/%d", o.id), AuthRedirect: true}
	resp, err := o.fs.srv.Call(ctx, &opts)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("error fetching file metadata (%d): %s", resp.StatusCode, string(body))
	}
	var doc DocumentResponse
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

func (o *Object) openDownloadURL(ctx context.Context, downloadURL string) (io.ReadCloser, error) {
	if downloadURL == "" {
		return nil, errors.New("empty download URL")
	}

	if strings.HasPrefix(downloadURL, "http://") || strings.HasPrefix(downloadURL, "https://") {
		u, err := url.Parse(downloadURL)
		if err != nil {
			return nil, err
		}

		path := strings.TrimPrefix(u.Path, "/api/v1.0/")

		opts := rest.Opts{
			Method: http.MethodGet,
			Path:   path,
		}

		resp, err := o.fs.srv.Call(ctx, &opts)
		if err != nil {
			return nil, fmt.Errorf("error downloading file: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("error downloading file (%d): %s", resp.StatusCode, string(body))
		}

		return resp.Body, nil
	}

	opts := rest.Opts{
		Method:       http.MethodGet,
		Path:         downloadURL,
		AuthRedirect: true,
	}

	resp, err := o.fs.srv.Call(ctx, &opts)
	if err != nil {
		return nil, fmt.Errorf("error downloading file: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("error downloading file (%d): %s", resp.StatusCode, string(body))
	}
	return resp.Body, nil
}

func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	doc, err := o.getDocument(ctx)
	if err != nil {
		return nil, fmt.Errorf("error fetching file metadata: %w", err)
	}

	if doc != nil && doc.DownloadUrl != "" {
		return o.openDownloadURL(ctx, doc.DownloadUrl)
	}

	candidates := []string{
		fmt.Sprintf("files/%d", o.id),
		fmt.Sprintf("documents/%d", o.id),
		fmt.Sprintf("files/%d/download", o.id),
		fmt.Sprintf("files/download/%d", o.id),
		fmt.Sprintf("documents/%d/download", o.id),
	}

	for _, candidate := range candidates {
		candidate = strings.TrimPrefix(candidate, "/")
		opts := rest.Opts{Method: http.MethodGet, Path: candidate}
		resp, err := o.fs.srv.Call(ctx, &opts)
		if err != nil {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			continue
		}
		return resp.Body, nil
	}

	return nil, fmt.Errorf("error downloading file: unable to find valid endpoint")
}
