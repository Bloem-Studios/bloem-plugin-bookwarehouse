// Command bloem-plugin-bookwarehouse serves native Bookwarehouse storage and
// Bookwarehouse ebook or audiobook requests over the published Bloem SDK
// go-plugin protocol.
package main

import (
	_ "embed"

	"github.com/Bloem-Studios/bloem-plugin-bookwarehouse/internal/provider"
	"github.com/Bloem-Studios/bloem-plugin-bookwarehouse/internal/requests"
	sdkruntime "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginsdk/runtime"
)

//go:embed manifest.json
var manifestJSON []byte

var version = "0.4.0"

func main() {
	p := provider.New()
	defer p.Close()
	sdkruntime.ServeManifestWithOptions(
		manifestJSON, version,
		sdkruntime.CapabilityServers{RequestRouter: requests.New(requestSettings(p))},
		sdkruntime.WithStorageProvider(p),
		sdkruntime.WithConfigure(p.Configure),
	)
}

// requestSettings hands the request router the installation's own
// connection: the one Runtime.Configure gave the storage provider.
func requestSettings(p *provider.Provider) func() (requests.Settings, bool) {
	return func() (requests.Settings, bool) {
		c, ok := p.Connection()
		if !ok {
			return requests.Settings{}, false
		}
		return requests.Settings{
			Content: c.Content, BaseURL: c.BaseURL, APIKey: c.APIKey, RequestAPIKey: c.RequestAPIKey,
			AllowHTTP: c.AllowInsecureHTTP, PreferredFormat: c.PreferredFormat,
		}, true
	}
}
