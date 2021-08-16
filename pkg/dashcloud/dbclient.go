package dashcloud

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/sawka/dashborg-go-sdk/pkg/dash"
	"github.com/sawka/dashborg-go-sdk/pkg/dasherr"
	"github.com/sawka/dashborg-go-sdk/pkg/dashproto"
	"github.com/sawka/dashborg-go-sdk/pkg/dashutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
)

const grpcServerPath = "/grpc-server"
const mbConst = 1000000

const (
	mdConnIdKey        = "dashborg-connid"
	mdClientVersionKey = "dashborg-clientversion"
)

const (
	consoleHostProd = "console.dashborg.net"
	consoleHostDev  = "console.dashborg-dev.com:8080"
)

var NotConnectedErr = dasherr.ErrWithCodeStr(dasherr.ErrCodeNotConnected, "DashborgCloudClient is not Connected")

type AppStruct struct {
	AppClient dash.AppClient
	App       dash.AppRuntime
}

type DashCloudClient struct {
	Lock      *sync.Mutex
	StartTime time.Time
	ProcRunId string
	Config    *Config
	Conn      *grpc.ClientConn
	DBService dashproto.DashborgServiceClient
	ConnId    *atomic.Value
	AppMap    map[string]*AppStruct
	DoneCh    chan bool
	PermErr   bool
	ExitErr   error
	AccInfo   *dashproto.AccInfo
}

func makeCloudClient(config *Config) *DashCloudClient {
	rtn := &DashCloudClient{
		Lock:      &sync.Mutex{},
		StartTime: time.Now(),
		ProcRunId: uuid.New().String(),
		Config:    config,
		ConnId:    &atomic.Value{},
		AppMap:    make(map[string]*AppStruct),
		DoneCh:    make(chan bool),
	}
	rtn.ConnId.Store("")
	return rtn
}

type grpcConfig struct {
	GrpcServer string `json:"grpcserver"`
	GrpcPort   int    `json:"grpcport"`
}

type grpcServerRtn struct {
	Success bool       `json:"success"`
	Error   string     `json:"error"`
	Data    grpcConfig `json:"data"`
}

func (pc *DashCloudClient) getGrpcServer() (*grpcConfig, error) {
	urlVal := fmt.Sprintf("https://%s%s?accid=%s", pc.Config.DashborgConsoleHost, grpcServerPath, pc.Config.AccId)
	resp, err := http.Get(urlVal)
	if err != nil {
		return nil, fmt.Errorf("Cannot get gRPC Server Host: %w", err)
	}
	defer resp.Body.Close()
	bodyContent, err := ioutil.ReadAll(resp.Body)
	var grpcRtn grpcServerRtn
	err = json.Unmarshal(bodyContent, &grpcRtn)
	if err != nil {
		return nil, fmt.Errorf("Cannot get gRPC Server Host (decoding response): %w", err)
	}
	if !grpcRtn.Success {
		return nil, fmt.Errorf("Cannot get gRPC Server Host (error response): %s", grpcRtn.Error)
	}
	if grpcRtn.Data.GrpcServer == "" || grpcRtn.Data.GrpcPort == 0 {
		return nil, fmt.Errorf("Cannot get gRPC Server Host (bad response)")
	}
	return &grpcRtn.Data, nil
}

func (pc *DashCloudClient) startClient() error {
	err := pc.connectGrpc()
	if err != nil {
		pc.logV("Dashborg ERROR connecting gRPC client: %v\n", err)
	}
	if pc.Config.ShutdownCh != nil {
		go func() {
			<-pc.Config.ShutdownCh
			pc.externalShutdown()
		}()
	}
	if pc.Config.DashborgSrvHost == "" {
		grpcConfig, err := pc.getGrpcServer()
		if err != nil {
			return err
		}
		pc.Config.DashborgSrvHost = grpcConfig.GrpcServer
		pc.Config.DashborgSrvPort = grpcConfig.GrpcPort
		if pc.Config.Verbose {
			log.Printf("Dashborg Using gRPC host %s:%d\n", pc.Config.DashborgSrvHost, pc.Config.DashborgSrvPort)
		}
	}
	if pc.Config.Verbose {
		log.Printf("Dashborg Initialized CloudClient AccId:%s Zone:%s ProcName:%s ProcRunId:%s\n", pc.Config.AccId, pc.Config.ZoneName, pc.Config.ProcName, pc.ProcRunId)
	}
	err = pc.sendConnectClientMessage(false)
	if err != nil && !dasherr.CanRetry(err) {
		pc.setExitError(err)
		return err
	}
	go pc.runRequestStreamLoop()
	return nil
}

