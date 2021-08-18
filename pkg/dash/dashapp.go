package dash

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"reflect"
	"sync"

	"github.com/google/uuid"
	"github.com/sawka/dashborg-go-sdk/pkg/dashutil"
)

var notAuthorizedErr = fmt.Errorf("Not Authorized")

const MaxAppConfigSize = 1000000
const rootHtmlKey = "html:root"
const htmlMimeType = "text/html"

const (
	OptionInitHandler   = "inithandler"
	OptionHtml          = "html"
	OptionAuth          = "auth"
	OptionOfflineMode   = "offlinemode"
	OptionTitle         = "title"
	OptionAppVisibility = "visibility"

	AuthTypeZone = "zone"

	VisTypeHidden        = "hidden"  // always hide
	VisTypeDefault       = "default" // shown if user has permission
	VisTypeAlwaysVisible = "visible" // always show

	HtmlTypeStatic               = "static"
	HtmlTypeDynamicWhenConnected = "dynamic-when-connected"
	HtmlTypeDynamic              = "dynamic"
)

// html: static, dynamic, dynamic-when-connected
// offlinemode: enable, disable

// AppConfig is passed as JSON to the container.  this struct
// helps with marshaling/unmarshaling the structure.
type AppConfig struct {
	AppName            string                      `json:"appname"`
	AppVersion         string                      `json:"appversion,omitempty"` // uuid
	UpdatedTs          int64                       `json:"updatedts"`            // set by container
	ProcRunId          string                      `json:"procrunid"`            // set by container
	ClientVersion      string                      `json:"clientversion"`
	Options            map[string]GenericAppOption `json:"options"`
	StaticData         []staticDataVal             `json:"staticdata,omitempty"`
	ClearExistingData  bool                        `json:"clearexistingdata,omitempty"`
	ClearExistingBlobs bool                        `json:"clearexistingblobs,omitempty"`
}

// super-set of all option fields for JSON marshaling/parsing
type GenericAppOption struct {
	Type         string   `json:"type,omitempty"`
	Path         string   `json:"path,omitempty"`
	AllowedRoles []string `json:"allowedroles,omitempty"`
	Enabled      bool     `json:"enabled,omitempty"`
	AppTitle     string   `json:"apptitle,omitempty"`
	Order        float64  `json:"order,omitempty"`
}

type staticDataVal struct {
	Path string      `json:"path"`
	Data interface{} `json:"data"`
}

type BlobData struct {
	BlobKey  string      `json:"blobkey"`
	MimeType string      `json:"mimetype"`
	Size     int64       `json:"size"`
	Sha256   string      `json:"sha256"`
	UpdateTs int64       `json:"updatets"`
	Metadata interface{} `json:"metadata"`
	Removed  bool        `json:"removed"`
}

type ProcInfo struct {
	StartTs   int64
	ProcRunId string
	ProcName  string
	ProcTags  map[string]string
	HostData  map[string]string
}

type AppRuntime interface {
	GetAppName() string
	GetAppConfig() AppConfig
	RunHandler(req *Request) (interface{}, error)
}

type appRuntimeImpl struct {
	lock         *sync.Mutex
	appStateType reflect.Type
	html         valueType
	handlers     map[handlerKey]handlerType
}

type App struct {
	AppConfig  AppConfig
	appRuntime *appRuntimeImpl
	Container  Container
	isNewApp   bool

	// liveUpdateMode  bool
	// connectOnlyMode bool
}

type valueType interface {
	IsDynamic() bool
	GetValue() (interface{}, error)
}

type funcValueType struct {
	Dyn     bool
	ValueFn func() (string, error)
}

func fileValue(fileName string, isDynamic bool) valueType {
	return funcValueType{
		Dyn: isDynamic,
		ValueFn: func() (string, error) {
			fd, err := os.Open(fileName)
			if err != nil {
				return "", err
			}
			fileBytes, err := ioutil.ReadAll(fd)
			if err != nil {
				return "", err
			}
			return string(fileBytes), nil
		},
	}
}

