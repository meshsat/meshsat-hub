package api

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/cubeos-app/meshsat-hub/internal/auth"
	"github.com/cubeos-app/meshsat-hub/internal/directory"
)

// Directory vCard + CSV import / export — Hub side. Writes go
// through the existing DirectoryHandler.store (directory.Store) with
// caller tenant from context. Always bumps the tenant's directory
// version so the next directory_push snapshot (MESHSAT-540) carries
// the imported contacts to every bridge. [MESHSAT-541]
//
// The wire format is identical to the bridge-side implementation so
// Hub-originated .vcf files drop straight into a bridge's bulk
// import and vice versa.

// @Summary Import contacts from vCard 4.0
// @Description Uploads a .vcf file (Content-Type: text/vcard) and
// @Description creates directory_contacts + directory_addresses rows
// @Description for each record. X-MESHSAT-* extensions cover the
// @Description bearer kinds that standard vCard does not
// @Description (MESHTASTIC, APRS, IRIDIUM_SBD/IMT, CELLULAR, TAK,
// @Description RETICULUM, ZIGBEE, BLE, WEBHOOK, MQTT) plus team /
// @Description role / SIDC / trust-level metadata. Response reports
// @Description parsed / imported / errors counts.
// @Tags directory
// @Accept text/vcard
// @Produce json
// @Param body body string true "vCard 4.0 text (one or more BEGIN/END records)"
// @Success 200 {object} map[string]int
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/v1/directory/import/vcard [post]
func (h *DirectoryHandler) ImportVCard(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	contacts, err := directory.ParseVCards(bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadRequest, "parse vcard: "+err.Error())
		return
	}
	imported, errored := h.importContacts(w, r, tid, contacts)
	writeJSON(w, http.StatusOK, map[string]int{
		"parsed":   len(contacts),
		"imported": imported,
		"errors":   errored,
	})
}

// @Summary Import contacts from CSV
// @Description Uploads a CSV file with header row:
// @Description `display_name,sms,meshtastic,aprs,email,team,role`.
// @Description Unknown columns ignored; missing columns treated as
// @Description empty. One contact per data row.
// @Tags directory
// @Accept text/csv
// @Produce json
// @Param body body string true "CSV with header row"
// @Success 200 {object} map[string]int
// @Failure 400 {object} map[string]string
// @Router /api/v1/directory/import/csv [post]
func (h *DirectoryHandler) ImportCSV(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	reader := csv.NewReader(io.LimitReader(r.Body, 10<<20))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		writeError(w, http.StatusBadRequest, "parse csv: "+err.Error())
		return
	}
	if len(records) < 2 {
		writeError(w, http.StatusBadRequest, "csv must have a header row plus at least one record")
		return
	}
	headers := records[0]
	colIdx := map[string]int{}
	for i, hdr := range headers {
		colIdx[strings.ToLower(strings.TrimSpace(hdr))] = i
	}
	contacts := make([]directory.Contact, 0, len(records)-1)
	for _, row := range records[1:] {
		cell := func(key string) string {
			i, ok := colIdx[key]
			if !ok || i >= len(row) {
				return ""
			}
			return strings.TrimSpace(row[i])
		}
		name := cell("display_name")
		if name == "" {
			continue
		}
		c := directory.Contact{
			DisplayName: name,
			Team:        cell("team"),
			Role:        cell("role"),
			Origin:      directory.OriginImported,
		}
		csvAppend := func(k directory.AddressKind, col string) {
			v := cell(col)
			if v == "" {
				return
			}
			c.Addresses = append(c.Addresses, directory.Address{
				Kind:        k,
				Value:       v,
				PrimaryRank: 0,
			})
		}
		csvAppend(directory.KindSMS, "sms")
		csvAppend(directory.KindMeshtastic, "meshtastic")
		csvAppend(directory.KindAPRS, "aprs")
		csvAppend(directory.KindEmail, "email")
		contacts = append(contacts, c)
	}
	imported, errored := h.importContacts(w, r, tid, contacts)
	writeJSON(w, http.StatusOK, map[string]int{
		"parsed":   len(contacts),
		"imported": imported,
		"errors":   errored,
	})
}

// importContacts loops over parsed contacts and Puts them through
// the tenant-scoped store. A UNIQUE(kind,value) collision (bridge
// v45 constraint — Hub mirrors it) is a soft skip: the contact is
// rolled back and the import continues. Bumps the tenant's directory
// version exactly once at the end so a single directory_push covers
// the whole import on the next fan-out.
func (h *DirectoryHandler) importContacts(w http.ResponseWriter, r *http.Request, tenantID string, contacts []directory.Contact) (imported int, errored int) {
	for i := range contacts {
		c := &contacts[i]
		c.TenantID = tenantID
		if c.Origin == "" {
			c.Origin = directory.OriginImported
		}
		if err := h.store.PutContact(r.Context(), c); err != nil {
			if errors.Is(err, directory.ErrInvalidAddressKind) {
				errored++
				continue
			}
			errored++
			continue
		}
		imported++
	}
	if imported > 0 {
		_, _ = h.store.BumpVersion(r.Context(), tenantID)
	}
	return imported, errored
}

// @Summary Export tenant contacts as vCard 4.0
// @Description Streams every contact in the caller's tenant as a
// @Description concatenated vCard 4.0 document. X-MESHSAT-*
// @Description extensions carry the bearer-kind metadata not covered
// @Description by standard TEL/EMAIL.
// @Tags directory
// @Produce text/vcard
// @Param limit query integer false "Max contacts (default 10000)"
// @Success 200 {string} string "vCard 4.0 text"
// @Failure 500 {object} map[string]string
// @Router /api/v1/directory/export/vcard [get]
func (h *DirectoryHandler) ExportVCard(w http.ResponseWriter, r *http.Request) {
	tid := auth.TenantIDFromContext(r.Context())
	limit := 10000
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	contacts, err := h.store.ListContacts(r.Context(), tid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(contacts) > limit {
		contacts = contacts[:limit]
	}

	var buf bytes.Buffer
	if err := directory.WriteVCards(&buf, contacts); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/vcard; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="meshsat-directory-%s-%d.vcf"`, tid, len(contacts)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
