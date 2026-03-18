package api

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	hubauth "github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/store"
	"github.com/go-chi/chi/v5"
)

// UserHandler provides owner-only CRUD for local user accounts.
type UserHandler struct {
	store store.Store
}

// NewUserHandler creates a user management handler.
func NewUserHandler(s store.Store) *UserHandler {
	return &UserHandler{store: s}
}

type createUserRequest struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	Password string `json:"password"`
	Role     string `json:"role"` // "viewer", "operator", "owner"
}

type updateUserRequest struct {
	Name     string `json:"name,omitempty"`
	Role     string `json:"role,omitempty"`
	Enabled  *bool  `json:"enabled,omitempty"`
	Password string `json:"password,omitempty"` // optional — only set if changing
}

type userResponse struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	Enabled     bool   `json:"enabled"`
	LastLoginAt string `json:"last_login_at,omitempty"`
	CreatedAt   string `json:"created_at"`
}

// CreateUser registers a new local user (owner-only, invite model).
// POST /api/users
func (h *UserHandler) CreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := readJSON(w, r, &req, 4096); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email is required"})
		return
	}
	if !strings.Contains(req.Email, "@") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid email address"})
		return
	}

	if err := hubauth.ValidatePasswordStrength(req.Password); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	role := req.Role
	if role == "" {
		role = "viewer"
	}
	if role != "viewer" && role != "operator" && role != "owner" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role must be viewer, operator, or owner"})
		return
	}

	tenantID := hubauth.TenantIDFromContext(r.Context())

	// Check if email already exists
	if existing, _ := h.store.GetUserByEmail(r.Context(), tenantID, req.Email); existing != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "email already registered"})
		return
	}

	hash, err := hubauth.HashPassword(req.Password)
	if err != nil {
		slog.Error("auth: failed to hash password", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	id, _ := generateUserID()
	now := time.Now().UTC()
	user := &store.LocalUser{
		ID:           id,
		Email:        req.Email,
		Name:         req.Name,
		PasswordHash: hash,
		Role:         role,
		Enabled:      true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if err := h.store.CreateUser(r.Context(), tenantID, user); err != nil {
		slog.Error("auth: failed to create user", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create user"})
		return
	}

	slog.Info("auth: user created", "email", req.Email, "role", role, "id", id)
	writeJSON(w, http.StatusCreated, toUserResponse(user))
}

// ListUsers returns all local users for the tenant.
// GET /api/users
func (h *UserHandler) ListUsers(w http.ResponseWriter, r *http.Request) {
	tenantID := hubauth.TenantIDFromContext(r.Context())
	users, err := h.store.ListUsers(r.Context(), tenantID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list users"})
		return
	}

	resp := make([]userResponse, len(users))
	for i, u := range users {
		resp[i] = toUserResponse(&u)
	}
	writeJSON(w, http.StatusOK, resp)
}

// GetUser returns a single user by ID.
// GET /api/users/{id}
func (h *UserHandler) GetUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := hubauth.TenantIDFromContext(r.Context())

	user, err := h.store.GetUserByID(r.Context(), tenantID, id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
		return
	}
	writeJSON(w, http.StatusOK, toUserResponse(user))
}

// UpdateUser modifies a user's name, role, enabled status, or password.
// PUT /api/users/{id}
func (h *UserHandler) UpdateUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := hubauth.TenantIDFromContext(r.Context())

	var req updateUserRequest
	if err := readJSON(w, r, &req, 4096); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	user, err := h.store.GetUserByID(r.Context(), tenantID, id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
		return
	}

	if req.Name != "" {
		user.Name = req.Name
	}
	if req.Role != "" {
		if req.Role != "viewer" && req.Role != "operator" && req.Role != "owner" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role must be viewer, operator, or owner"})
			return
		}
		user.Role = req.Role
	}
	if req.Enabled != nil {
		user.Enabled = *req.Enabled
	}
	if req.Password != "" {
		if err := hubauth.ValidatePasswordStrength(req.Password); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		hash, err := hubauth.HashPassword(req.Password)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		user.PasswordHash = hash
		// Invalidate all refresh tokens when password changes
		_ = h.store.DeleteRefreshTokensByUser(r.Context(), tenantID, id)
	}

	if err := h.store.UpdateUser(r.Context(), tenantID, user); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update user"})
		return
	}

	slog.Info("auth: user updated", "id", id, "role", user.Role, "enabled", user.Enabled)
	writeJSON(w, http.StatusOK, toUserResponse(user))
}

// DeleteUser removes a user and all their refresh tokens.
// DELETE /api/users/{id}
func (h *UserHandler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := hubauth.TenantIDFromContext(r.Context())

	// Prevent self-deletion
	caller := hubauth.FromContext(r.Context())
	if caller != nil && caller.ID == id {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot delete your own account"})
		return
	}

	_ = h.store.DeleteRefreshTokensByUser(r.Context(), tenantID, id)
	if err := h.store.DeleteUser(r.Context(), tenantID, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete user"})
		return
	}

	slog.Info("auth: user deleted", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

func toUserResponse(u *store.LocalUser) userResponse {
	lastLogin := ""
	if !u.LastLoginAt.IsZero() {
		lastLogin = u.LastLoginAt.Format(time.RFC3339)
	}
	return userResponse{
		ID:          u.ID,
		Email:       u.Email,
		Name:        u.Name,
		Role:        u.Role,
		Enabled:     u.Enabled,
		LastLoginAt: lastLogin,
		CreatedAt:   u.CreatedAt.Format(time.RFC3339),
	}
}

func generateUserID() (string, error) {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	return "usr_" + hex.EncodeToString(b), err
}
