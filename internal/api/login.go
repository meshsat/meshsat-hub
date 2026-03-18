package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cubeos-app/meshsat-hub/internal/audit"
	hubauth "github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/store"
)

// LoginHandler handles user authentication with local accounts.
type LoginHandler struct {
	store    store.Store
	sessions *hubauth.SessionManager
	audit    *audit.Service

	// Per-IP rate limiting for login attempts.
	// Map of IP → (attempts, window_start). Reset after window expires.
	mu         sync.Mutex
	ipAttempts map[string]*loginAttempt
	maxPerIP   int
	windowDur  time.Duration
}

type loginAttempt struct {
	count     int
	windowEnd time.Time
}

// NewLoginHandler creates a login handler with per-IP rate limiting.
func NewLoginHandler(s store.Store, sm *hubauth.SessionManager, auditSvc *audit.Service) *LoginHandler {
	return &LoginHandler{
		store:      s,
		sessions:   sm,
		audit:      auditSvc,
		ipAttempts: make(map[string]*loginAttempt),
		maxPerIP:   5,
		windowDur:  15 * time.Minute,
	}
}

// loginRequest is the POST /api/auth/login body.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// loginResponse is returned on successful authentication.
type loginResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"` // seconds
	TokenType    string `json:"token_type"`
}

