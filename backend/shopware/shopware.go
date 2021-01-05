package shopware

import (
	bytebytes "bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/h2non/filetype"
	"github.com/pkg/errors"
	"github.com/rclone/rclone/backend/shopware/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	minSleep      = 10 * time.Millisecond
	maxSleep      = 2 * time.Second
	decayConstant = 2
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "shopware",
		Description: "Use your Shopware Media Manager as Filesystem",
		NewFs:       NewFs,
		Options: []fs.Option{
			{
				Name: "url",
				Help: "URL to Shop (e.G https://my-shop.com)",
			},
			{
				Name: "client_id",
				Help: "Client ID from a Integration",
			},
			{
				Name: "client_secret",
				Help: "Client Secret from a Integration",
			},
		},
	})
}

var retryErrorCodes = []int{
	429, // Too Many Requests.
	500, // Internal Server Error
	502, // Bad Gateway
	503, // Service Unavailable
	504, // Gateway Timeout
	509, // Bandwidth Limit Exceeded
}

type Options struct {
	ShopURL      string `config:"url"`
	ClientID     string `config:"client_id"`
	ClientSecret string `config:"client_secret"`
}

type Fs struct {
	name     string
	root     string
	features *fs.Features
	srv      *rest.Client
	dirCache *dircache.DirCache
	pacer    *fs.Pacer
}

type Object struct {
	fs          *Fs
	name        string
	remote      string
	hasMetaData bool
	size        int64
	Type        string
	URL         string
	modTime     time.Time
	id          string
}

func (o Object) String() string {
	return o.name
}

func (o Object) Remote() string {
	return o.remote
}

func (o Object) ModTime(ctx context.Context) time.Time {
	return o.modTime
}

func (o Object) Size() int64 {
	return o.size
}

func (o Object) Fs() fs.Info {
	return o.fs
}

func (o Object) Hash(ctx context.Context, ty hash.Type) (string, error) {
	return "", hash.ErrUnsupported
}

func (o Object) Storable() bool {
	return true
}

func (o Object) SetModTime(ctx context.Context, t time.Time) error {
	o.modTime = t
	return nil
}

func (o Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	resp, err := http.Get(o.URL)
	if err != nil {
		return nil, err
	}

	return resp.Body, nil
}

func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	extension := path.Ext(o.name)

	kind := filetype.GetType(o.name)

	opts := rest.Opts{
		Method:       "POST",
		Path:         fmt.Sprintf("/api/v3/_action/media/%s/upload?extension=%s&fileName=%s", o.id, extension[1:], o.name[0:len(o.name)-len(extension)]),
		ExtraHeaders: map[string]string{"Accept": "application/json", "Content-Type": kind.MIME.Value},
		Body:         in,
	}

	err := o.fs.pacer.Call(func() (bool, error) {
		resp, err := o.fs.srv.Call(ctx, &opts)

		if resp.StatusCode == http.StatusBadRequest {
			return false, fmt.Errorf("Shopware does not allow this file extension")
		}

		return shouldRetry(resp, err)
	})

	if err != nil {
		return err
	}

	file, err := o.fs.findFileById(ctx, o.id)

	if err != nil {
		return err
	}

	o.size = int64(file.FileSize)
	o.modTime = o.fs.parseShopwareDate(file.UploadedAt)
	o.URL = file.URL

	return nil
}

func (o Object) Remove(ctx context.Context) error {
	opts := rest.Opts{
		Method:       "DELETE",
		Path:         fmt.Sprintf("/api/v3/media/%s", o.id),
		ExtraHeaders: map[string]string{"Accept": "application/json", "Content-Type": "application/json"},
	}

	return o.fs.pacer.Call(func() (bool, error) {
		resp, err := o.fs.srv.Call(ctx, &opts)

		if resp.StatusCode == http.StatusBadRequest {
			err = fmt.Errorf("Shopware does not allow this file extension")
		}

		return shouldRetry(resp, err)
	})
}

