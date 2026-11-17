package modsite

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"fmt"
)

// The two pages: login.html for anyone, app.html for a signed-in moderator.
// Each is one file with its CSS and script inline and nothing loaded from
// anywhere. The Content-Security-Policy allows exactly that inline script
// and style by their sha256, so nothing injected could run.

//go:embed login.html
var loginHTML []byte

//go:embed app.html
var appHTML []byte

type page struct {
	html []byte
	csp  string
}

type pages struct {
	login, app page
}

func loadPages() *pages {
	return &pages{login: newPage("login.html", loginHTML), app: newPage("app.html", appHTML)}
}

func newPage(name string, html []byte) page {
	// Browsers hash a script as parsed, with every CR LF (and lone CR) read
	// as LF; a Windows checkout has CR LF, so serve and hash it that way.
	html = bytes.ReplaceAll(html, []byte("\r\n"), []byte("\n"))
	html = bytes.ReplaceAll(html, []byte("\r"), []byte("\n"))
	script := inline(name, html, "script")
	style := inline(name, html, "style")
	csp := "default-src 'none'; script-src " + hashSource(script) + "; style-src " + hashSource(style) +
		"; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	return page{html: html, csp: csp}
}

// inline returns the content of the page's one <tag>...</tag> block.
func inline(name string, html []byte, tag string) []byte {
	open, end := []byte("<"+tag+">"), []byte("</"+tag+">")
	i := bytes.Index(html, open)
	j := bytes.Index(html, end)
	if i < 0 || j < i || bytes.Count(html, open) != 1 || bytes.Count(html, []byte("<"+tag)) != 1 {
		panic(fmt.Sprintf("modsite: %s needs exactly one plain <%s> block", name, tag))
	}
	return html[i+len(open) : j]
}

func hashSource(content []byte) string {
	sum := sha256.Sum256(content)
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}