func stringValue(val string) valueType {
	return funcValueType{
		Dyn: false,
		ValueFn: func() (string, error) {
			return val, nil
		},
	}
}

func interfaceValue(val interface{}) valueType {
	return funcValueType{
		Dyn: false,
		ValueFn: func() (string, error) {
			return dashutil.MarshalJson(val)
		},
	}
}

func funcValue(fn func() (interface{}, error), isDynamic bool) valueType {
	return funcValueType{
		Dyn: isDynamic,
		ValueFn: func() (string, error) {
			val, err := fn()
			if err != nil {
				return "", err
			}
			return dashutil.MarshalJson(val)
		},
	}
}

func (fv funcValueType) IsDynamic() bool {
	return fv.Dyn
}

func (fv funcValueType) GetValue() (interface{}, error) {
	return fv.ValueFn()
}

func defaultAuthOpt() GenericAppOption {
	authOpt := GenericAppOption{
		Type:         AuthTypeZone,
		AllowedRoles: []string{"user"},
	}
	return authOpt
}

func makeAppRuntime() *appRuntimeImpl {
	rtn := &appRuntimeImpl{
		lock: &sync.Mutex{},
	}
	rtn.handlers = make(map[handlerKey]handlerType)
	rtn.handlers[handlerKey{HandlerType: "html"}] = handlerType{HandlerFn: rtn.htmlHandler}
	return rtn
}

func MakeApp(appName string, container Container) *App {
	rtn := &App{
		Container:  container,
		appRuntime: makeAppRuntime(),
		AppConfig: AppConfig{
			AppVersion: uuid.New().String(),
			AppName:    appName,
		},
		isNewApp: true,
	}
	rtn.AppConfig.Options = make(map[string]GenericAppOption)
	authOpt := defaultAuthOpt()
	rtn.AppConfig.Options[OptionAuth] = authOpt
	rtn.AppConfig.Options[OptionOfflineMode] = GenericAppOption{Type: "allow"}
	return rtn
}

func (app *App) SetOfflineMode(allow bool) {
	if allow {
		app.AppConfig.Options[OptionOfflineMode] = GenericAppOption{Type: "allow"}
	} else {
		delete(app.AppConfig.Options, OptionOfflineMode)
	}
}

func (app *App) IsNew() bool {
	return app.isNewApp
}

// func (app *App) SetConnectOnly(connectOnly bool) {
// 	app.connectOnlyMode = connectOnly
// }

// func (app *App) SetLiveUpdate(liveUpdate bool) {
// 	app.liveUpdateMode = liveUpdate
// }

func (app *App) ClearExistingData() {
	app.AppConfig.ClearExistingData = true
}

func (app *App) ClearExistingBlobs() {
	app.AppConfig.ClearExistingBlobs = true
}

func MakeAppFromConfig(cfg AppConfig, container Container) *App {
	rtn := &App{
		Container:  container,
		appRuntime: makeAppRuntime(),
		AppConfig:  cfg,
	}
	rtn.AppConfig.AppVersion = uuid.New().String()
	return rtn
}

type handlerType struct {
	HandlerFn       func(req *Request) (interface{}, error)
	BoundHandlerKey *handlerKey
}

func (app *App) GetAppConfig() AppConfig {
	return app.AppConfig
}

func (app *App) RunHandler(req *Request) (interface{}, error) {
	return app.appRuntime.RunHandler(req)
}

func (app *appRuntimeImpl) SetHandler(hkey handlerKey, handler handlerType) {
	app.lock.Lock()
	defer app.lock.Unlock()
	app.handlers[hkey] = handler
}