func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	leaf, dirId, err := f.dirCache.FindPath(ctx, filepath.Join(f.root, remote), false)
	if err != nil {
		return nil, err
	}

	file, err := f.findFileByName(ctx, dirId, leaf)

	if err != nil {
		return nil, err
	}

	if file == nil {
		return nil, fs.ErrorObjectNotFound
	}

	o := &Object{
		id:      file.ID,
		name:    leaf,
		remote:  filepath.Join(f.root, remote),
		size:    int64(file.FileSize),
		URL:     file.URL,
		modTime: f.parseShopwareDate(file.UploadedAt),
		fs:      f,
	}

	return o, nil
}

func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	existingObj, err := f.NewObject(ctx, src.Remote())
	switch err {
	case nil:
		return existingObj, existingObj.Update(ctx, in, src, options...)
	case fs.ErrorObjectNotFound:
		leaf, dirId, err := f.dirCache.FindPath(ctx, filepath.Join(f.root, src.Remote()), false)
		if err != nil {
			return nil, err
		}

		file := api.MediaItem{
			ID:           strings.ReplaceAll(uuid.New().String(), "-", ""),
			CustomFields: map[string]string{"FileName": leaf},
			FolderId: dirId,
		}

		if dirId == "root" {
			file.FolderId = nil
		}

		extension := path.Ext(leaf)

		bodyJson, err := json.Marshal(file)
		if err != nil {
			return nil, err
		}

		opts := rest.Opts{
			Method:       "POST",
			Path:         "/api/v3/media",
			ExtraHeaders: map[string]string{"Accept": "application/json", "Content-Type": "application/json"},
			Body:         strings.NewReader(string(bodyJson)),
		}

		err = f.pacer.Call(func() (bool, error) {
			resp, err := f.srv.Call(ctx, &opts)
			return shouldRetry(resp, err)
		})

		if err != nil {
			return nil, err
		}

		kind := filetype.GetType(extension[1:])

		opts = rest.Opts{
			Method:       "POST",
			Path:         fmt.Sprintf("/api/v3/_action/media/%s/upload?extension=%s&fileName=%s", file.ID, extension[1:], leaf[0:len(leaf)-len(extension)]),
			ExtraHeaders: map[string]string{"Accept": "application/json", "Content-Type": kind.MIME.Value},
			Body:         in,
		}

		err = f.pacer.Call(func() (bool, error) {
			resp, err := f.srv.Call(ctx, &opts)

			if resp.StatusCode == http.StatusBadRequest {
				return false, fmt.Errorf("Shopware does not allow this file extension")
			}

			return shouldRetry(resp, err)
		})

		if err != nil {
			return nil, err
		}

		updatedFile, err := f.findFileById(ctx, file.ID)

		if err != nil {
			return nil, err
		}

		o := &Object{
			fs:      f,
			name:    fmt.Sprintf("%s.%s", updatedFile.FileName, updatedFile.FileExtension),
			id:      updatedFile.ID,
			size:    int64(updatedFile.FileSize),
			URL:     updatedFile.URL,
			modTime: f.parseShopwareDate(updatedFile.UploadedAt),
			remote:  filepath.Join(f.root, src.Remote()),
		}

		return o, nil
	default:
		return nil, err
	}
}

func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	_, err := f.dirCache.FindDir(ctx, dir, true)
	return err
}

func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	id, err := f.dirCache.FindDir(ctx, dir, false)

	if err != nil {
		return err
	}

	opts := rest.Opts{
		Method:       "DELETE",
		Path:         fmt.Sprintf("/api/v3/media-folder/%s", id),
		ExtraHeaders: map[string]string{"Accept": "application/json", "Content-Type": "application/json"},
	}

	return f.pacer.Call(func() (bool, error) {
		resp, err := f.srv.Call(ctx, &opts)
		return shouldRetry(resp, err)
	})
}

func (f *Fs) FindLeaf(ctx context.Context, pathID, leaf string) (pathIDOut string, found bool, err error) {
	if leaf == f.root && pathID != "root" {
		return pathID, true, nil
	}

	pathIDOut, err = f.findFolderByName(ctx, pathID, leaf)
	if err != nil {
		return "", false, err
	}

	if len(pathIDOut) == 0 {
		return "", false, nil
	}

	return pathIDOut, true, nil
}

