package dash

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/sawka/dashborg-go-sdk/pkg/dasherr"
	"github.com/sawka/dashborg-go-sdk/pkg/dashutil"
)

const (
	MimeTypeDashborgHtml = "text/x-dashborg-html"
	MimeTypeHtml         = "text/html"
	MimeTypeJson         = "application/json"
	MimeTypeDashborgApp  = "application/x-dashborg+json"
)

const (
	FileTypeStatic         = "static"
	FileTypeRuntimeLink    = "rt-link"
	FileTypeAppRuntimeLink = "rt-app-link"
	FileTypeDir            = "dir"
	FileTypeApp            = "app"
)

type FileInfo struct {
	ParentDir     string   `json:"parentdir"`
	FileName      string   `json:"filename"`
	Path          string   `json:"path"`
	Size          int64    `json:"size"`
	CreatedTs     int64    `json:"createdts"`
	UpdatedTs     int64    `json:"updatedts"`
	Sha256        string   `json:"sha256"`
	FileType      string   `json:"filetype"`
	MimeType      string   `json:"mimetype"`
	AllowedRoles  []string `json:"allowedroles"`
	EditRoles     []string `json:"editroles"`
	Display       string   `json:"display,omitempty"`
	MetadataJson  string   `json:'metadata,omitempty"` // json-string
	Description   string   `json:"description,omitempty"`
	Hidden        bool     `json:"hidden,omitempty"`
	Removed       bool     `json:"removed,omitempty"`
	ProcLinks     []string `json:"proclinks,omitempty"`
	TxId          string   `json:"txid,omitempty"`
	AppConfigJson string   `json:"appconfig"` // json-string
}

func (finfo *FileInfo) BindMetadata(obj interface{}) error {
	return json.Unmarshal([]byte(finfo.MetadataJson), obj)
}

func (finfo *FileInfo) IsLinkType() bool {
	return finfo.FileType == FileTypeRuntimeLink || finfo.FileType == FileTypeAppRuntimeLink
}

type BlobReturn struct {
	Reader   io.Reader
	MimeType string
}

type FileOpts struct {
	FileType      string   `json:"filetype"`
	Sha256        string   `json:"sha256"`
	Size          int64    `json:"size"`
	MimeType      string   `json:"mimetype"`
	AllowedRoles  []string `json:"allowedroles,omitempty"`
	EditRoles     []string `json:"editroles,omitempty"`
	Display       string   `json:"display,omitempty"`
	MetadataJson  string   `json:"metadata,omitempty"`
	Description   string   `json:"description,omitempty"`
	NoMkDirs      bool     `json:"nomkdirs,omitempty"`
	Hidden        bool     `json:"hidden,omitempty"`
	AppConfigJson string   `json:"appconfig"` // json-string
}

func (opts *FileOpts) SetMetadata(obj interface{}) error {
	metaStr, err := dashutil.MarshalJson(obj)
	if err != nil {
		return err
	}
	if len(metaStr) > dashutil.MetadataJsonMax {
		return dasherr.ValidateErr(fmt.Errorf("Metadata too large"))
	}
	opts.MetadataJson = metaStr
	return nil
}

func (opts *FileOpts) IsLinkType() bool {
	return opts.FileType == FileTypeRuntimeLink || opts.FileType == FileTypeAppRuntimeLink
}

type DirOpts struct {
	RoleList   []string `json:"rolelist"`
	ShowHidden bool     `json:"showhidden"`
	Recursive  bool     `json:"recursive"`
}

type WatchOpts struct {
	ThrottleTime time.Duration
	ShutdownCh   chan struct{}
}

type DashFSClient struct {
	rootPath string
	client   *DashCloudClient
}

func (fs *DashFSClient) SetRawPath(path string, r io.Reader, fileOpts *FileOpts, runtime LinkRuntime) error {
	if path == "" || path[0] != '/' {
		return dasherr.ValidateErr(fmt.Errorf("Path must begin with '/'"))
	}
	return fs.client.setRawPath(fs.rootPath+path, r, fileOpts, runtime)
}

