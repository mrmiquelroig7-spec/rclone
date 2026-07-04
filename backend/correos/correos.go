package correos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/rest"
)

var _ fs.Fs = (*Fs)(nil)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "correos",
		Description: "Buzón Digital de Correos",
		NewFs:       NewFS,
		Options: []fs.Option{{
			Name:     "jwt",
			Help:     "JWT obtenido del localStorage del navegador",
			Required: true,
		}},
	})
}

type Fs struct {
	name     string
	root     string
	opt      Options
	srv      *rest.Client
	mu       sync.Mutex // Protects dirCache
	dirCache map[string]int64
}

type Options struct {
	JWT string `config:"jwt"`
}

func NewFS(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	rawJWT, ok := m.Get("jwt")
	if !ok || rawJWT == "" {
		return nil, errors.New("se requiere un jwt válido")
	}

	opt := Options{
		JWT: rawJWT,
	}

	f := &Fs{
		name:     name,
		root:     root,
		opt:      opt,
		dirCache: make(map[string]int64),
	}
	f.dirCache[""] = 0

	client := &http.Client{Timeout: 60 * time.Second}
	f.srv = rest.NewClient(client).SetRoot("https://buzondigital.correos.es/api/v1.0/")

	f.srv.SetHeader("Accept", "application/json, text/plain, */*")
	f.srv.SetHeader("Origin", "https://buzondigital.correos.es")
	f.srv.SetHeader("Referer", "https://buzondigital.correos.es/")
	f.srv.SetHeader("Authorization", opt.JWT)

	return f, nil
}

type LoginResponse struct {
	IDToken string `json:"idToken"`
}

type CorreosItem struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	CreatedAt string `json:"createdAt"`
	ID        int64  `json:"id"`
}

type ListResponse struct {
	Cursor string        `json:"cursor"`
	Items  []CorreosItem `json:"items"`
}

func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	f.mu.Lock()
	parentID, existe := f.dirCache[dir]
	f.mu.Unlock()

	if !existe {
		return nil, fs.ErrorDirNotFound
	}
	parentStr := fmt.Sprintf("%d", parentID)

	opts := rest.Opts{
		Method: "GET",
		Path: fmt.Sprintf(
			"/folders/items?parameters.order=desc&parameters.sort=folder_first&parameters.limit=52&parameters.parent=%s",
			url.QueryEscape(parentStr),
		)}

	var result ListResponse
	fs.Debugf(f, "List(%q): GET %s", dir, opts.Path)
	_, err = f.srv.CallJSON(ctx, &opts, nil, &result)
	b, _ := json.MarshalIndent(result, "", "  ")
	fs.Debugf(f, "List(%q): received %d items", dir, len(result.Items))
	if err != nil {
		return nil, fmt.Errorf("error al listar elementos de Correos: %w", err)
	}

	for _, item := range result.Items {
		var rutaElemento string
		if dir == "" {
			rutaElemento = item.Name
		} else {
			rutaElemento = dir + "/" + item.Name
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

			d := fs.NewDir(item.Name, t)
			entries = append(entries, d)

		default:
			o := &Object{
				fs:      f,
				remote:  rutaElemento,
				id:      item.ID,
				size:    0,
				modTime: t,
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
	return nil, fs.ErrorObjectNotFound
}
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return nil, errors.New("operación Put no implementada")
}

type Object struct {
	fs      *Fs
	remote  string
	id      int64
	size    int64
	modTime time.Time
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

func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	fileIDStr := fmt.Sprintf("%d", o.id)

	opts := rest.Opts{
		Method: "GET",
		Path:   "/files/download/" + fileIDStr,
	}

	resp, err := o.fs.srv.Call(ctx, &opts)
	if err != nil {
		return nil, fmt.Errorf("error al descargar el archivo: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)

		return nil, fmt.Errorf(
			"error al descargar el archivo (%d): %s",
			resp.StatusCode,
			string(body),
		)
	}

	return resp.Body, nil
}
