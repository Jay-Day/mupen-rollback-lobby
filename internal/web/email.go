package web

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/accounts"
)

// EnableEmail turns on the password reset email endpoints. requestReset
// emails an account's owner a reset code when the address matches
// (ws.Hub.RequestPasswordResetEmail) and returns a wait > 0 when the IP has
// asked too often.
func (s *Server) EnableEmail(requestReset func(username, email, ip string) time.Duration) {
	s.requestReset = requestReset
}

// handleEmailConfig tells RMG-K whether to offer emailing a reset code. It's
// a 404 when the server has no email.
func (s *Server) handleEmailConfig(w http.ResponseWriter, r *http.Request) {
	if s.requestReset == nil {
		writeError(w, r, http.StatusNotFound, "this server can't send email")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"passwordReset": true,
		"codeMinutes":   int(accounts.EmailCodeTTL / time.Minute),
	})
}

// maxResetRequestBody caps POST /api/auth/password-reset bodies.
const maxResetRequestBody = 1 << 10

// handlePasswordResetRequest emails a reset code to the named account's
// confirmed address, if the request names that address too: a username alone
// can't send reset emails to someone's inbox. Every well-formed request gets
// the same answer, so it doesn't reveal which accounts exist or their
// addresses.
func (s *Server) handlePasswordResetRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.requestReset == nil {
		writeError(w, r, http.StatusNotFound, "this server can't send email")
		return
	}
	// JSON only: a browser can't send it from another site without a CORS
	// preflight, which this server never approves, so other sites can't make
	// their visitors trigger emails.
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil ||
		mediaType != "application/json" {
		writeError(w, r, http.StatusUnsupportedMediaType, "send the request as JSON")
		return
	}
	var req struct {
		Username string `json:"username"`
		Email    string `json:"email"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxResetRequestBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		strings.TrimSpace(req.Username) == "" || strings.TrimSpace(req.Email) == "" {
		writeError(w, r, http.StatusBadRequest, "enter your username and the email address on your account")
		return
	}
	if wait := s.requestReset(req.Username, req.Email, s.remoteIP(r)); wait > 0 {
		minutes := int((wait + time.Minute - 1) / time.Minute)
		unit := "minutes"
		if minutes <= 1 {
			minutes, unit = 1, "minute"
		}
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds()+0.999)))
		writeError(w, r, http.StatusTooManyRequests,
			fmt.Sprintf("Too many reset requests from your network. Try again in %d %s.", minutes, unit))
		return
	}
	writeJSON(w, r, http.StatusAccepted, map[string]any{
		"ok":      true,
		"message": "If that's the account's confirmed email address, a reset code is on its way.",
	})
}
