// Package keycloak is a dependency-free Admin REST client for the bundled (or
// an external) Keycloak. It speaks the Admin API over the standard library
// net/http only — it never imports an SDK — so the management plane can manage
// platform users (list / create / update / delete / reset password) and, during
// first-run setup, bootstrap the bundled 'admin' account.
//
// It authenticates as a confidential client using the client_credentials grant
// (a service account with realm-admin rights), caches the resulting access
// token and refreshes it shortly before expiry. The client is safe for
// concurrent use.
//
// When the integration is not configured (KEYCLOAK_BASE_URL / client secret
// unset), New returns a DISABLED client: every method returns ErrDisabled so
// callers can map it to 503 and non-Keycloak deployments (an operator supplying
// their own external oidcIssuer) keep working unchanged.
package keycloak

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrDisabled is returned by every method when the client has no base URL /
// credentials configured. Callers translate it to HTTP 503.
var ErrDisabled = errors.New("keycloak admin integration not configured")

// ErrNotFound is returned when an addressed user does not exist.
var ErrNotFound = errors.New("keycloak: user not found")

// User is the wire shape exchanged with the admin UI. It is a deliberately
// small projection of Keycloak's UserRepresentation — only the fields the
// management console reads or writes.
type User struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Email     string `json:"email"`
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
	Enabled   bool   `json:"enabled"`
}

// CreateInput is the payload for Create. Password is optional: when empty the
// user is created without credentials (the operator can reset it later, or the
// user can use a Keycloak-managed flow).
type CreateInput struct {
	Username  string
	Email     string
	FirstName string
	LastName  string
	Password  string
}

// UpdateInput is a partial update for Update. All fields are pointers so a nil
// field is left untouched on the existing Keycloak user; a non-nil field is
// written. This mirrors the PUT semantics the admin UI expects.
type UpdateInput struct {
	Username  *string
	Email     *string
	FirstName *string
	LastName  *string
	Enabled   *bool
}

// Client is a Keycloak Admin REST client. A disabled client (no base URL or no
// secret) makes every method return ErrDisabled.
type Client struct {
	enabled  bool
	baseURL  string // normalised, no trailing slash, e.g. http://keycloak:8080/auth
	realm    string
	clientID string
	secret   string
	http     *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// New constructs a Client from the resolved configuration. It never returns an
// error: when baseURL or secret is empty it returns a disabled client whose
// methods return ErrDisabled, so deployments without the bundled Keycloak run
// unchanged. The base URL is normalised (trailing slash trimmed) so callers may
// pass either form.
func New(baseURL, realm, clientID, secret string) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	realm = strings.TrimSpace(realm)
	clientID = strings.TrimSpace(clientID)
	secret = strings.TrimSpace(secret)

	if baseURL == "" || secret == "" || realm == "" || clientID == "" {
		return &Client{enabled: false}
	}

	return &Client{
		enabled:  true,
		baseURL:  baseURL,
		realm:    realm,
		clientID: clientID,
		secret:   secret,
		http:     &http.Client{Timeout: 15 * time.Second},
	}
}

// Enabled reports whether the client has a base URL and credentials. Callers
// use it to skip work entirely, but every method is also guarded so calling
// them on a disabled client is safe (they return ErrDisabled).
func (c *Client) Enabled() bool {
	return c != nil && c.enabled
}

// tokenResponse is the subset of the token endpoint response we consume.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// accessToken returns a valid bearer token for the Admin API, fetching a fresh
// one via client_credentials when the cached token is missing or within 30s of
// expiry. The fetch is serialised under the mutex so concurrent callers share a
// single round-trip.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.secret)

	endpoint := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token", c.baseURL, url.PathEscape(c.realm))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer drain(resp)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("token endpoint status %s: %s", resp.Status, bytes.TrimSpace(msg))
	}

	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", errors.New("token endpoint returned empty access_token")
	}

	// Refresh 30s before the server-stated expiry to avoid using a token that
	// expires mid-request. Clamp the lifetime to a sane minimum.
	lifetime := tr.ExpiresIn - 30
	if lifetime < 5 {
		lifetime = 5
	}
	c.token = tr.AccessToken
	c.expires = time.Now().Add(time.Duration(lifetime) * time.Second)
	return c.token, nil
}