func (f *Fs) Name() string {
	return f.name
}

func (f *Fs) Root() string {
	return f.root
}

func (f *Fs) String() string {
	return fmt.Sprintf("shopware root '%s'", f.root)
}

func (f *Fs) Precision() time.Duration {
	return fs.ModTimeNotSupported
}

func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.None)
}

func (f *Fs) Features() *fs.Features {
	return f.features
}

func (f *Fs) splitPath(remote string) (directory, leaf string) {
	directory, leaf = dircache.SplitPath(remote)
	if f.root != "" {
		// Adds the root folder to the path to get a full path
		directory = path.Join(f.root, directory)
	}
	return
}

func shouldRetry(resp *http.Response, err error) (bool, error) {
	if resp.StatusCode == 204 {
		return false, nil
	}

	authRetry := false

	if resp != nil && resp.StatusCode == 401 {
		authRetry = true
		fs.Debugf(nil, "Should retry: %v", err)
	}
	return authRetry || fserrors.ShouldRetry(err) || fserrors.ShouldRetryHTTP(resp, retryErrorCodes), err
}

func (f *Fs) readMetaDataForID(ctx context.Context, id string) (*api.MediaItem, error) {
	opts := rest.Opts{
		Method:       "GET",
		Path:         "/api/v3/media/" + id,
		ExtraHeaders: map[string]string{"Accept": "application/json", "Content-Type": "application/json"},
		Parameters:   url.Values{},
	}
	var result *api.MediaDetailResponse
	var resp *http.Response
	var err error
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &result)
		return shouldRetry(resp, err)
	})
	if err != nil {
		return nil, err
	}
	return &result.Data, nil
}

func (f *Fs) CreateDir(ctx context.Context, pathID, leaf string) (newID string, err error) {
	folder := api.MediaFolderItem{
		Name:          leaf,
		ID:            strings.ReplaceAll(uuid.New().String(), "-", ""),
		Configuration: api.MediaFolderConfiguration{Private: false},
	}

	if pathID != "root" {
		folder.ParentId = pathID
	}

	jsonString, err := json.Marshal(folder)

	if err != nil {
		return "", err
	}

	opts := rest.Opts{
		Method:       "POST",
		Path:         "/api/v3/media-folder",
		ExtraHeaders: map[string]string{"Accept": "application/json", "Content-Type": "application/json"},
		Body: bytebytes.NewReader(jsonString),
	}

	err = f.pacer.Call(func() (bool, error) {
		resp, err := f.srv.Call(ctx, &opts)
		return shouldRetry(resp, err)
	})

	if err != nil {
		return "", err
	}

	return folder.ID, nil
}

func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't move - not same remote type")
		return nil, fs.ErrorCantMove
	}

	folderPath := filepath.Dir(remote)
	var dirId interface{}
	var err error
	if folderPath != "." {
		dirId, err = f.dirCache.FindDir(ctx, folderPath, false)
	} else {
		dirId = nil
	}

	if err != nil {
		fs.Debugf(src, "Cannot find target folder")
		return nil, fs.ErrorCantMove
	}


	fileName := filepath.Base(remote)

	oldExtension := path.Ext(srcObj.name)
	extension := path.Ext(fileName)
	fileNameWithoutExtension := fileName[0 : len(fileName)-len(extension)]

	jsonBody, _ := json.Marshal(map[string]string{"fileName": fileNameWithoutExtension})

	// Update filename
	opts := rest.Opts{
		Method:       "POST",
		Path:         fmt.Sprintf("/api/v3/_action/media/%s/rename", srcObj.id),
		ExtraHeaders: map[string]string{"Accept": "application/json", "Content-Type": "application/json"},
		Body: bytebytes.NewReader(jsonBody),
	}

	err = f.pacer.Call(func() (bool, error) {
		resp, err := f.srv.Call(ctx, &opts)
		return shouldRetry(resp, err)
	})

	// Update parent folder

	jsonBody, _ = json.Marshal(api.MediaItem{FolderId: dirId})
	opts = rest.Opts{
		Method:       "PATCH",
		Path:         fmt.Sprintf("/api/v3/media/%s", srcObj.id),
		ExtraHeaders: map[string]string{"Accept": "application/json", "Content-Type": "application/json"},
		Body: bytebytes.NewReader(jsonBody),
	}

	err = f.pacer.Call(func() (bool, error) {
		resp, err := f.srv.Call(ctx, &opts)
		return shouldRetry(resp, err)
	})

	srcObj.name = fmt.Sprintf("%s.%s", fileNameWithoutExtension, oldExtension[1:])
	srcObj.remote = remote
	srcObj.modTime = time.Now()

	return srcObj, err
}