func (pc *DashCloudClient) ctxWithMd() context.Context {
	ctx := context.Background()
	connId := pc.ConnId.Load().(string)
	ctx = metadata.AppendToOutgoingContext(ctx, mdConnIdKey, connId, mdClientVersionKey, dash.ClientVersion)

	return ctx
}

func (pc *DashCloudClient) externalShutdown() {
	if pc.Conn == nil {
		pc.logV("Dashborg ERROR shutting down, gRPC connection is not initialized\n")
		return
	}
	pc.setExitError(fmt.Errorf("ShutdownCh channel closed"))
	err := pc.Conn.Close()
	if err != nil {
		pc.logV("Dashborg ERROR closing gRPC connection: %v\n", err)
	}
}

func makeHostData() map[string]string {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	hostData := map[string]string{
		"HostName": hostname,
		"Pid":      strconv.Itoa(os.Getpid()),
	}
	return hostData
}

func (pc *DashCloudClient) sendConnectClientMessage(isReconnect bool) error {
	// only allow one proc message at a time (synchronize)
	hostData := makeHostData()
	reconApps := make([]string, 0)
	for _, appstruct := range pc.AppMap {
		reconApps = append(reconApps, appstruct.App.GetAppName())
	}
	m := &dashproto.ConnectClientMessage{
		Ts:                   dashutil.Ts(),
		ProcRunId:            pc.ProcRunId,
		AccId:                pc.Config.AccId,
		ZoneName:             pc.Config.ZoneName,
		AnonAcc:              pc.Config.AnonAcc,
		ProcName:             pc.Config.ProcName,
		ProcTags:             pc.Config.ProcTags,
		HostData:             hostData,
		StartTs:              dashutil.DashTime(pc.StartTime),
		ReconnectAppRuntimes: reconApps,
	}
	resp, respErr := pc.DBService.ConnectClient(pc.ctxWithMd(), m)
	dashErr := pc.handleStatusErrors("ConnectClient", resp, respErr, true)
	if resp != nil && len(resp.ReconnectErrs) > 0 {
		for _, recErr := range resp.ReconnectErrs {
			log.Printf("%s\n", recErr)
		}
	}
	if dashErr != nil {
		pc.ConnId.Store("")
		if !dasherr.CanRetry(dashErr) {
			pc.Lock.Lock()
			pc.PermErr = true
			pc.Lock.Unlock()
		}
		return dashErr
	}
	pc.ConnId.Store(resp.ConnId)
	pc.Lock.Lock()
	pc.AccInfo = resp.AccInfo
	pc.Lock.Unlock()
	if !isReconnect {
		if resp.AccInfo.NewAccount {
			pc.printNewAccMessage()
		} else if resp.AccInfo.AccType == "anon" {
			pc.printAnonAccMessage()
		}
		if pc.Config.Verbose {
			log.Printf("DashborgCloudClient Connected, AccId:%s Zone:%s ConnId:%s AccType:%v\n", pc.Config.AccId, pc.Config.ZoneName, resp.ConnId, resp.AccInfo.AccType)
		} else {
			log.Printf("DashborgCloudClient Connected, AccId:%s Zone:%s\n", pc.Config.AccId, pc.Config.ZoneName)
		}
	} else {
		if pc.Config.Verbose {
			log.Printf("DashborgCloudClient ReConnected, AccId:%s Zone:%s ConnId:%s\n", pc.Config.AccId, pc.Config.ZoneName, resp.ConnId)
		}
	}
	return nil
}

func (pc *DashCloudClient) printNewAccMessage() {
	log.Printf("Welcome to Dashborg!  Your new account has been provisioned.  AccId:%s\n", pc.Config.AccId)
	log.Printf("You are currently using a free version of the Dashborg Service.\n")
	log.Printf("Your use of this service is subject to the Dashborg Terms of Service - https://www.dashborg.net/static/tos.html\n")
}

func (pc *DashCloudClient) printAnonAccMessage() {
	log.Printf("You are currently using a free version of the Dashborg Service.\n")
	log.Printf("Your use of this service is subject to the Dashborg Terms of Service - https://www.dashborg.net/static/tos.html\n")
}

