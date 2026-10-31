package ws

import (
	"net/http"
	"net/url"
	"strings"
)

// CrossSiteBrowser reports whether r comes from a web page on another site.
// Browsers always send Origin on a WebSocket handshake; native clients
// (RMG-K) send none. Refusing these keeps other sites from connecting in a
// visitor's name, which would also spend that visitor's connect budget.
func CrossSiteBrowser(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	return err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host)
}