func (app *appRuntimeImpl) RunHandler(req *Request) (interface{}, error) {
	hkey := handlerKey{
		HandlerType: req.info.RequestType,
		Path:        req.info.Path,
	}
	app.lock.Lock()
	hval, ok := app.handlers[hkey]
	app.lock.Unlock()
	if !ok {
		return nil, fmt.Errorf("No handler found for %s:%s", req.info.AppName, req.info.Path)
	}
	rtn, err := hval.HandlerFn(req)
	if err != nil {
		return nil, err
	}
	return rtn, nil
}

func (app *App) RemoveOption(optName string) {
	delete(app.AppConfig.Options, optName)
}

func (app *App) SetOption(optName string, opt GenericAppOption) {
	app.AppConfig.Options[optName] = opt
}

func wrapHandler(handlerFn func(req *Request) error) func(req *Request) (interface{}, error) {
	wrappedHandlerFn := func(req *Request) (interface{}, error) {
		err := handlerFn(req)
		return nil, err
	}
	return wrappedHandlerFn
}

func (app *App) getAuthOpt() GenericAppOption {
	authOpt, ok := app.AppConfig.Options[OptionAuth]
	if !ok {
		return defaultAuthOpt()
	}
	return authOpt
}

func (app *App) SetAuthType(authType string) {
	authOpt := app.getAuthOpt()
	authOpt.Type = authType
	app.AppConfig.Options[OptionAuth] = authOpt
}

func (app *App) SetAllowedRoles(roles ...string) {
	authOpt := app.getAuthOpt()
	authOpt.AllowedRoles = roles
	app.AppConfig.Options[OptionAuth] = authOpt
}

// SetAppVisibility controls whether the app shows in the UI's app-switcher (see VisType constants)
// Apps will be sorted by displayOrder (and then AppTitle).  displayOrder of 0 (the default) will
// sort to the end of the list, not the beginning
func (app *App) SetAppVisibility(visType string, displayOrder float64) {
	visOpt := GenericAppOption{Type: visType, Order: displayOrder}
	app.AppConfig.Options[OptionAppVisibility] = visOpt
}

func (app *App) SetAppTitle(title string) {
	if title == "" {
		delete(app.AppConfig.Options, OptionTitle)
	} else {
		app.AppConfig.Options[OptionTitle] = GenericAppOption{AppTitle: title}
	}
}

func (app *App) SetHtml(htmlStr string) error {
	bytesReader := bytes.NewReader([]byte(htmlStr))
	blobData, err := BlobDataFromReadSeeker(rootHtmlKey, htmlMimeType, bytesReader)
	if err != nil {
		return err
	}
	err = app.SetRawBlobData(blobData, bytesReader)
	if err != nil {
		return err
	}
	app.SetOption(OptionHtml, GenericAppOption{Type: HtmlTypeStatic})
	return nil
}

func (app *App) SetHtmlFromFile(fileName string) error {
	htmlValue := fileValue(fileName, true)
	htmlIf, err := htmlValue.GetValue()
	if err != nil {
		return err
	}
	htmlStr, err := dashutil.ConvertToString(htmlIf)
	if err != nil {
		return err
	}
	bytesReader := bytes.NewReader([]byte(htmlStr))
	blobData, err := BlobDataFromReadSeeker(rootHtmlKey, htmlMimeType, bytesReader)
	if err != nil {
		return err
	}
	err = app.SetRawBlobData(blobData, bytesReader)
	if err != nil {
		return err
	}
	app.appRuntime.html = htmlValue
	app.SetOption(OptionHtml, GenericAppOption{Type: HtmlTypeDynamicWhenConnected})
	return nil
}

func (app *App) SetAppStateType(appStateType reflect.Type) {
	app.appRuntime.appStateType = appStateType
}

func (app *App) SetInitHandler(handlerFn func(req *Request) error) {
	hkey := handlerKey{HandlerType: "init"}
	app.SetOption(OptionInitHandler, GenericAppOption{Type: "handler"})
	app.appRuntime.SetHandler(hkey, handlerType{HandlerFn: wrapHandler(handlerFn)})
}