func (pc *DashCloudClient) handleStatusErrors(fnName string, resp interface{}, respErr error, forceLog bool) error {
	var rtnErr error
	if respErr != nil {
		rtnErr = dasherr.RpcErr(fnName, respErr)
	} else {
		respV := reflect.ValueOf(resp).Elem()
		rtnStatus := respV.FieldByName("Status").Interface().(*dashproto.RtnStatus)
		rtnErr = dasherr.FromRtnStatus(fnName, rtnStatus)
	}
	if rtnErr == nil {
		return nil
	}
	if forceLog || pc.Config.Verbose {
		log.Printf("DashborgCloudClient %v\n", rtnErr)
	}
	if pc.AccInfo != nil {
		explainLimit(pc.AccInfo.AccType, rtnErr.Error())
	}
	return rtnErr
}

func (pc *DashCloudClient) connectGrpc() error {
	addr := pc.Config.DashborgSrvHost + ":" + strconv.Itoa(pc.Config.DashborgSrvPort)
	backoffConfig := backoff.Config{
		BaseDelay:  1.0 * time.Second,
		Multiplier: 1.6,
		Jitter:     0.2,
		MaxDelay:   60 * time.Second,
	}
	connectParams := grpc.ConnectParams{MinConnectTimeout: time.Second, Backoff: backoffConfig}
	keepaliveParams := keepalive.ClientParameters{Time: 10 * time.Second, Timeout: 5 * time.Second, PermitWithoutStream: true}
	clientCert, err := tls.LoadX509KeyPair(pc.Config.CertFileName, pc.Config.KeyFileName)
	if err != nil {
		return fmt.Errorf("Cannot load keypair key:%s cert:%s err:%w", pc.Config.KeyFileName, pc.Config.CertFileName, err)
	}
	tlsConfig := &tls.Config{
		MinVersion:               tls.VersionTLS13,
		CurvePreferences:         []tls.CurveID{tls.CurveP384},
		PreferServerCipherSuites: true,
		InsecureSkipVerify:       true,
		Certificates:             []tls.Certificate{clientCert},
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
		},
	}
	tlsCreds := credentials.NewTLS(tlsConfig)
	conn, err := grpc.Dial(
		addr,
		grpc.WithConnectParams(connectParams),
		grpc.WithKeepaliveParams(keepaliveParams),
		grpc.WithTransportCredentials(tlsCreds),
	)
	pc.Conn = conn
	pc.DBService = dashproto.NewDashborgServiceClient(conn)
	return err
}

func (pc *DashCloudClient) showAppLink(appName string) {
	pc.Lock.Lock()
	accInfo := pc.AccInfo
	pc.Lock.Unlock()
	if accInfo == nil {
		return
	}
	if pc.Config.NoShowJWT || !accInfo.AccJWTEnabled {
		log.Printf("DashborgCloudClient App Link [%s]: %s\n", appName, pc.appLink(appName))
	} else {
		appLink, err := pc.MakeJWTAppLink(appName, pc.Config.JWTDuration, pc.Config.JWTUserId, pc.Config.JWTRole)
		if err != nil {
			log.Printf("DashborgCloudClient App Link [%s] Error: %v\n", appName, err)
		} else {
			log.Printf("DashborgCloudClient App Link [%s]: %s\n", appName, appLink)
		}
	}
}

func (pc *DashCloudClient) ConnectApp(app dash.AppRuntime) error {
	if !pc.IsConnected() {
		return NotConnectedErr
	}
	appName := app.GetAppName()
	appConfig := app.GetAppConfig()
	dashErr := pc.baseWriteApp(app.GetAppName(), true, &appConfig, fmt.Sprintf("ConnectApp(%s)", app.GetAppName()))
	if dashErr != nil && !dasherr.CanRetry(dashErr) {
		log.Printf("DashborgCloudClient %v\n", dashErr)
		return dashErr
	}
	clientConfig := dash.AppClientConfig{
		Verbose: pc.Config.Verbose,
	}
	appClient := dash.MakeAppClient(pc, app, pc, clientConfig, pc.ConnId)
	pc.Lock.Lock()
	pc.AppMap[appName] = &AppStruct{App: app, AppClient: appClient}
	pc.Lock.Unlock()
	pc.showAppLink(appName)
	if dashErr != nil {
		log.Printf("DashborgCloudClient %v\n", dashErr)
		return dashErr
	}
	return nil
}