func (fs *DashFSClient) SetJsonPath(path string, data interface{}, fileOpts *FileOpts) error {
	var jsonBuf bytes.Buffer
	enc := json.NewEncoder(&jsonBuf)
	enc.SetEscapeHTML(false)
	err := enc.Encode(data)
	if err != nil {
		return dasherr.JsonMarshalErr("JsonData", err)
	}
	reader := bytes.NewReader(jsonBuf.Bytes())
	if fileOpts == nil {
		fileOpts = &FileOpts{}
	}
	err = UpdateFileOptsFromReadSeeker(reader, fileOpts)
	if err != nil {
		return err
	}
	if fileOpts.MimeType == "" {
		fileOpts.MimeType = MimeTypeJson
	}
	return fs.SetRawPath(path, reader, fileOpts, nil)
}

func (fs *DashFSClient) SetPathFromFile(path string, fileName string, fileOpts *FileOpts) error {
	fd, err := os.Open(fileName)
	if err != nil {
		return err
	}
	err = UpdateFileOptsFromReadSeeker(fd, fileOpts)
	if err != nil {
		return err
	}
	return fs.SetRawPath(path, fd, fileOpts, nil)
}

// Will call Seek(0, 0) on the reader twice, once at the beginning and once at the end.
// If an error is returned, the seek position is not specified.  If no error is returned
// the reader will be reset to the beginning.
// A []byte can be wrapped in a bytes.Buffer to use this function (error will always be nil)
func UpdateFileOptsFromReadSeeker(r io.ReadSeeker, fileOpts *FileOpts) error {
	if fileOpts == nil {
		return dasherr.ValidateErr(fmt.Errorf("Must pass non-nil FileOpts (set at least MimeType)"))
	}
	_, err := r.Seek(0, 0)
	if err != nil {
		return err
	}
	h := sha256.New()
	numCopyBytes, err := io.Copy(h, r)
	if err != nil {
		return err
	}
	hashVal := h.Sum(nil)
	hashValStr := base64.StdEncoding.EncodeToString(hashVal[:])
	_, err = r.Seek(0, 0)
	if err != nil {
		return err
	}
	fileOpts.FileType = FileTypeStatic
	fileOpts.Sha256 = hashValStr
	fileOpts.Size = numCopyBytes
	return nil
}

func (fs *DashFSClient) runWatchedSetPath(path string, fileName string, fileOpts *FileOpts) {
	err := fs.SetPathFromFile(path, fileName, fileOpts)
	if err != nil {
		log.Printf("Error calling SetPathFromFile (watched file) path=%s file=%s err=%v\n", dashutil.SimplifyPath(path, nil), fileName, err)
	} else {
		log.Printf("Watcher called SetPathFromFile path=%s file=%s size=%d hash=%s\n", dashutil.SimplifyPath(path, nil), fileName, fileOpts.Size, fileOpts.Sha256)
	}
}

func (fs *DashFSClient) WatchFile(path string, fileName string, fileOpts *FileOpts, watchOpts *WatchOpts) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if fileOpts == nil {
		fileOpts = &FileOpts{}
	}
	if watchOpts == nil {
		watchOpts = &WatchOpts{ThrottleTime: time.Second}
	}
	err = fs.SetPathFromFile(path, fileName, fileOpts)
	if err != nil {
		return err
	}
	err = watcher.Add(fileName)
	if err != nil {
		return err
	}
	go func() {
		var needsRun bool
		lastRun := time.Now()
		defer watcher.Close()
		var timer *time.Timer
		for {
			var timerCh <-chan time.Time
			if timer != nil {
				timerCh = timer.C
			}
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op == fsnotify.Write || event.Op == fsnotify.Create {
					dur := time.Since(lastRun)
					if dur < watchOpts.ThrottleTime {
						needsRun = true
						if timer == nil {
							timer = time.NewTimer(watchOpts.ThrottleTime - dur)
						}
					} else {
						needsRun = false
						fs.runWatchedSetPath(path, fileName, fileOpts)
						lastRun = time.Now()
					}
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("DashFS Watch Error path=%s file=%s err=%v\n", dashutil.SimplifyPath(path, nil), fileName, err)
				return

			case <-timerCh:
				if needsRun {
					timer = nil
					needsRun = false
					fs.runWatchedSetPath(path, fileName, fileOpts)
					lastRun = time.Now()
				}

			case <-watchOpts.ShutdownCh:
				return
			}
		}
	}()
	return nil
}