func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	srcFs, ok := src.(*Fs)
	if !ok {
		fs.Debugf(srcFs, "Can't move directory - not same remote type")
		return fs.ErrorCantDirMove
	}

	srcID, _, _, dstDirectoryID, dstLeaf, err := f.dirCache.DirMove(ctx, srcFs.dirCache, srcFs.root, srcRemote, f.root, dstRemote)
	if err != nil {
		return err
	}

	updatedFolder := api.MediaFolderItem{
		Name: dstLeaf,
		ParentId: dstDirectoryID,
	}

	if dstDirectoryID == "root" {
		updatedFolder.ParentId = nil
	}

	jsonString, _ := json.Marshal(updatedFolder)

	opts := rest.Opts{
		Method:       "PATCH",
		Path:         fmt.Sprintf("/api/v3/media-folder/%s", srcID),
		ExtraHeaders: map[string]string{"Accept": "application/json", "Content-Type": "application/json"},
		Body: bytebytes.NewReader(jsonString),
	}

	err = f.pacer.Call(func() (bool, error) {
		resp, err := f.srv.Call(ctx, &opts)
		return shouldRetry(resp, err)
	})

	srcFs.dirCache.FlushDir(srcRemote)
	return nil
}

func (f *Fs) parseShopwareDate(date string) time.Time {
	if date == "" {
		return time.Now()
	}

	time, err := time.Parse(time.RFC3339, date)

	if err != nil {
		log.Println(err)
	}

	return time
}

func (f *Fs) findFileByName(ctx context.Context, parentId string, name string) (*api.MediaItem, error) {
	filter := api.Search{}
	filter.Includes = make(map[string][]string)
	filter.Includes["media"] = []string{"id", "fileName", "fileExtension", "fileSize", "mediaFolderId", "url", "uploadedAt"}

	extension := path.Ext(name)
	fileName := name[0 : len(name)-len(extension)]

	filter.Filter = []api.SearchFilter{
		{
			Type:     "multi",
			Operator: "or",
			Queries: []api.SearchFilter{
				{
					Type:     "multi",
					Operator: "and",
					Queries: []api.SearchFilter{
						{Type: "equals", Field: "fileName", Value: fileName},
						{Type: "equals", Field: "fileExtension", Value: extension[1:]},
					},
				},
				{
					Type:  "equals",
					Field: "customFields.FileName",
					Value: name,
				},
			},
		},
	}

	if parentId == "root" {
		filter.Filter = append(filter.Filter, api.SearchFilter{Type: "equals", Field: "mediaFolderId", Value: nil})
	} else {
		filter.Filter = append(filter.Filter, api.SearchFilter{Type: "equals", Field: "mediaFolderId", Value: parentId})
	}

	bodyJson, err := json.Marshal(filter)

	if err != nil {
		return nil, err
	}

	opts := rest.Opts{
		Method:       "POST",
		Path:         "/api/v3/search/media",
		ExtraHeaders: map[string]string{"Accept": "application/json", "Content-Type": "application/json"},
		Body:         strings.NewReader(string(bodyJson)),
	}

	var result api.MediaListResponse
	var resp *http.Response

	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &result)
		return shouldRetry(resp, err)
	})

	if err != nil {
		return nil, errors.Wrap(err, fmt.Sprintf("couldn't list file by name %s", name))
	}

	for _, file := range result.Data {
		return &file, nil
	}

	return nil, nil
}

