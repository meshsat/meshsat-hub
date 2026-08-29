package api

// Channel key management — Hub-driven key rotation + MQTT distribution. [MESHSAT-447]

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/meshsat/meshsat-hub/internal/bridge"
	hubcrypto "github.com/meshsat/meshsat-hub/internal/crypto"
	"github.com/meshsat/meshsat-hub/internal/store"
)

// ChannelKeyHandler handles channel encryption key rotation and distribution.
type ChannelKeyHandler struct {
	store     store.Store
	keyStore  *hubcrypto.KeyStore
	commander *bridge.Commander
}

// NewChannelKeyHandler creates a new channel key handler.
func NewChannelKeyHandler(s store.Store, ks *hubcrypto.KeyStore, cmdr *bridge.Commander) *ChannelKeyHandler {
	return &ChannelKeyHandler{store: s, keyStore: ks, commander: cmdr}
}

type channelKeyRotateRequest struct {
	ChannelType string   `json:"channel_type"` // "sms", "iridium", "mesh", etc.
	Address     string   `json:"address"`      // "+31653618463", "!4a20b4e0", etc.
	BridgeIDs   []string `json:"bridge_ids"`   // specific bridges, or empty for all
}

type channelKeyRotateResponse struct {
	ChannelType   string   `json:"channel_type"`
	Address       string   `json:"address"`
	KeyHex        string   `json:"key_hex"` // plaintext — shown once
	Version       int      `json:"version"`
	Distributed   int      `json:"distributed"`    // number of bridges notified
	FailedBridges []string `json:"failed_bridges"` // bridges that failed to receive
}

// RotateChannelKey generates a new AES-256 key for a channel+address and
// distributes it to all connected bridges via MQTT key_rotate command.
//
//	@Summary      Rotate channel encryption key
//	@Description  Generates a new AES-256 key, stores in Hub keystore, pushes to bridges via MQTT
//	@Tags         keys
//	@Accept       json
//	@Produce      json
//	@Success      201  {object}  channelKeyRotateResponse
//	@Failure      400  {object}  map[string]string
//	@Router       /api/keys/channel/rotate [post]
func (h *ChannelKeyHandler) RotateChannelKey(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}

	var req channelKeyRotateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.ChannelType == "" || req.Address == "" {
		writeError(w, http.StatusBadRequest, "channel_type and address are required")
		return
	}

	// Generate new AES-256 key
	rawKey := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, rawKey); err != nil {
		writeError(w, http.StatusInternalServerError, "key generation failed")
		return
	}
	keyHex := hex.EncodeToString(rawKey)

	// Store in Hub's keystore (device IMEI = channel_type:address for channel keys)
	keyID := fmt.Sprintf("%s:%s", req.ChannelType, req.Address)
	version := 1
	if h.keyStore != nil {
		if entry, err := h.keyStore.StoreKey(keyID, rawKey, "decrypt"); err != nil {
			slog.Warn("channelkeys: keystore store failed, using generated key", "error", err)
		} else if entry != nil {
			version = entry.Version
		}
	}

	slog.Info("channelkeys: key rotated",
		"channel", req.ChannelType,
		"address", req.Address,
		"version", version,
	)

	// Distribute to bridges via MQTT
	distributed := 0
	var failedBridges []string

	if h.commander != nil {
		cmd := bridge.KeyRotateCommand(req.ChannelType, req.Address, keyHex, version)

		// Get target bridges
		var bridgeIDs []string
		if len(req.BridgeIDs) > 0 {
			bridgeIDs = req.BridgeIDs
		} else {
			// All online bridges
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			bridges, err := h.store.ListBridges(ctx, store.DefaultTenantID)
			if err == nil {
				for _, b := range bridges {
					if b.Online {
						bridgeIDs = append(bridgeIDs, b.BridgeID)
					}
				}
			}
		}

		for _, bid := range bridgeIDs {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			resp, err := h.commander.SendCommand(ctx, bid, cmd)
			cancel()
			if err != nil {
				slog.Warn("channelkeys: key_rotate failed for bridge",
					"bridge", bid, "error", err)
				failedBridges = append(failedBridges, bid)
			} else if resp != nil && resp.Status == "ok" {
				distributed++
				slog.Info("channelkeys: key distributed",
					"bridge", bid, "channel", req.ChannelType, "address", req.Address)
			} else {
				failedBridges = append(failedBridges, bid)
			}
		}
	}

	resp := channelKeyRotateResponse{
		ChannelType:   req.ChannelType,
		Address:       req.Address,
		KeyHex:        keyHex,
		Version:       version,
		Distributed:   distributed,
		FailedBridges: failedBridges,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}