// do performs an authenticated Admin API request against an admin-realm path
// (the part after /admin/realms/{realm}). When body is non-nil it is JSON
// encoded. It returns the response for the caller to consume; the caller must
// drain/close it. A 401 invalidates the cached token so the next call re-auths.
func (c *Client) do(ctx context.Context, method, adminPath string, body any) (*http.Response, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}

	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}

	full := fmt.Sprintf("%s/admin/realms/%s%s", c.baseURL, url.PathEscape(c.realm), adminPath)
	req, err := http.NewRequestWithContext(ctx, method, full, rdr)
	if err != nil {
		return nil, fmt.Errorf("build admin request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("admin request %s %s: %w", method, adminPath, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// The cached token was rejected (rotation / clock skew). Drop it so the
		// next call re-authenticates from scratch.
		c.mu.Lock()
		c.token = ""
		c.expires = time.Time{}
		c.mu.Unlock()
	}
	return resp, nil
}

// List returns all users in the realm. Keycloak paginates with a default cap
// (100); we request a generous max so the management console sees the full set
// without re-paging. For very large realms a future change can add search/
// paging passthrough — the UI contract returns a flat array today.
func (c *Client) List(ctx context.Context) ([]User, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	resp, err := c.do(ctx, http.MethodGet, "/users?max=1000", nil)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	if err := expect(resp, http.StatusOK); err != nil {
		return nil, err
	}
	var users []User
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return nil, fmt.Errorf("decode users: %w", err)
	}
	return users, nil
}

// userRepresentation is the Keycloak UserRepresentation subset we send on
// create/update plus the credentials array used to set an initial password.
type userRepresentation struct {
	Username    string                     `json:"username,omitempty"`
	Email       string                     `json:"email,omitempty"`
	FirstName   string                     `json:"firstName,omitempty"`
	LastName    string                     `json:"lastName,omitempty"`
	Enabled     *bool                      `json:"enabled,omitempty"`
	Credentials []credentialRepresentation `json:"credentials,omitempty"`
}

type credentialRepresentation struct {
	Type      string `json:"type"`
	Value     string `json:"value"`
	Temporary bool   `json:"temporary"`
}

// Create creates a new user and returns its generated id. Keycloak returns the
// id in the Location header of the 201 response; we parse the trailing path
// segment. When a password is supplied it is set inline (non-temporary) via the
// credentials array.
func (c *Client) Create(ctx context.Context, in CreateInput) (string, error) {
	if !c.Enabled() {
		return "", ErrDisabled
	}
	enabled := true
	rep := userRepresentation{
		Username:  strings.TrimSpace(in.Username),
		Email:     strings.TrimSpace(in.Email),
		FirstName: strings.TrimSpace(in.FirstName),
		LastName:  strings.TrimSpace(in.LastName),
		Enabled:   &enabled,
	}
	if in.Password != "" {
		rep.Credentials = []credentialRepresentation{{
			Type:      "password",
			Value:     in.Password,
			Temporary: false,
		}}
	}

	resp, err := c.do(ctx, http.MethodPost, "/users", rep)
	if err != nil {
		return "", err
	}
	defer drain(resp)
	if err := expect(resp, http.StatusCreated); err != nil {
		return "", err
	}

	// The new user's id is the last path segment of the Location header.
	loc := resp.Header.Get("Location")
	if loc != "" {
		if i := strings.LastIndex(loc, "/"); i >= 0 && i+1 < len(loc) {
			return loc[i+1:], nil
		}
	}

	// Fallback: Keycloak should always set Location, but if a proxy strips it
	// we look the user up by the (unique) username to recover the id.
	id, err := c.findIDByUsername(ctx, rep.Username)
	if err != nil {
		return "", fmt.Errorf("user created but id not resolvable: %w", err)
	}
	return id, nil
}