func (pc *DashCloudClient) RemoveApp(appName string) error {
	if !pc.IsConnected() {
		return NotConnectedErr
	}
	m := &dashproto.RemoveAppMessage{
		Ts:      dashutil.Ts(),
		AppName: appName,
	}
	resp, respErr := pc.DBService.RemoveApp(pc.ctxWithMd(), m)
	dashErr := pc.handleStatusErrors("RemoveApp", resp, respErr, true)
	if dashErr != nil {
		return dashErr
	}
	log.Printf("DashborgCloudClient removed app %s\n", appName)
	return nil
}

func (pc *DashCloudClient) ConnectAppRuntime(app dash.AppRuntime) error {
	if !pc.IsConnected() {
		return NotConnectedErr
	}
	appName := app.GetAppName()
	dashErr := pc.baseWriteApp(appName, true, nil, fmt.Sprintf("ConnectAppRuntime(%s)", appName))
	if dashErr != nil && !dasherr.CanRetry(dashErr) {
		log.Printf("DashborgCloudClient %v\n", dashErr)
		return dashErr
	}
	clientConfig := dash.AppClientConfig{
		Verbose: pc.Config.Verbose,
	}
	appClient := dash.MakeAppClient(pc, app, pc, clientConfig, pc.ConnId)
	pc.Lock.Lock()
	pc.AppMap[appName] = &AppStruct{App: app, AppClient: appClient}
	pc.Lock.Unlock()
	pc.showAppLink(appName)
	if dashErr != nil {
		log.Printf("DashborgCloudClient %v\n", dashErr)
		return dashErr
	}
	return nil
}

func (pc *DashCloudClient) runRequestStreamLoop() {
	defer close(pc.DoneCh)

	w := &dashutil.ExpoWait{}
	for {
		state := pc.Conn.GetState()
		if state == connectivity.Shutdown {
			log.Printf("DashborgCloudClient RunRequestStreamLoop exiting - Conn Shutdown\n")
			pc.setExitError(fmt.Errorf("gRPC Connection Shutdown"))
			break
		}
		if state == connectivity.Connecting || state == connectivity.TransientFailure {
			time.Sleep(1 * time.Second)
			w.Reset()
			continue
		}
		okWait := w.Wait()
		if !okWait {
			continue
		}
		if pc.ConnId.Load().(string) == "" {
			err := pc.sendConnectClientMessage(true)
			if err != nil && !dasherr.CanRetry(err) {
				log.Printf("DashborgCloudClient RunRequestStreamLoop exiting - Permanent Error: %v\n", err)
				pc.setExitError(err)
				break
			}
			if err != nil {
				continue
			}
		}
		ranOk, errCode := pc.runRequestStream()
		if ranOk {
			w.Reset()
		}
		if errCode == dasherr.ErrCodeBadConnId {
			pc.ConnId.Store("")
			continue
		}
		w.ForceWait = true
	}
}

func (pc *DashCloudClient) sendNoAppResponse(reqMsg *dashproto.RequestMessage) {
	m := &dashproto.SendResponseMessage{
		Ts:           dashutil.Ts(),
		ReqId:        reqMsg.ReqId,
		RequestType:  reqMsg.RequestType,
		PanelName:    reqMsg.PanelName,
		FeClientId:   reqMsg.FeClientId,
		ResponseDone: true,
		Err:          "No App Found",
	}
	_, err := pc.DBService.SendResponse(pc.ctxWithMd(), m)
	if err != nil {
		pc.logV("Error sending No App Response: %v\n", err)
	}
}