// Login authenticates a user with email/password and returns JWT + refresh token.
// POST /api/auth/login
func (h *LoginHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email and password are required"})
		return
	}

	clientIP := r.RemoteAddr
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		clientIP = strings.Split(fwd, ",")[0]
	}

	// Per-IP rate limiting
	if !h.allowIP(clientIP) {
		slog.Warn("auth: login rate limited", "ip", clientIP)
		h.auditLog(r, "login_rate_limited", req.Email, clientIP)
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many login attempts, try again later"})
		return
	}

	tenantID := hubauth.TenantIDFromContext(r.Context())

	// Look up user by email
	user, err := h.store.GetUserByEmail(r.Context(), tenantID, req.Email)
	if err != nil {
		// Constant-time: always hash even on user-not-found to prevent timing oracle
		_, _ = hubauth.HashPassword(req.Password)
		h.recordIPAttempt(clientIP)
		h.auditLog(r, "login_failed", req.Email, clientIP)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid email or password"})
		return
	}

	// Check account lockout
	if !user.LockedUntil.IsZero() && time.Now().Before(user.LockedUntil) {
		slog.Warn("auth: locked account login attempt", "email", req.Email, "locked_until", user.LockedUntil)
		h.auditLog(r, "login_locked", req.Email, clientIP)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "account is temporarily locked"})
		return
	}

	// Check enabled
	if !user.Enabled {
		h.auditLog(r, "login_disabled", req.Email, clientIP)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "account is disabled"})
		return
	}

	// Verify password
	ok, err := hubauth.VerifyPassword(req.Password, user.PasswordHash)
	if err != nil || !ok {
		count, _ := h.store.IncrementFailedLogins(r.Context(), tenantID, user.ID)
		h.recordIPAttempt(clientIP)
		h.auditLog(r, "login_failed", req.Email, clientIP)
		if count >= store.MaxFailedLogins {
			slog.Warn("auth: account locked after failed attempts", "email", req.Email, "attempts", count)
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid email or password"})
		return
	}

	// Success — reset failed logins
	_ = h.store.ResetFailedLogins(r.Context(), tenantID, user.ID)

	// Issue access token
	accessToken, err := h.sessions.IssueAccessToken(user.ID, user.Email, user.Name, user.Role, tenantID)
	if err != nil {
		slog.Error("auth: failed to issue access token", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Issue refresh token
	refreshPlain, refreshHash, err := hubauth.GenerateRefreshToken()
	if err != nil {
		slog.Error("auth: failed to generate refresh token", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	rtID, _ := generateID()
	rt := &store.RefreshToken{
		ID:        rtID,
		UserID:    user.ID,
		TenantID:  tenantID,
		TokenHash: refreshHash,
		ExpiresAt: time.Now().UTC().Add(hubauth.RefreshTokenTTL),
		CreatedAt: time.Now().UTC(),
	}
	if err := h.store.StoreRefreshToken(r.Context(), tenantID, rt); err != nil {
		slog.Error("auth: failed to store refresh token", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Set refresh token as HttpOnly cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "meshsat_refresh",
		Value:    refreshPlain,
		Path:     "/api/auth",
		MaxAge:   int(hubauth.RefreshTokenTTL.Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteStrictMode,
	})

	h.auditLog(r, "login_success", req.Email, clientIP)
	slog.Info("auth: login success", "email", req.Email, "role", user.Role)

	writeJSON(w, http.StatusOK, loginResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshPlain,
		ExpiresIn:    int(hubauth.AccessTokenTTL.Seconds()),
		TokenType:    "Bearer",
	})
}

// Refresh exchanges a valid refresh token for a new access token + rotated refresh token.
// POST /api/auth/refresh
func (h *LoginHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	// Get refresh token from cookie or body
	var refreshToken string
	if cookie, err := r.Cookie("meshsat_refresh"); err == nil {
		refreshToken = cookie.Value
	}
	if refreshToken == "" {
		var req struct {
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err == nil {
			refreshToken = req.RefreshToken
		}
	}
	if refreshToken == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "refresh token required"})
		return
	}

	tokenHash := hubauth.HashRefreshToken(refreshToken)
	rt, err := h.store.GetRefreshToken(r.Context(), tokenHash)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid refresh token"})
		return
	}

	// Check expiry
	if time.Now().After(rt.ExpiresAt) {
		_ = h.store.DeleteRefreshToken(r.Context(), tokenHash)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "refresh token expired"})
		return
	}

	// Delete old refresh token (rotation — each token is single-use)
	_ = h.store.DeleteRefreshToken(r.Context(), tokenHash)

	// Look up user
	user, err := h.store.GetUserByID(r.Context(), rt.TenantID, rt.UserID)
	if err != nil || !user.Enabled {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "user not found or disabled"})
		return
	}

	// Issue new access token
	accessToken, err := h.sessions.IssueAccessToken(user.ID, user.Email, user.Name, user.Role, rt.TenantID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Issue new refresh token (rotation)
	newPlain, newHash, _ := hubauth.GenerateRefreshToken()
	newID, _ := generateID()
	newRT := &store.RefreshToken{
		ID:        newID,
		UserID:    user.ID,
		TenantID:  rt.TenantID,
		TokenHash: newHash,
		ExpiresAt: time.Now().UTC().Add(hubauth.RefreshTokenTTL),
		CreatedAt: time.Now().UTC(),
	}
	_ = h.store.StoreRefreshToken(r.Context(), rt.TenantID, newRT)

	http.SetCookie(w, &http.Cookie{
		Name:     "meshsat_refresh",
		Value:    newPlain,
		Path:     "/api/auth",
		MaxAge:   int(hubauth.RefreshTokenTTL.Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteStrictMode,
	})

	writeJSON(w, http.StatusOK, loginResponse{
		AccessToken:  accessToken,
		RefreshToken: newPlain,
		ExpiresIn:    int(hubauth.AccessTokenTTL.Seconds()),
		TokenType:    "Bearer",
	})
}

// Logout invalidates all refresh tokens for the current user.
// POST /api/auth/logout
func (h *LoginHandler) Logout(w http.ResponseWriter, r *http.Request) {
	user := hubauth.FromContext(r.Context())
	if user != nil {
		tenantID := hubauth.TenantIDFromContext(r.Context())
		_ = h.store.DeleteRefreshTokensByUser(r.Context(), tenantID, user.ID)
	}

	// Clear refresh cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "meshsat_refresh",
		Value:    "",
		Path:     "/api/auth",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})

	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

// IP rate limiting

func (h *LoginHandler) allowIP(ip string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()
	a, ok := h.ipAttempts[ip]
	if !ok || now.After(a.windowEnd) {
		h.ipAttempts[ip] = &loginAttempt{count: 0, windowEnd: now.Add(h.windowDur)}
		return true
	}
	return a.count < h.maxPerIP
}

func (h *LoginHandler) recordIPAttempt(ip string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if a, ok := h.ipAttempts[ip]; ok {
		a.count++
	}
}

func (h *LoginHandler) auditLog(r *http.Request, action, email, ip string) {
	if h.audit == nil {
		return
	}
	tenantID := hubauth.TenantIDFromContext(r.Context())
	_ = h.audit.Log(r.Context(), tenantID, action, email, fmt.Sprintf("email=%s ip=%s", email, ip), ip)
}

func generateID() (string, error) {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	return hex.EncodeToString(b), err
}
