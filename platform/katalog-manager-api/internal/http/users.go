package http

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/nalet/stube/platform/katalog-manager-api/internal/keycloak"
)

// The user-management surface (GET/POST/PUT/DELETE /api/manage/users and the
// reset-password sub-resource) is a thin, auth-gated proxy over the Keycloak
// Admin REST API. The admin UI consumes it to manage platform users without
// embedding admin credentials in the browser. Every handler maps
// keycloak.ErrDisabled -> 503 and keycloak.ErrNotFound -> 404 so a deployment
// without the bundled Keycloak degrades cleanly instead of 500-ing.

// createUserRequest is the body of POST /api/manage/users. Password is
// optional — when omitted the user is created without credentials.
type createUserRequest struct {
	Username  string `json:"username"`
	Email     string `json:"email"`
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
	Password  string `json:"password,omitempty"`
	// Admin grants the stube-admin realm role on creation so the new account can
	// use the /manage console. Omitted / false => a plain end user.
	Admin bool `json:"admin,omitempty"`
}

// updateUserRequest is the body of PUT /api/manage/users/{id}. All fields are
// pointers so the update is partial: an omitted field is left untouched.
type updateUserRequest struct {
	Username  *string `json:"username,omitempty"`
	Email     *string `json:"email,omitempty"`
	FirstName *string `json:"firstName,omitempty"`
	LastName  *string `json:"lastName,omitempty"`
	Enabled   *bool   `json:"enabled,omitempty"`
	// Admin, when present, promotes (true) or demotes (false) the account via
	// the stube-admin realm role. Omitted leaves role mappings untouched.
	Admin *bool `json:"admin,omitempty"`
}

// resetPasswordRequest is the body of POST /api/manage/users/{id}/reset-password.
// temporary defaults to false (a permanent password) when omitted.
type resetPasswordRequest struct {
	Password  string `json:"password"`
	Temporary bool   `json:"temporary,omitempty"`
}

// ListUsers returns every platform user as a flat array.
func (a *API) ListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.Keycloak.List(r.Context())
	if writeKeycloakErr(w, r, "list users", err) {
		return
	}
	writeJSON(w, http.StatusOK, users)
}

// CreateUser provisions a new user and returns its generated id.
func (a *API) CreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	req.Email = strings.TrimSpace(req.Email)
	if req.Username == "" {
		writeErr(w, http.StatusBadRequest, "username is required")
		return
	}

	id, err := a.Keycloak.Create(r.Context(), keycloak.CreateInput{
		Username:  req.Username,
		Email:     req.Email,
		FirstName: strings.TrimSpace(req.FirstName),
		LastName:  strings.TrimSpace(req.LastName),
		Password:  req.Password,
		Admin:     req.Admin,
	})
	if writeKeycloakErr(w, r, "create user", err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// UpdateUser applies a partial update to one user.
func (a *API) UpdateUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req updateUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	err := a.Keycloak.Update(r.Context(), id, keycloak.UpdateInput{
		Username:  req.Username,
		Email:     req.Email,
		FirstName: req.FirstName,
		LastName:  req.LastName,
		Enabled:   req.Enabled,
		Admin:     req.Admin,
	})
	if writeKeycloakErr(w, r, "update user", err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteUser removes a user. 204 on success, 404 when the id is unknown.
func (a *API) DeleteUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	err := a.Keycloak.Delete(r.Context(), id)
	if writeKeycloakErr(w, r, "delete user", err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ResetUserPassword sets a user's password (optionally temporary).
func (a *API) ResetUserPassword(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req resetPasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Password == "" {
		writeErr(w, http.StatusBadRequest, "password is required")
		return
	}
	err := a.Keycloak.ResetPassword(r.Context(), id, req.Password, req.Temporary)
	if writeKeycloakErr(w, r, "reset password", err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeKeycloakErr collapses the Keycloak error fan-out into one helper.
// Returns true when it handled the error (caller must return).
func writeKeycloakErr(w http.ResponseWriter, r *http.Request, op string, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, keycloak.ErrDisabled):
		writeErr(w, http.StatusServiceUnavailable, "user management not configured")
	case errors.Is(err, keycloak.ErrNotFound):
		writeErr(w, http.StatusNotFound, "user not found")
	default:
		slog.Error("keycloak call failed", "op", op, "path", r.URL.Path, "err", err)
		writeErr(w, http.StatusBadGateway, "identity provider error")
	}
	return true
}