func (pc *DashCloudClient) runRequestStream() (bool, dasherr.ErrCode) {
	m := &dashproto.RequestStreamMessage{Ts: dashutil.Ts()}
	pc.logV("Dashborg gRPC RequestStream starting\n")
	reqStreamClient, err := pc.DBService.RequestStream(pc.ctxWithMd(), m)
	if err != nil {
		log.Printf("Dashborg Error setting up gRPC RequestStream: %v\n", err)
		return false, dasherr.ErrCodeRpc
	}
	startTime := time.Now()
	var reqCounter int64
	var endingErrCode dasherr.ErrCode
	for {
		reqMsg, err := reqStreamClient.Recv()
		if err == io.EOF {
			pc.logV("Dashborg gRPC RequestStream done: EOF\n")
			endingErrCode = dasherr.ErrCodeEof
			break
		}
		if err != nil {
			pc.logV("Dashborg %v\n", dasherr.RpcErr("RequestStream", err))
			endingErrCode = dasherr.ErrCodeRpc
			break
		}
		if reqMsg.Status != nil {
			dashErr := dasherr.FromRtnStatus("RequestStream", reqMsg.Status)
			if dashErr != nil {
				pc.logV("Dashborg %v\n", dashErr)
				endingErrCode = dasherr.GetErrCode(dashErr)
				break
			}
		}
		pc.logV("Dashborg gRPC got request: app=%s, type=%s, path=%s\n", reqMsg.PanelName, reqMsg.RequestType, reqMsg.Path)
		go func() {
			atomic.AddInt64(&reqCounter, 1)
			timeoutMs := reqMsg.TimeoutMs
			if timeoutMs == 0 || timeoutMs > 60000 {
				timeoutMs = 60000
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
			defer cancel()

			appName := reqMsg.PanelName
			pc.Lock.Lock()
			appClient := pc.AppMap[appName]
			pc.Lock.Unlock()
			if appClient == nil {
				pc.sendNoAppResponse(reqMsg)
				return
			}
			appClient.AppClient.DispatchRequest(ctx, reqMsg)
		}()
	}
	elapsed := time.Since(startTime)
	return (elapsed >= 5*time.Second), endingErrCode
}

func (pc *DashCloudClient) logV(fmtStr string, args ...interface{}) {
	if pc.Config.Verbose {
		log.Printf(fmtStr, args...)
	}
}

func (pc *DashCloudClient) BackendPush(panelName string, path string, data interface{}) error {
	if !pc.IsConnected() {
		return NotConnectedErr
	}
	m := &dashproto.BackendPushMessage{
		Ts:        dashutil.Ts(),
		PanelName: panelName,
		Path:      path,
	}
	resp, respErr := pc.DBService.BackendPush(pc.ctxWithMd(), m)
	dashErr := pc.handleStatusErrors("BackendPush", resp, respErr, false)
	if dashErr != nil {
		return dashErr
	}
	return nil
}

func (pc *DashCloudClient) ReflectZone() (*ReflectZoneType, error) {
	if !pc.IsConnected() {
		return nil, NotConnectedErr
	}
	m := &dashproto.ReflectZoneMessage{Ts: dashutil.Ts()}
	resp, respErr := pc.DBService.ReflectZone(pc.ctxWithMd(), m)
	dashErr := pc.handleStatusErrors("ReflectZone", resp, respErr, false)
	if dashErr != nil {
		return nil, dashErr
	}
	var rtn ReflectZoneType
	err := json.Unmarshal([]byte(resp.JsonData), &rtn)
	if err != nil {
		return nil, dasherr.JsonUnmarshalErr("ReflectZone", err)
	}
	return &rtn, nil
}

func (pc *DashCloudClient) CallDataHandler(panelName string, path string, data interface{}) (interface{}, error) {
	if !pc.IsConnected() {
		return nil, NotConnectedErr
	}
	jsonData, err := dashutil.MarshalJson(data)
	if err != nil {
		return nil, err
	}
	m := &dashproto.CallDataHandlerMessage{
		Ts:        dashutil.Ts(),
		PanelName: panelName,
		Path:      path,
		JsonData:  jsonData,
	}
	resp, respErr := pc.DBService.CallDataHandler(pc.ctxWithMd(), m)
	dashErr := pc.handleStatusErrors("CallDataHandler", resp, respErr, false)
	if dashErr != nil {
		return nil, dashErr
	}
	var rtn interface{}
	if resp.JsonData != "" {
		err = json.Unmarshal([]byte(resp.JsonData), &rtn)
		if err != nil {
			return nil, dasherr.JsonUnmarshalErr("CallDataResponse", err)
		}
	}
	return rtn, nil
}

// Bare streams start with no connected clients.  ControlPath is ignored, and NoServerCancel must be set to true.
// A future request can attach to the stream by calling req.StartStream() and passing the
// same StreamId.  An error will be returned if a stream with this StreamId has already started.
// Unlike StartStream StreamId must be specified ("" will return an error).
// Caller is responsible for calling req.Done() when the stream is finished.
func (pc *DashCloudClient) StartBareStream(appName string, streamOpts dash.StreamOpts) (*dash.Request, error) {
	if !pc.IsConnected() {
		return nil, NotConnectedErr
	}
	pc.Lock.Lock()
	app := pc.AppMap[appName]
	pc.Lock.Unlock()
	if app == nil {
		return nil, fmt.Errorf("No active app[%s] found for StartBareStream", appName)
	}
	streamReq, _, err := app.AppClient.StartStream(appName, streamOpts, "")
	return streamReq, err
}

// returns the reason for shutdown (GetExitError())
func (pc *DashCloudClient) WaitForShutdown() error {
	<-pc.DoneCh
	return pc.GetExitError()
}

func (pc *DashCloudClient) OpenApp(appName string) (*dash.App, error) {
	if !pc.IsConnected() {
		return nil, NotConnectedErr
	}
	m := &dashproto.OpenAppMessage{
		Ts:      dashutil.Ts(),
		AppName: appName,
	}
	resp, respErr := pc.DBService.OpenApp(pc.ctxWithMd(), m)
	dashErr := pc.handleStatusErrors("OpenApp", resp, respErr, true)
	if dashErr != nil {
		return nil, dashErr
	}
	if resp.AppConfigJson == "" {
		return dash.MakeApp(appName, pc), nil
	}
	var rtn dash.AppConfig
	err := json.Unmarshal([]byte(resp.AppConfigJson), &rtn)
	if err != nil {
		return nil, dasherr.JsonUnmarshalErr("AppConfig", err)
	}
	return dash.MakeAppFromConfig(rtn, pc), nil
}

func (pc *DashCloudClient) baseWriteApp(appName string, shouldConnect bool, acfg *dash.AppConfig, writeAppFnStr string) error {
	if !pc.IsConnected() {
		return NotConnectedErr
	}
	var jsonVal string
	if acfg != nil {
		var err error
		jsonVal, err = dashutil.MarshalJson(acfg)
		if err != nil {
			return dasherr.JsonMarshalErr("AppConfig", err)
		}
	}
	m := &dashproto.WriteAppMessage{
		Ts:            dashutil.Ts(),
		AppName:       appName,
		AppConfigJson: jsonVal,
		ConnectApp:    shouldConnect,
	}
	resp, respErr := pc.DBService.WriteApp(pc.ctxWithMd(), m)
	dashErr := pc.handleStatusErrors(writeAppFnStr, resp, respErr, false)
	if dashErr != nil {
		return dashErr
	}
	for name, warning := range resp.OptionWarnings {
		log.Printf("%s WARNING option[%s]: %s\n", writeAppFnStr, name, warning)
	}
	return nil
}

func (pc *DashCloudClient) WriteApp(acfg dash.AppConfig) error {
	dashErr := pc.baseWriteApp(acfg.AppName, false, &acfg, fmt.Sprintf("WriteApp(%s)", acfg.AppName))
	pc.showAppLink(acfg.AppName)
	if dashErr != nil {
		log.Printf("DashborgCloudClient %v\n", dashErr)
		return dashErr
	}
	return nil
}

func (pc *DashCloudClient) SetBlobData(acfg dash.AppConfig, blob dash.BlobData, r io.Reader) error {
	if !pc.IsConnected() {
		return NotConnectedErr
	}
	blobJson, err := dashutil.MarshalJson(blob)
	if err != nil {
		return err
	}
	barr, err := ioutil.ReadAll(r)
	if err != nil {
		return err
	}
	if float64(len(barr)) > pc.AccInfo.BlobSizeLimitMB*mbConst {
		err = dasherr.LimitErr("Cannot upload BLOB", "AppBlobs.MaxSizeMB", pc.AccInfo.BlobSizeLimitMB)
		explainLimit(pc.AccInfo.AccType, err.Error())
		return err
	}
	m := &dashproto.SetBlobMessage{
		Ts:           dashutil.Ts(),
		AppName:      acfg.AppName,
		AppVersion:   acfg.AppVersion,
		BlobDataJson: blobJson,
		BlobBytes:    barr,
	}
	bclient, err := pc.DBService.SetBlob(pc.ctxWithMd())
	if err != nil {
		return err
	}
	err = bclient.Send(m)
	if err != nil {
		return err
	}
	resp, respErr := bclient.CloseAndRecv()
	dashErr := pc.handleStatusErrors("SetBlobData", resp, respErr, false)
	if dashErr != nil {
		return dashErr
	}
	blob.Size = int64(len(barr))
	return nil
}

func (pc *DashCloudClient) setExitError(err error) {
	pc.Lock.Lock()
	defer pc.Lock.Unlock()
	if pc.ExitErr == nil {
		pc.ExitErr = err
	}
}

// Returns nil if client is still running. Returns error (reason for shutdown) if client has stopped.
func (pc *DashCloudClient) GetExitError() error {
	pc.Lock.Lock()
	defer pc.Lock.Unlock()
	return pc.ExitErr
}

func (pc *DashCloudClient) IsConnected() bool {
	if pc == nil || pc.Config == nil {
		return false
	}
	pc.Lock.Lock()
	defer pc.Lock.Unlock()

	if pc.ExitErr != nil {
		return false
	}
	if pc.Conn == nil {
		return false
	}
	connId := pc.ConnId.Load().(string)
	if connId == "" {
		return false
	}
	return true
}

func (pc *DashCloudClient) MakeJWTAppLink(appName string, validTime time.Duration, userId string, roleName string) (string, error) {
	if validTime == 0 {
		validTime = 24 * time.Hour
	}
	if roleName == "" {
		roleName = "user"
	}
	if userId == "" {
		userId = "jwt-user"
	}
	jwtToken, err := pc.Config.MakeAccountJWT(validTime, userId, roleName)
	if err != nil {
		return "", err
	}
	link := pc.appLink(appName)
	return fmt.Sprintf("%s?jwt=%s", link, jwtToken), nil
}

func (pc *DashCloudClient) MustMakeJWTAppLink(appName string, validTime time.Duration, userId string, roleName string) string {
	rtn, err := pc.MakeJWTAppLink(appName, validTime, userId, roleName)
	if err != nil {
		panic(err)
	}
	return rtn
}

func (pc *DashCloudClient) getAccHost() string {
	if !pc.IsConnected() {
		panic("DashCloudClient is not connected")
	}
	pc.Lock.Lock()
	defer pc.Lock.Unlock()

	if pc.AccInfo != nil && pc.AccInfo.AccCName != "" {
		if pc.Config.Env != "prod" {
			return fmt.Sprintf("https://%s:8080", pc.AccInfo.AccCName)
		}
		return fmt.Sprintf("https://%s", pc.AccInfo.AccCName)
	}
	accId := pc.Config.AccId
	if pc.Config.Env != "prod" {
		return fmt.Sprintf("https://acc-%s.%s", accId, consoleHostDev)
	}
	return fmt.Sprintf("https://acc-%s.%s", accId, consoleHostProd)
}

func (pc *DashCloudClient) appLink(appName string) string {
	accHost := pc.getAccHost()
	path := dashutil.MakeAppPath(pc.Config.ZoneName, appName)
	return accHost + path
}

// StartStreamProtoRpc is for use by the Dashborg AppClient, not to be called by end user.
func (pc *DashCloudClient) StartStreamProtoRpc(m *dashproto.StartStreamMessage) (string, error) {
	if !pc.IsConnected() {
		return "", NotConnectedErr
	}
	resp, respErr := pc.DBService.StartStream(pc.ctxWithMd(), m)
	dashErr := pc.handleStatusErrors("StartStream", resp, respErr, false)
	if dashErr != nil {
		pc.logV("DashborgCloudClient %v\n", dashErr)
		return "", dashErr
	}
	if m.ExistingReqId != "" && m.ExistingReqId != resp.ReqId {
		return "", fmt.Errorf("Dashborg startStream returned reqid:%s does not match existing reqid:%s", resp.ReqId, m.ExistingReqId)
	}
	return resp.ReqId, nil
}

// SendResponseProtoRpc is for internal use by the Dashborg AppClient, not to be called by the end user.
func (pc *DashCloudClient) SendResponseProtoRpc(m *dashproto.SendResponseMessage) (int, error) {
	if !pc.IsConnected() {
		return 0, NotConnectedErr
	}
	resp, respErr := pc.DBService.SendResponse(pc.ctxWithMd(), m)
	dashErr := pc.handleStatusErrors("SendResponse", resp, respErr, false)
	if dashErr != nil {
		return 0, dashErr
	}
	return int(resp.NumStreamClients), nil
}
