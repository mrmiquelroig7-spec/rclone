package correos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
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

var (
	_ fs.Fs     = (*Fs)(nil)
	_ fs.Object = (*Object)(nil)
	_ fs.Purger = (*Fs)(nil)
	// _ fs.PutStreamer = (*Fs)(nil)
	commandHelp = []fs.CommandHelp{
		{
			Name:  "restore",
			Short: "Restore a document from the trash by its ID",
		},
		{
			Name:  "delete_permanently",
			Short: "Permanently delete a document from the trash by its ID",
		},
	}
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "correos",
		Description: "Buzón Digital de Correos",
		NewFs:       NewFS,
		Options: []fs.Option{
			{
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
			},
			{
				Name:     "trashed_only",
				Help:     "List only documents in the trash",
				Default:  false,
				Advanced: true,
			},
		},
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
	features   *fs.Features
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
	Username    string `config:"username"`
	Password    string `config:"password"`
	JWT         string `config:"jwt"`
	TrashedOnly bool
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
	if v, ok := m.Get("trashed_only"); ok {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("invalid trashed_only value %q: %w", v, err)
		}
		opt.TrashedOnly = parsed
	}

	f := &Fs{
		name:     name,
		root:     root,
		opt:      opt,
		dirCache: make(map[string]int64),
	}

	f.dirCache[""] = 0

	f.features = (&fs.Features{}).Fill(ctx, f)

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

	if f.root != "" {
		_, err := f.resolveAbsoluteFolderID(ctx, f.root)
		switch {
		case err == nil:
			return f, nil
		case !errors.Is(err, fs.ErrorDirNotFound):
			return nil, err
		}

		if _, err := f.NewObject(ctx, ""); err == nil {
			f.root = path.Dir(f.root)
			if f.root == "." {
				f.root = ""
			}
			return f, fs.ErrorIsFile
		} else if !errors.Is(err, fs.ErrorObjectNotFound) {
			return nil, err
		}
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
	Name        string `json:"name"`
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

func (f *Fs) Command(ctx context.Context, name string, arg []string, opt map[string]string) (out interface{}, err error) {
	switch name {
	case "help":
		return commandHelp, nil

	case "restore":
		if len(arg) != 1 {
			return nil, errors.New("usage: rclone backend restore remote: document-id")
		}

		id, err := strconv.ParseInt(arg[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid document ID %q: %w", arg[0], err)
		}

		return nil, f.restoreDocument(ctx, id)

	case "delete_permanently":
		if len(arg) != 1 {
			return nil, errors.New("usage: rclone backend delete_permanently remote: document-id")
		}

		id, err := strconv.ParseInt(arg[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid document ID %q: %w", arg[0], err)
		}

		return nil, f.deleteDocumentPermanently(ctx, id)
	}

	return nil, fs.ErrorCommandNotFound
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
	fs.Debugf(f, "resolvePath: remote=%q root=%q", remote, f.root)
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

func (f *Fs) resolveFolderID(ctx context.Context, dir string) (int64, error) {
	fs.Debugf(f, "resolveFolderID: input=%q", dir)
	dir = f.resolvePath(dir)
	fs.Debugf(f, "resolveFolderID: resolved=%q", dir)
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

func (f *Fs) resolveAbsoluteFolderID(ctx context.Context, dir string) (int64, error) {
	fs.Debugf(f, "resolveAbsoluteFolderID: input=%q", dir)

	dir = strings.Trim(dir, "/")
	if dir == "" {
		return 0, nil
	}

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

			if !strings.EqualFold(strings.ToLower(item.Type), "folder") {
				continue
			}

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

			if idx == len(parts)-1 {
				return currentID, nil
			}

			break
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
	fs.Debugf(f, "List: trashed_only=%v", f.opt.TrashedOnly)

	if f.opt.TrashedOnly {
		return f.listTrash(ctx)
	}

	parentID, err := f.resolveFolderID(ctx, dir)
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

func (f *Fs) String() string           { return f.name + ":" + f.root }
func (f *Fs) Name() string             { return f.name }
func (f *Fs) Root() string             { return f.root }
func (f *Fs) Precision() time.Duration { return fs.ModTimeNotSupported }

func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	if dir == "" {
		dir = f.root
	}

	parent := path.Dir(dir)
	if parent == "." {
		parent = ""
	}

	var (
		parentID int64
		err      error
	)

	if parent == "" {
		parentID = 0
	} else {
		parentID, err = f.resolveAbsoluteFolderID(ctx, parent)
		if err != nil {
			return err
		}
	}

	name := path.Base(dir)

	req := map[string]string{
		"name": name,
	}

	var result FolderResponse

	opts := rest.Opts{
		Method: http.MethodPost,
		Path:   fmt.Sprintf("folders/%d", parentID),
	}

	_, err = f.srv.CallJSON(ctx, &opts, &req, &result)

	if err != nil {
		return err
	}

	f.mu.Lock()
	f.dirCache[dir] = result.ID
	f.mu.Unlock()

	return nil
}

func (f *Fs) deleteFolder(ctx context.Context, dir string, deleteContent bool) error {
	if dir == f.root {
		dir = ""
	}

	id, err := f.resolveFolderID(ctx, dir)
	if err != nil {
		return err
	}

	opts := rest.Opts{
		Method: http.MethodDelete,
		Path:   fmt.Sprintf("folders/%d", id),
		Parameters: url.Values{
			"deleteContent": []string{strconv.FormatBool(deleteContent)},
		},
	}

	var result bool
	_, err = f.srv.CallJSON(ctx, &opts, nil, &result)
	if err != nil {
		return err
	}

	if !result {
		return errors.New("folder deletion failed")
	}

	f.mu.Lock()
	delete(f.dirCache, dir)
	f.mu.Unlock()

	return nil
}

func (f *Fs) Rmdir(ctx context.Context, dir string) error { return f.deleteFolder(ctx, dir, false) }
func (f *Fs) Purge(ctx context.Context, dir string) error { return f.deleteFolder(ctx, dir, true) }

func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	fs.Debugf(f, "DirMove: %q -> %q", srcRemote, dstRemote)

	srcFs, ok := src.(*Fs)
	if !ok {
		return fs.ErrorCantDirMove
	}

	srcDir := path.Join(srcFs.root, srcRemote)
	dstDir := path.Join(f.root, dstRemote)

	fs.Debugf(f, "DirMove: srcDir=%q dstDir=%q", srcDir, dstDir)

	srcID, err := srcFs.resolveAbsoluteFolderID(ctx, srcDir)
	if err != nil {
		return err
	}

	dstID, err := f.resolveAbsoluteFolderID(ctx, dstDir)

	if err == nil {
		if err := f.moveFolder(ctx, srcID, dstID); err != nil {
			return err
		}
	} else if errors.Is(err, fs.ErrorDirNotFound) {

		dstParent := path.Dir(dstDir)
		if dstParent == "." {
			dstParent = ""
		}

		parentID, err := f.resolveAbsoluteFolderID(ctx, dstParent)
		if err != nil {
			return err
		}

		if err := f.moveFolder(ctx, srcID, parentID); err != nil {
			return err
		}

		dstName := path.Base(dstDir)
		if dstName != "." && dstName != "" {
			if err := f.renameFolder(ctx, srcID, dstName); err != nil {
				return err
			}
		}
	} else {
		return err
	}

	finalDir := dstDir

	if err == nil {
		finalDir = path.Join(dstDir, path.Base(srcDir))
	}

	f.mu.Lock()
	delete(f.dirCache, srcDir)
	f.dirCache[finalDir] = srcID
	f.mu.Unlock()

	return nil
}

func (f *Fs) moveFolder(ctx context.Context, id, parentID int64) error {
	fs.Debugf(f, "moveFolder: id=%d parentID=%d", id, parentID)
	req := map[string]int64{
		"parentId": parentID,
	}

	var result FolderResponse

	opts := rest.Opts{
		Method: http.MethodPost,
		Path:   fmt.Sprintf("folders/%d/move", id),
	}

	_, err := f.srv.CallJSON(ctx, &opts, &req, &result)
	if err != nil {
		return err
	}

	return nil
}

func (f *Fs) renameFolder(ctx context.Context, id int64, name string) error {
	fs.Debugf(f, "renameFolder: id=%d name=%q", id, name)
	req := map[string]string{
		"name": name,
	}

	var result FolderResponse

	opts := rest.Opts{
		Method: http.MethodPost,
		Path:   fmt.Sprintf("folders/%d/rename", id),
	}

	_, err := f.srv.CallJSON(ctx, &opts, &req, &result)
	if err != nil {
		return err
	}

	return nil
}

func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		return nil, fs.ErrorCantMove
	}

	srcDir := path.Dir(path.Join(srcObj.fs.root, srcObj.remote))
	dstDir := path.Dir(path.Join(f.root, remote))

	if srcDir == "." {
		srcDir = ""
	}
	if dstDir == "." {
		dstDir = ""
	}

	if srcDir != dstDir {
		parentID, err := f.resolveAbsoluteFolderID(ctx, dstDir)
		if err != nil {
			return nil, err
		}

		if parentID == 0 {
			return nil, errors.New("moving files to root folder is not supported")
		}

		if err := f.moveDocument(ctx, srcObj.id, parentID); err != nil {
			return nil, err
		}
	}

	srcName := path.Base(srcObj.remote)
	dstName := path.Base(remote)

	if srcName != dstName {
		if err := f.renameDocument(ctx, srcObj.id, dstName); err != nil {
			return nil, err
		}
	}

	srcObj.fs = f
	srcObj.remote = remote

	return srcObj, nil
}

func (f *Fs) moveDocument(ctx context.Context, id, parentID int64) error {
	req := map[string]int64{
		"parent": parentID,
	}

	var result DocumentResponse

	opts := rest.Opts{
		Method: http.MethodPost,
		Path:   fmt.Sprintf("documents/%d/move", id),
	}

	_, err := f.srv.CallJSON(ctx, &opts, &req, &result)
	return err
}

func (f *Fs) renameDocument(ctx context.Context, id int64, title string) error {
	req := map[string]string{
		"title": title,
	}

	var result DocumentResponse

	opts := rest.Opts{
		Method: http.MethodPost,
		Path:   fmt.Sprintf("documents/%d/rename", id),
	}

	_, err := f.srv.CallJSON(ctx, &opts, &req, &result)
	return err
}

func (f *Fs) Features() *fs.Features { return f.features }
func (f *Fs) Hashes() hash.Set       { return hash.Set(hash.None) }

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
	remote := src.Remote()
	parentPath := path.Dir(remote)
	if parentPath == "." {
		parentPath = ""
	}

	parentID, err := f.resolveFolderID(ctx, parentPath)
	if err != nil {
		return nil, err
	}

	name := path.Base(remote)
	params := url.Values{
		"FolderId": []string{strconv.FormatInt(parentID, 10)},
		"Name":     []string{name},
	}

	contentType := mime.TypeByExtension(path.Ext(name))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	opts := rest.Opts{
		Method:               http.MethodPost,
		Path:                 "documents",
		Body:                 in,
		Options:              options,
		MultipartParams:      params,
		MultipartContentName: "file",
		MultipartFileName:    name,
		MultipartContentType: contentType,
	}

	if size := src.Size(); size >= 0 {
		opts.ContentLength = &size
	}

	var result DocumentResponse
	_, err = f.srv.CallJSON(ctx, &opts, nil, &result)
	if err != nil {
		return nil, err
	}

	return &Object{
		fs:      f,
		remote:  remote,
		id:      result.ID,
		size:    src.Size(),
		modTime: src.ModTime(ctx),
		loaded:  false,
	}, nil
}

type TrashResponse struct {
	Items  []DocumentResponse `json:"items"`
	Cursor string             `json:"cursor"`
}

func (f *Fs) listTrash(ctx context.Context) (fs.DirEntries, error) {
	params := url.Values{
		"parameters.limit":   {"100"},
		"parameters.parent":  {"-1"},
		"parameters.type":    {"documents"},
		"parameters.deleted": {"true"},
	}

	var result TrashResponse

	opts := rest.Opts{
		Method:     http.MethodGet,
		Path:       "folders/items",
		Parameters: params,
	}

	_, err := f.srv.CallJSON(ctx, &opts, nil, &result)
	if err != nil {
		return nil, err
	}

	entries := make(fs.DirEntries, 0, len(result.Items))
	for _, doc := range result.Items {
		if doc.FileName == "" {
			doc.FileName = doc.Name
		}
		if doc.FileName == "" {
			return nil, fmt.Errorf("trashed document %d has no name", doc.ID)
		}

		entries = append(entries, &Object{
			fs:      f,
			remote:  doc.FileName,
			id:      doc.ID,
			size:    parseSize(doc.FileSize),
			modTime: time.Now(),
			loaded:  false,
		})
	}

	return entries, nil
}

func (f *Fs) restoreDocument(ctx context.Context, id int64) error {
	var result DocumentResponse
	contentLength := int64(0)

	opts := rest.Opts{
		Method:        http.MethodPost,
		Path:          fmt.Sprintf("documents/%d/restore", id),
		ContentLength: &contentLength,
		ExtraHeaders: map[string]string{
			"Cache-Control": "no-cache",
			"Pragma":        "no-cache",
			"Referer":       "https://buzondigital.correos.es/trash",
		},
	}

	_, err := f.srv.CallJSON(ctx, &opts, nil, &result)
	return err
}

func (f *Fs) deleteDocumentPermanently(ctx context.Context, id int64) error {
	opts := rest.Opts{
		Method: http.MethodDelete,
		Path:   fmt.Sprintf("documents/%d/permanently", id),
		ExtraHeaders: map[string]string{
			"Cache-Control": "no-cache",
			"Pragma":        "no-cache",
			"Referer":       "https://buzondigital.correos.es/trash",
		},
	}

	resp, err := f.srv.Call(ctx, &opts)
	if err != nil {
		return err
	}
	defer fs.CheckClose(resp.Body, &err)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("permanent delete failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

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

func (o *Object) ID() string                            { return strconv.FormatInt(o.id, 10) }
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

func (o *Object) Remove(ctx context.Context) error {
	if o.fs.opt.TrashedOnly {
		return o.fs.deleteDocumentPermanently(ctx, o.id)
	}

	var result DocumentResponse

	opts := rest.Opts{
		Method: http.MethodDelete,
		Path:   fmt.Sprintf("documents/%d", o.id),
	}

	_, err := o.fs.srv.CallJSON(ctx, &opts, nil, &result)
	return err
}

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
