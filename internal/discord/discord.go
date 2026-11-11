// Package discord signs players in with their Discord account: it exchanges
// an OAuth2 authorization code (from RMG-K's loopback redirect, with PKCE) for
// a token and reads who the user is. Only the "identify" scope is used, and
// the Discord token is dropped right after the lookup.
package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	defaultAPIBase      = "https://discord.com/api"
	defaultAuthorizeURL = "https://discord.com/oauth2/authorize"
	// Scope is the only permission asked for: the user's ID and names.
	Scope = "identify"
)

// Config is the Discord application the lobby signs in with.
type Config struct {
	ClientID     string
	ClientSecret string
	// RedirectURIs are the loopback redirects registered with Discord, e.g.
	// http://127.0.0.1:53135/callback; RMG-K listens on the first free one.
	RedirectURIs []string
	// APIBase and AuthorizeURL default to Discord's; tests and local
	// end-to-end runs point them at a fake.
	APIBase      string
	AuthorizeURL string
}

// ConfigFromEnv reads RMGK_DISCORD_*. It returns nil when Discord sign-in
// isn't configured, and an error when it's only partly configured.
func ConfigFromEnv() (*Config, error) {
	cfg := &Config{
		ClientID:     strings.TrimSpace(os.Getenv("RMGK_DISCORD_CLIENT_ID")),
		ClientSecret: strings.TrimSpace(os.Getenv("RMGK_DISCORD_CLIENT_SECRET")),
		APIBase:      strings.TrimSpace(os.Getenv("RMGK_DISCORD_API_BASE")),
		AuthorizeURL: strings.TrimSpace(os.Getenv("RMGK_DISCORD_AUTHORIZE_URL")),
	}
	for _, uri := range strings.Split(os.Getenv("RMGK_DISCORD_REDIRECT_URIS"), ",") {
		if uri = strings.TrimSpace(uri); uri != "" {
			cfg.RedirectURIs = append(cfg.RedirectURIs, uri)
		}
	}
	if cfg.ClientID == "" && cfg.ClientSecret == "" {
		return nil, nil
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" || len(cfg.RedirectURIs) == 0 {
		return nil, errors.New("RMGK_DISCORD_CLIENT_ID, RMGK_DISCORD_CLIENT_SECRET and RMGK_DISCORD_REDIRECT_URIS must all be set")
	}
	for _, uri := range cfg.RedirectURIs {
		u, err := url.Parse(uri)
		if err != nil || u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || u.Port() == "" {
			return nil, fmt.Errorf("redirect URI %q must be a loopback http URL with a port, like http://127.0.0.1:53135/callback", uri)
		}
	}
	return cfg, nil
}

// User is the signed-in Discord account.
type User struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
}

// DisplayName is the user's display name, or their username.
func (u User) DisplayName() string {
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

// Client talks to Discord.
type Client struct {
	cfg  Config
	http *http.Client
}

// New returns a client for cfg.
func New(cfg Config) *Client {
	if cfg.APIBase == "" {
		cfg.APIBase = defaultAPIBase
	}
	if cfg.AuthorizeURL == "" {
		cfg.AuthorizeURL = defaultAuthorizeURL
	}
	cfg.APIBase = strings.TrimRight(cfg.APIBase, "/")
	return &Client{cfg: cfg, http: &http.Client{Timeout: 10 * time.Second}}
}

// ClientID, AuthorizeURL and RedirectURIs are what RMG-K needs to start a sign-in.
func (c *Client) ClientID() string       { return c.cfg.ClientID }
func (c *Client) AuthorizeURL() string   { return c.cfg.AuthorizeURL }
func (c *Client) RedirectURIs() []string { return slices.Clone(c.cfg.RedirectURIs) }

// ErrBadRedirect is returned for a redirect URI that isn't registered.
var ErrBadRedirect = errors.New("redirect URI not registered")

// Identify exchanges an authorization code (with its PKCE verifier) and
// returns the Discord user it belongs to.
func (c *Client) Identify(ctx context.Context, code, verifier, redirectURI string) (User, error) {
	if !slices.Contains(c.cfg.RedirectURIs, redirectURI) {
		return User{}, ErrBadRedirect
	}
	if code == "" || len(code) > 512 || len(verifier) < 43 || len(verifier) > 128 {
		return User{}, errors.New("invalid code or verifier")
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIBase+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return User{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(c.cfg.ClientID, c.cfg.ClientSecret)
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
	}
	if err := c.do(req, &token); err != nil {
		return User{}, fmt.Errorf("token exchange: %w", err)
	}
	if token.AccessToken == "" {
		return User{}, errors.New("token exchange: no access token")
	}

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.APIBase+"/users/@me", nil)
	if err != nil {
		return User{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	var user User
	if err := c.do(req, &user); err != nil {
		return User{}, fmt.Errorf("user lookup: %w", err)
	}
	if user.ID == "" {
		return User{}, errors.New("user lookup: no user id")
	}
	return user, nil
}

func (c *Client) do(req *http.Request, out any) error {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "RMG-K lobby (https://github.com/jay-day/rmgk-lobby)")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}