func (app *App) Handler(path string, handlerFn func(req *Request) error) error {
	hkey := handlerKey{HandlerType: "handler", Path: path}
	app.appRuntime.SetHandler(hkey, handlerType{HandlerFn: wrapHandler(handlerFn)})
	return nil
}

func (app *appRuntimeImpl) htmlHandler(req *Request) (interface{}, error) {
	if app.html == nil {
		return nil, nil
	}
	htmlValueIf, err := app.html.GetValue()
	if err != nil {
		return nil, err
	}
	htmlStr, err := dashutil.ConvertToString(htmlValueIf)
	if err != nil {
		return nil, err
	}
	RequestEx{req}.SetHtml(htmlStr)
	return nil, nil
}

func (app *App) DataHandler(path string, handlerFn func(req *Request) (interface{}, error)) error {
	hkey := handlerKey{HandlerType: "data", Path: path}
	app.appRuntime.SetHandler(hkey, handlerType{HandlerFn: handlerFn})
	return nil
}

func (app *App) SetStaticData(path string, data interface{}) {
	app.AppConfig.StaticData = append(app.AppConfig.StaticData, staticDataVal{Path: path, Data: data})
}

func (app *App) GetAppName() string {
	return app.AppConfig.AppName
}

// SetRawBlobData blobData must have BlobKey, MimeType, Size, and Sha256 set.
// Clients will normally call SetBlobDataFromFile, or construct a BlobData
// from calling BlobDataFromReadSeeker or BlobDataFromReader rather than
// creating a BlobData directly.
func (app *App) SetRawBlobData(blobData BlobData, reader io.Reader) error {
	err := app.Container.SetBlobData(app.AppConfig, blobData, reader)
	if err != nil {
		log.Printf("Dashborg error setting blob data app:%s blobkey:%s err:%v\n", app.AppConfig.AppName, blobData.BlobKey, err)
		return err
	}
	return nil
}

// Will call Seek(0, 0) on the reader twice, once at the beginning and once at the end.
// If an error is returned, the seek position is not specified.  If no error is returned
// the reader will be reset to the beginning.
// A []byte can be wrapped in a bytes.Buffer to use this function (error will always be nil)
func BlobDataFromReadSeeker(key string, mimeType string, r io.ReadSeeker) (BlobData, error) {
	_, err := r.Seek(0, 0)
	if err != nil {
		return BlobData{}, nil
	}
	h := sha256.New()
	numCopyBytes, err := io.Copy(h, r)
	if err != nil {
		return BlobData{}, err
	}
	hashVal := h.Sum(nil)
	hashValStr := base64.StdEncoding.EncodeToString(hashVal[:])
	_, err = r.Seek(0, 0)
	if err != nil {
		return BlobData{}, err
	}
	blobData := BlobData{
		BlobKey:  key,
		MimeType: mimeType,
		Sha256:   hashValStr,
		Size:     numCopyBytes,
	}
	return blobData, nil
}

// If you only have an io.Reader, this function will call ioutil.ReadAll, read the full stream
// into a []byte, compute the size and SHA-256, and then wrap the []byte in a *bytes.Reader
// suitable to pass to SetRawBlobData()
func BlobDataFromReader(key string, mimeType string, r io.Reader) (BlobData, *bytes.Reader, error) {
	barr, err := ioutil.ReadAll(r)
	if err != nil {
		return BlobData{}, nil, err
	}
	breader := bytes.NewReader(barr)
	blobData, err := BlobDataFromReadSeeker(key, mimeType, breader)
	if err != nil {
		return BlobData{}, nil, err
	}
	return blobData, breader, nil
}

func (app *App) SetBlobDataFromFile(key string, mimeType string, fileName string, metadata interface{}) error {
	fd, err := os.Open(fileName)
	if err != nil {
		return err
	}
	blobData, err := BlobDataFromReadSeeker(key, mimeType, fd)
	if err != nil {
		return err
	}
	blobData.Metadata = metadata
	return app.SetRawBlobData(blobData, fd)
}