func (f *Fs) findFileById(ctx context.Context, id string) (*api.MediaItem, error) {
	filter := api.Search{}
	filter.Includes = make(map[string][]string)
	filter.Includes["media"] = []string{"id", "fileName", "fileExtension", "fileSize", "mediaFolderId", "url", "uploadedAt"}

	filter.IDs = []string{id}

	bodyJson, err := json.Marshal(filter)

	if err != nil {
		return nil, err
	}

	opts := rest.Opts{
		Method:       "POST",
		Path:         "/api/v3/search/media",
		ExtraHeaders: map[string]string{"Accept": "application/json", "Content-Type": "application/json"},
		Body:         strings.NewReader(string(bodyJson)),
	}

	var result api.MediaListResponse
	var resp *http.Response

	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &result)
		return shouldRetry(resp, err)
	})

	if err != nil {
		return nil, errors.Wrap(err, fmt.Sprintf("couldn't get file by id %s", id))
	}

	for _, file := range result.Data {
		return &file, nil
	}

	return nil, nil
}

func (f *Fs) listFilesInFolder(ctx context.Context, parentId string, remote string) ([]fs.Object, error) {
	filter := api.Search{}
	filter.Includes = make(map[string][]string)
	filter.Includes["media"] = []string{"id", "fileName", "fileExtension", "fileSize", "mediaFolderId", "url", "uploadedAt"}

	if parentId == "root" || parentId == "" {
		filter.Filter = []api.SearchFilter{{Type: "equals", Field: "mediaFolderId", Value: nil}}
	} else {
		filter.Filter = []api.SearchFilter{{Type: "equals", Field: "mediaFolderId", Value: parentId}}
	}

	bodyJson, err := json.Marshal(filter)

	if err != nil {
		return nil, err
	}

	opts := rest.Opts{
		Method:       "POST",
		Path:         "/api/v3/search/media",
		ExtraHeaders: map[string]string{"Accept": "application/json", "Content-Type": "application/json"},
		Body:         strings.NewReader(string(bodyJson)),
	}

	var result api.MediaListResponse
	var resp *http.Response

	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &result)
		return shouldRetry(resp, err)
	})

	if err != nil {
		return nil, errors.Wrap(err, "couldn't list files")
	}

	var files = make([]fs.Object, 0)

	for _, file := range result.Data {
		o := &Object{
			fs:      f,
			name:    fmt.Sprintf("%s.%s", file.FileName, file.FileExtension),
			id:      file.ID,
			size:    int64(file.FileSize),
			Type:    "file",
			URL:     file.URL,
			modTime: f.parseShopwareDate(file.UploadedAt),
			remote:  path.Join(remote, fmt.Sprintf("%s.%s", file.FileName, file.FileExtension)),
		}

		files = append(files, o)
	}

	return files, nil
}

func (f *Fs) findFolderByName(ctx context.Context, parentId string, name string) (string, error) {
	filter := api.Search{}
	filter.Includes = make(map[string][]string)
	filter.Includes["media-folder"] = []string{"id", "name", "parentId"}

	if parentId == "root" {
		filter.Filter = []api.SearchFilter{{Type: "equals", Field: "parentId", Value: nil}, {Type: "equals", Field: "name", Value: name}}
	} else {
		filter.Filter = []api.SearchFilter{{Type: "equals", Field: "parentId", Value: parentId}, {Type: "equals", Field: "name", Value: name}}
	}

	bodyJson, err := json.Marshal(filter)
	if err != nil {
		return "", err
	}

	opts := rest.Opts{
		Method:       "POST",
		Path:         "/api/v3/search-ids/media-folder",
		ExtraHeaders: map[string]string{"Accept": "application/json", "Content-Type": "application/json"},
		Body:         bytebytes.NewReader(bodyJson),
	}

	var result api.SearchIdResponse
	err = f.pacer.Call(func() (bool, error) {
		resp, err := f.srv.CallJSON(ctx, &opts, nil, &result)
		return shouldRetry(resp, err)
	})

	if err != nil {
		return "", errors.Wrap(err, fmt.Sprintf("Could not list folder by name %s", name))
	}

	for _, id := range result.Data {
		return id, nil
	}

	return "", nil
}