// Update applies a partial update. It reads the current representation, overlays
// the supplied non-nil fields, and PUTs the merged user back so untouched fields
// are preserved (Keycloak's PUT replaces the representation it is given).
func (c *Client) Update(ctx context.Context, id string, in UpdateInput) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return ErrNotFound
	}

	cur, err := c.get(ctx, id)
	if err != nil {
		return err
	}

	if in.Username != nil {
		cur.Username = strings.TrimSpace(*in.Username)
	}
	if in.Email != nil {
		cur.Email = strings.TrimSpace(*in.Email)
	}
	if in.FirstName != nil {
		cur.FirstName = strings.TrimSpace(*in.FirstName)
	}
	if in.LastName != nil {
		cur.LastName = strings.TrimSpace(*in.LastName)
	}
	enabled := cur.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}

	rep := userRepresentation{
		Username:  cur.Username,
		Email:     cur.Email,
		FirstName: cur.FirstName,
		LastName:  cur.LastName,
		Enabled:   &enabled,
	}

	resp, err := c.do(ctx, http.MethodPut, "/users/"+url.PathEscape(id), rep)
	if err != nil {
		return err
	}
	defer drain(resp)
	return expect(resp, http.StatusNoContent)
}

// Delete removes a user by id.
func (c *Client) Delete(ctx context.Context, id string) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return ErrNotFound
	}
	resp, err := c.do(ctx, http.MethodDelete, "/users/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	defer drain(resp)
	return expect(resp, http.StatusNoContent)
}

// ResetPassword sets a user's password. When temporary is true Keycloak forces
// a change on next login. Used both by the USERS API reset endpoint and by
// first-run setup to seed the bundled 'admin' credentials.
func (c *Client) ResetPassword(ctx context.Context, id, password string, temporary bool) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return ErrNotFound
	}
	cred := credentialRepresentation{Type: "password", Value: password, Temporary: temporary}
	resp, err := c.do(ctx, http.MethodPut, "/users/"+url.PathEscape(id)+"/reset-password", cred)
	if err != nil {
		return err
	}
	defer drain(resp)
	return expect(resp, http.StatusNoContent)
}

// FindIDByUsername resolves a user id from an exact username. Returns ErrNotFound
// when no user matches. Exported so first-run setup can locate the bundled
// 'admin' account to seed its password.
func (c *Client) FindIDByUsername(ctx context.Context, username string) (string, error) {
	if !c.Enabled() {
		return "", ErrDisabled
	}
	return c.findIDByUsername(ctx, username)
}

func (c *Client) findIDByUsername(ctx context.Context, username string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", ErrNotFound
	}
	// exact=true so a substring match on another account can't shadow the one
	// we asked for.
	q := "/users?exact=true&username=" + url.QueryEscape(username)
	resp, err := c.do(ctx, http.MethodGet, q, nil)
	if err != nil {
		return "", err
	}
	defer drain(resp)
	if err := expect(resp, http.StatusOK); err != nil {
		return "", err
	}
	var users []User
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return "", fmt.Errorf("decode user lookup: %w", err)
	}
	for _, u := range users {
		if strings.EqualFold(u.Username, username) {
			return u.ID, nil
		}
	}
	return "", ErrNotFound
}

// get fetches a single user representation by id.
func (c *Client) get(ctx context.Context, id string) (User, error) {
	resp, err := c.do(ctx, http.MethodGet, "/users/"+url.PathEscape(id), nil)
	if err != nil {
		return User{}, err
	}
	defer drain(resp)
	if err := expect(resp, http.StatusOK); err != nil {
		return User{}, err
	}
	var u User
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return User{}, fmt.Errorf("decode user: %w", err)
	}
	return u, nil
}

// expect maps the response status to an error: nil on the wanted code,
// ErrNotFound on 404, and a status-carrying error otherwise.
func expect(resp *http.Response, want int) error {
	if resp.StatusCode == want {
		return nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("keycloak admin status %s: %s", resp.Status, bytes.TrimSpace(msg))
}

// drain consumes and closes a response body so the underlying connection can be
// reused.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}