func (fs *DashFSClient) RemovePath(path string) error {
	if path == "" || path[0] != '/' {
		return fmt.Errorf("Path must begin with '/'")
	}
	return fs.client.removePath(fs.rootPath + path)
}

func (fs *DashFSClient) FileInfo(path string) (*FileInfo, error) {
	if path == "" || path[0] != '/' {
		return nil, fmt.Errorf("Path must begin with '/'")
	}
	rtn, _, err := fs.client.fileInfo(fs.rootPath+path, nil, false)
	if err != nil {
		return nil, err
	}
	if len(rtn) == 0 {
		return nil, nil
	}
	return rtn[0], nil
}

func (fs *DashFSClient) DirInfo(path string, dirOpts *DirOpts) ([]*FileInfo, error) {
	if dirOpts == nil {
		dirOpts = &DirOpts{}
	}
	if path == "" || path[0] != '/' {
		return nil, fmt.Errorf("Path must begin with '/'")
	}
	rtn, _, err := fs.client.fileInfo(fs.rootPath+path, dirOpts, false)
	return rtn, err
}

func (fs *DashFSClient) LinkRuntime(path string, rt LinkRuntime, fileOpts *FileOpts) error {
	if hasErr, ok := rt.(HasErr); ok {
		err := hasErr.Err()
		if err != nil {
			return err
		}
	}
	if fileOpts == nil {
		fileOpts = &FileOpts{}
	}
	fileOpts.FileType = FileTypeRuntimeLink
	if path == "" || path[0] != '/' {
		return fmt.Errorf("Path must begin with '/'")
	}
	return fs.client.setRawPath(fs.rootPath+path, nil, fileOpts, rt)
}

func (fs *DashFSClient) LinkAppRuntime(path string, apprt LinkRuntime, fileOpts *FileOpts) error {
	if hasErr, ok := apprt.(HasErr); ok {
		err := hasErr.Err()
		if err != nil {
			return err
		}
	}
	if fileOpts == nil {
		fileOpts = &FileOpts{}
	}
	fileOpts.FileType = FileTypeAppRuntimeLink
	if path == "" || path[0] != '/' {
		return fmt.Errorf("Path must begin with '/'")
	}
	return fs.client.setRawPath(fs.rootPath+path, nil, fileOpts, apprt)
}

func (fs *DashFSClient) SetStaticPath(path string, r io.ReadSeeker, fileOpts *FileOpts) error {
	if fileOpts == nil {
		fileOpts = &FileOpts{}
	}
	fileOpts.FileType = FileTypeStatic
	err := UpdateFileOptsFromReadSeeker(r, fileOpts)
	if err != nil {
		return err
	}
	return fs.SetRawPath(path, r, fileOpts, nil)
}

func (fs *DashFSClient) MakePathUrl(path string, jwtOpts *JWTOpts) (string, error) {
	if path == "" || !dashutil.IsFullPathValid(path) {
		return "", fmt.Errorf("Invalid Path")
	}
	if jwtOpts == nil {
		jwtOpts = fs.client.Config.GetJWTOpts()
	}
	pathLink := fs.client.getAccHost() + "/@fs" + fs.rootPath + path
	if jwtOpts.NoJWT {
		return pathLink, nil
	}
	err := jwtOpts.Validate()
	if err != nil {
		return "", err
	}
	jwtToken, err := fs.client.Config.MakeAccountJWT(jwtOpts)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s?jwt=%s", pathLink, jwtToken), nil
}

func (fs *DashFSClient) MustMakePathUrl(path string, jwtOpts *JWTOpts) string {
	rtn, err := fs.MakePathUrl(path, jwtOpts)
	if err != nil {
		panic(err)
	}
	return rtn
}

func (fs *DashFSClient) ConnectLinkRuntime(path string, runtime LinkRuntime) error {
	if !dashutil.IsFullPathValid(path) {
		return fmt.Errorf("Invalid Path")
	}
	if runtime == nil {
		return fmt.Errorf("LinkRuntime() error, runtime must not be nil")
	}
	err := fs.client.connectLinkRpc(path)
	if err != nil {
		return err
	}
	fs.client.connectLinkRuntime(path, runtime)
	return nil
}
