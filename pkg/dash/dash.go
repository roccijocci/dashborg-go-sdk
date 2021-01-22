package dash

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"sync"
	"time"

	"github.com/mitchellh/mapstructure"
	"github.com/sawka/dashborg-go-sdk/pkg/dashproto"
	"github.com/sawka/dashborg-go-sdk/pkg/dashutil"
)

type Config struct {
	// DASHBORG_ACCID, set to force an AccountId (must match certificate).  If not set, AccountId is set from certificate file.
	// If AccId is given and AutoKeygen is true, and key/cert files are not found, Dashborg will create a new self-signed
	//     keypair using the AccId given.
	// If AccId is given, and the certificate does not match, this will cause a panic.
	AccId string

	// Set to true for unregistered accounts
	AnonAcc bool

	// DASHBORG_ZONE defaults to "default"
	ZoneName string

	// Process Name Attributes.  Only ProcName is required
	ProcName string // DASHBORG_PROCNAME (set from executable filename if not set)
	ProcTags map[string]string

	KeyFileName  string // DASHBORG_KEYFILE private key file
	CertFileName string // DASHBORG_CERTFILE certificate file, CN must be set to your Dashborg Account Id.

	// Create a self-signed key/cert if they do not exist.  This will also create a random Account Id.
	// Should only be used with AnonAcc is true, and AccId is not set
	AutoKeygen bool

	// The minimum amount of time to wait for all events to complete processing before shutting down after calling WaitForClear()
	// Defaults to 1 second.
	MinClearTimeout time.Duration

	// DASHBORG_VERBOSE, set to true for extra debugging information
	Verbose bool

	// These are for internal testing, should not normally be set by clients.
	Env             string // DASHBORG_ENV
	DashborgSrvHost string // DASHBORG_PROCHOST
	DashborgSrvPort int    // DASHBORG_PROCPORT
}

type PanelRequest struct {
	Ctx         context.Context
	Lock        *sync.Mutex // synchronizes RRActions
	PanelName   string
	ReqId       string
	RequestType string
	FeClientId  string
	Path        string
	Data        interface{}
	Model       interface{}
	AuthData    []*authAtom
	RRActions   []*dashproto.RRAction
	Err         error
	IsDone      bool
	AuthImpl    bool // if not set, will default NoAuth() on Done()
}

func panelLink(panelName string) string {
	accId := globalClient.Config.AccId
	zoneName := globalClient.Config.ZoneName
	var hostName string
	if globalClient.Config.Env == "dev" {
		hostName = fmt.Sprintf("http://acc-%s.console.dashborg.localdev:8080", accId)
		return fmt.Sprintf("%s/zone/%s/%s", hostName, zoneName, panelName)
	}
	hostName = fmt.Sprintf("https://acc-%s.console.dashborg.net", accId)
	return fmt.Sprintf("%s/zone/%s/%s", hostName, zoneName, panelName)
}

func (req *PanelRequest) appendRR(rrAction *dashproto.RRAction) {
	req.Lock.Lock()
	defer req.Lock.Unlock()
	req.RRActions = append(req.RRActions, rrAction)
}

func (req *PanelRequest) SetData(path string, data interface{}) error {
	if req.IsDone {
		return fmt.Errorf("Cannot call SetData(), path=%s, PanelRequest is already done", path)
	}
	jsonData, err := marshalJson(data)
	if err != nil {
		return fmt.Errorf("Error marshaling json for SetData, path:%s, err:%v\n", path, err)
	}
	rrAction := &dashproto.RRAction{
		Ts:         dashutil.Ts(),
		ActionType: "setdata",
		Selector:   path,
		JsonData:   jsonData,
	}
	req.appendRR(rrAction)
	return nil
}

func (req *PanelRequest) SetHtml(html string) error {
	ts := dashutil.Ts()
	htmlAction := &dashproto.RRAction{
		Ts:         ts,
		ActionType: "html",
		Html:       html,
	}
	req.appendRR(htmlAction)
	return nil
}

func (req *PanelRequest) SetHtmlFromFile(fileName string) error {
	fd, err := os.Open(fileName)
	if err != nil {
		return err
	}
	defer fd.Close()
	htmlBytes, err := ioutil.ReadAll(fd)
	if err != nil {
		return err
	}
	ts := dashutil.Ts()
	htmlAction := &dashproto.RRAction{
		Ts:         ts,
		ActionType: "html",
		Html:       string(htmlBytes),
	}
	req.appendRR(htmlAction)
	return nil
}

func (req *PanelRequest) isRootReq() bool {
	return req.RequestType == "handler" && req.PanelName != "" && req.Path == "/"
}

type authAtom struct {
	Scope string      `json:"scope"`
	Type  string      `json:"type"`
	Auto  bool        `json:"auto,omitempty"`
	Ts    int64       `json:"ts,omitempty"`
	Id    string      `json:"id,omitempty"`
	Role  string      `json:"role"`
	Data  interface{} `json:"data,omitempty"`
}

type challengeField struct {
	Label string `json:"label"`
	Name  string `json:"name"`
	Type  string `json:"type"`
}

type authChallenge struct {
	AllowedAuth      string           `json:"allowedauth"` // challenge,dashborg
	ChallengeMessage string           `json:"challengemessage"`
	ChallengeError   string           `json:"challengeerror"`
	ChallengeFields  []challengeField `json:"challengefields"`
}