func (f *Fs) listFoldersInFolder(ctx context.Context, parentId string, remote string) ([]*Object, error) {
	filter := api.Search{}
	filter.Includes = make(map[string][]string)
	filter.Includes["media-folder"] = []string{"id", "name", "parentId", "created_at"}

	if parentId == "root" || parentId == "" {
		filter.Filter = []api.SearchFilter{{Type: "equals", Field: "parentId", Value: nil}}
	} else {
		filter.Filter = []api.SearchFilter{{Type: "equals", Field: "parentId", Value: parentId}}
	}

	bodyJson, err := json.Marshal(filter)

	if err != nil {
		return nil, err
	}

	opts := rest.Opts{
		Method:       "POST",
		Path:         "/api/v3/search/media-folder",
		ExtraHeaders: map[string]string{"Accept": "application/json", "Content-Type": "application/json"},
		Body:         strings.NewReader(string(bodyJson)),
	}

	var result api.MediaFolderListResponse
	var resp *http.Response

	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &result)
		return shouldRetry(resp, err)
	})

	if err != nil {
		return nil, errors.Wrap(err, "couldn't list folders")
	}

	var folders = make([]*Object, 0)

	for _, file := range result.Data {
		o := &Object{
			fs:      f,
			name:    file.Name,
			id:      file.ID,
			size:    0,
			Type:    "folder",
			modTime: f.parseShopwareDate(file.CreatedAt),
			remote:  path.Join(remote, file.Name),
		}

		folders = append(folders, o)
	}

	return folders, nil
}

func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	directoryID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return nil, err
	}

	files, err := f.listFilesInFolder(ctx, directoryID, dir)

	if err != nil {
		return nil, err
	}

	folders, err := f.listFoldersInFolder(ctx, directoryID, dir)

	if err != nil {
		return nil, err
	}

	for _, file := range files {
		entries = append(entries, file)
	}

	for _, folder := range folders {
		f.dirCache.Put(folder.remote, folder.id)
		d := fs.NewDir(folder.remote, folder.modTime).SetID(folder.id)
		entries = append(entries, d)
	}

	return entries, nil
}

func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}

	config := clientcredentials.Config{
		ClientID:     opt.ClientID,
		ClientSecret: opt.ClientSecret,
		TokenURL:     fmt.Sprintf("%s/api/oauth/token", opt.ShopURL),
		AuthStyle:    oauth2.AuthStyleInParams,
	}

	client := config.Client(context.Background())

	f := &Fs{
		name:  name,
		root:  root,
		srv:   rest.NewClient(client).SetRoot(opt.ShopURL),
		pacer: fs.NewPacer(ctx, pacer.NewDefault(pacer.MinSleep(minSleep), pacer.MaxSleep(maxSleep), pacer.DecayConstant(decayConstant))),
	}

	f.features = (&fs.Features{
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)

	f.dirCache = dircache.New(root, "root", f)

	// Find the current root
	err := f.dirCache.FindRoot(ctx, false)
	if err != nil {
		// Assume it is a file
		newRoot, remote := dircache.SplitPath(root)
		tempF := *f
		tempF.dirCache = dircache.New(newRoot, "root", &tempF)
		tempF.root = newRoot
		// Make new Fs which is the parent
		err = tempF.dirCache.FindRoot(ctx, false)
		if err != nil {
			// No root so return old f
			return f, nil
		}
		_, err := tempF.NewObject(ctx, remote)
		if err != nil {
			if err == fs.ErrorObjectNotFound {
				// File doesn't exist so return old f
				return f, nil
			}
			return nil, err
		}
		f.features.Fill(ctx, &tempF)
		// XXX: update the old f here instead of returning tempF, since
		// `features` were already filled with functions having *f as a receiver.
		// See https://github.com/rclone/rclone/issues/2182
		f.dirCache = tempF.dirCache
		f.root = tempF.root
		// return an error with an fs which points to the parent
		return f, fs.ErrorIsFile
	}

	return f, nil
}
