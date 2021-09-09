package dash

import (
	"bytes"
	"fmt"

	"github.com/sawka/dashborg-go-sdk/pkg/dasherr"
	"github.com/sawka/dashborg-go-sdk/pkg/dashutil"
)

const (
	AppRuntimeSubPath = "/_runtime"
	AppHtmlSubPath    = "/_html"
)

type DashAppClient struct {
	client *DashCloudClient
}

func (dac *DashAppClient) WriteApp(app *App) error {
	return dac.baseWriteApp(app, false)
}

func (dac *DashAppClient) WriteAndConnectApp(app *App) error {
	return dac.baseWriteApp(app, true)
}

func AppPathFromName(appName string) string {
	return "/@app/" + appName
}

func (dac *DashAppClient) RemoveApp(appName string) error {
	if !dashutil.IsAppNameValid(appName) {
		return dasherr.ValidateErr(fmt.Errorf("Invalid App Name"))
	}
	appPath := AppPathFromName(appName)
	err := dac.client.removePath(appPath)
	if err != nil {
		return err
	}
	err = dac.client.removePath(appPath + AppRuntimeSubPath)
	if err != nil {
		return err
	}
	err = dac.client.removePath(appPath + AppHtmlSubPath)
	if err != nil {
		return err
	}
	return nil
}

func (dac *DashAppClient) ConnectAppRuntime(app *App) error {
	appConfig, err := app.AppConfig()
	if err != nil {
		return err
	}
	if app.Runtime() == nil {
		return dasherr.ValidateErr(fmt.Errorf("No AppRuntime to connect, app.Runtime() is nil"))
	}
	if app.HasExternalRuntime() {
		return dasherr.ValidateErr(fmt.Errorf("App has specified an external runtime path '%s', use DashFS().LinkAppRuntime() to connect", app.getRuntimePath()))
	}
	runtimePath := appConfig.RuntimePath
	err = dac.client.connectLinkRpc(appConfig.RuntimePath)
	if err != nil {
		return err
	}
	dac.client.connectLinkRuntime(runtimePath, app.Runtime())
	return nil
}

func (dac *DashAppClient) MakeAppUrl(appNameOrPath string, jwtOpts *JWTOpts) (string, error) {
	if appNameOrPath == "" {
		return "", fmt.Errorf("Invalid App Path")
	}
	if appNameOrPath[0] == '/' {
		return dac.client.FSClient().MakePathUrl(appNameOrPath, jwtOpts)
	}
	appName := appNameOrPath
	accHost := dac.client.getAccHost()
	baseUrl := accHost + dashutil.MakeAppPath(dac.client.Config.ZoneName, appName)
	if jwtOpts == nil {
		jwtOpts = dac.client.Config.GetJWTOpts()
	}
	if jwtOpts.NoJWT {
		return baseUrl, nil
	}
	jwtToken, err := dac.client.Config.MakeAccountJWT(jwtOpts)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s?jwt=%s", baseUrl, jwtToken), nil
}

func (dac *DashAppClient) baseWriteApp(app *App, shouldConnect bool) error {
	appConfig, err := app.AppConfig()
	if err != nil {
		return err
	}
	if shouldConnect && app.HasExternalRuntime() {
		return dasherr.ValidateErr(fmt.Errorf("App has specified an external runtime path '%s', use DashFS().LinkAppRuntime() to connect", app.getRuntimePath()))
	}
	roles := appConfig.AllowedRoles
	fs := dac.client.FSClient()
	err = fs.SetJsonPath(app.AppPath(), appConfig, &FileOpts{MimeType: MimeTypeDashborgApp, AllowedRoles: roles})
	if err != nil {
		return err
	}
	// test html for error earlier
	htmlPath := appConfig.HtmlPath
	htmlFileOpts := &FileOpts{MimeType: MimeTypeHtml, AllowedRoles: roles}
	if app.htmlStr != "" {
		err = fs.SetStaticPath(htmlPath, bytes.NewReader([]byte(app.htmlStr)), htmlFileOpts)
	} else if app.htmlFileName != "" {
		if app.htmlFileWatchOpts == nil {
			err = fs.SetPathFromFile(htmlPath, app.htmlFileName, htmlFileOpts)
		} else {
			err = fs.WatchFile(htmlPath, app.htmlFileName, htmlFileOpts, app.htmlFileWatchOpts)
		}
	}
	if err != nil {
		return err
	}
	if shouldConnect {
		runtimePath := appConfig.RuntimePath
		err = fs.LinkAppRuntime(runtimePath, app.Runtime(), &FileOpts{AllowedRoles: roles})
		if err != nil {
			return err
		}
	}
	appLink, err := dac.MakeAppUrl(appConfig.AppName, nil)
	if err == nil {
		dac.client.log("Dashborg App Link [%s]: %s\n", appConfig.AppName, appLink)
	}
	return nil
}