func (req *PanelRequest) appendPanelAuthRRAction(authType string, role string) {
	data := authAtom{
		Type: authType,
		Auto: true,
		Ts:   dashutil.Ts() + (24 * 60 * 60 * 1000),
		Role: role,
	}
	jsonData, _ := json.Marshal(data)
	rr := &dashproto.RRAction{
		Ts:         dashutil.Ts(),
		ActionType: "panelauth",
		JsonData:   string(jsonData),
	}
	req.appendRR(rr)
}

func (req *PanelRequest) appendPanelAuthChallenge(ch authChallenge) {
	challengeJson, _ := json.Marshal(ch)
	req.appendRR(&dashproto.RRAction{
		Ts:         dashutil.Ts(),
		ActionType: "panelauthchallenge",
		JsonData:   string(challengeJson),
	})
	return
}

func (req *PanelRequest) getRawAuthData() []*authAtom {
	if req.AuthData == nil {
		return nil
	}
	var rawAuth []*authAtom
	err := mapstructure.Decode(req.AuthData, &rawAuth)
	if err != nil {
		return nil
	}
	return rawAuth
}

func (req *PanelRequest) isAuthenticated() bool {
	rawAuth := req.getRawAuthData()
	return len(rawAuth) > 0
}

func (req *PanelRequest) hasPanelAuth(authType string) {
}

func (req *PanelRequest) NoAuth() {
	req.AuthImpl = true
	if !req.isAuthenticated() {
		req.appendPanelAuthRRAction("noauth", "user")
	}
}

type challengeData struct {
	ChallengeData map[string]string `json:"challengedata"`
}

func (req *PanelRequest) PasswordAuth(pw string) (bool, error) {
	req.AuthImpl = true
	if req.isAuthenticated() {
		return true, nil
	}
	// check challenge
	var challengeData challengeData
	err := mapstructure.Decode(req.Data, &challengeData)
	if err == nil && challengeData.ChallengeData["password"] == pw {
		req.appendPanelAuthRRAction("password", "user")
		return true, nil
	}
	// send challenge
	ch := authChallenge{
		AllowedAuth: "challenge,dashborg",
		ChallengeFields: []challengeField{challengeField{
			Label: "Panel Password",
			Name:  "password",
			Type:  "password",
		}},
	}
	if challengeData.ChallengeData["submitted"] == "1" {
		if challengeData.ChallengeData["password"] == "" {
			ch.ChallengeError = "Password cannot be blank"
		} else {
			ch.ChallengeError = "Invalid password"
		}
	}
	req.appendPanelAuthChallenge(ch)
	return false, fmt.Errorf("Not Authorized | Sending Password Challenge")
}

func (req *PanelRequest) DashborgAuth() (bool, error) {
	req.AuthImpl = true
	if req.isAuthenticated() {
		return true, nil
	}
	// send challenge
	ch := authChallenge{
		AllowedAuth: "dashborg",
	}
	req.appendPanelAuthChallenge(ch)
	return false, fmt.Errorf("Not Authorized | Dashborg Auth")
}

func (req *PanelRequest) InvalidateData(path string) error {
	if req.IsDone {
		return fmt.Errorf("Cannot call InvalidateData(), path=%s, PanelRequest is already done", path)
	}
	rrAction := &dashproto.RRAction{
		Ts:         dashutil.Ts(),
		ActionType: "invalidate",
		Selector:   path,
	}
	req.appendRR(rrAction)
	return nil
}

func (req *PanelRequest) sendEvent(selector string, eventType string, data interface{}) error {
	if req.IsDone {
		return fmt.Errorf("Cannot call SendEvent(), selector=%s, event=%s, PanelRequest is already done", selector, eventType)
	}
	jsonData, err := marshalJson(data)
	if err != nil {
		return fmt.Errorf("Error marshaling json for SendEvent, selector:%s, event:%s, err:%v\n", selector, eventType, err)
	}
	rrAction := &dashproto.RRAction{
		Ts:         dashutil.Ts(),
		ActionType: "event",
		Selector:   selector,
		EventType:  eventType,
		JsonData:   jsonData,
	}
	req.appendRR(rrAction)
	return nil
}

func (req *PanelRequest) flush() error {
	if req.IsDone {
		return fmt.Errorf("Cannot Flush(), PanelRequest is already done")
	}
	return globalClient.sendRequestResponse(req, false)
}

func (req *PanelRequest) Done() error {
	if req.IsDone {
		return nil
	}
	if !req.AuthImpl && req.isRootReq() {
		req.NoAuth()
	}
	err := globalClient.sendRequestResponse(req, true)
	if err != nil {
		log.Printf("Dashborg ERROR sending handler response: %v\n", err)
	}
	return err
}

func RegisterPanelHandler(panelName string, path string, handlerFn func(*PanelRequest) error) {
	hkey := &dashproto.HandlerKey{
		PanelName:   panelName,
		HandlerType: "handler",
		Path:        path,
	}
	hfn := func(req *PanelRequest) (interface{}, error) {
		err := handlerFn(req)
		return nil, err
	}
	globalClient.registerHandler(hkey, hfn)
	if path == "/" {
		log.Printf("Dashborg Define Panel [%s] link: %s\n", panelName, panelLink(panelName))
	}
}

func RegisterDataHandler(panelName string, path string, handlerFn func(*PanelRequest) (interface{}, error)) {
	hkey := &dashproto.HandlerKey{
		PanelName:   panelName,
		HandlerType: "data",
		Path:        path,
	}
	globalClient.registerHandler(hkey, handlerFn)
}
