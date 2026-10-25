package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamarindTech/TamarindLOS/services/core/internal/connectors"
)

// Document pipeline (Spec 03, documents catalogue):
//
//	document.upload_url -> PUT /v1/uploads/{token} -> document.attach
//	  -> document.classify -> document.extract -> evidence.verify
//
// Files are stored once, hashed and scanned before they can be attached. Classified
// documents count towards completeness. Extracted values become evidence with a
// confidence score; agents may verify evidence only above the pack's threshold.

const (
	uploadTTL      = 15 * time.Minute
	maxUploadBytes = 25 << 20 // 25 MiB

	ErrUploadNotFound   = "UPLOAD_NOT_FOUND"
	ErrMalwareDetected  = "MALWARE_DETECTED"
	ErrUnsupportedFile  = "UNSUPPORTED_FILE"
	ErrExtractionFailed = "EXTRACTION_FAILED"
)

// newUploadToken returns a token that names its tenant (so the unauthenticated upload
// endpoint can open the right tenant scope) and 256 random bits.
func newUploadToken(tenantID string) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return "up_" + strings.ReplaceAll(tenantID, "-", "") + "_" + hex.EncodeToString(b)
}

func tenantFromToken(token string) (string, bool) {
	parts := strings.Split(token, "_")
	if len(parts) != 3 || parts[0] != "up" || len(parts[1]) != 32 || len(parts[2]) != 64 {
		return "", false
	}
	if _, err := hex.DecodeString(parts[1] + parts[2]); err != nil {
		return "", false
	}
	t := parts[1]
	return t[0:8] + "-" + t[8:12] + "-" + t[12:16] + "-" + t[16:20] + "-" + t[20:32], true
}

// document.upload_url
func (g *Gateway) documentUploadURL(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	partyID := str(c.input, "party_id")
	if partyID != "" && c.app.party(partyID) == nil {
		return nil, newErr(ErrNotFound, "party %s is not on this application", partyID)
	}
	maxBytes := int64(maxUploadBytes)
	if n, ok := num(c.input, "size_bytes"); ok && n > 0 && n < maxBytes {
		maxBytes = n
	}
	token := newUploadToken(c.tenantID)
	expires := g.Now().UTC().Add(uploadTTL).Truncate(time.Second)
	if _, err := tx.Exec(ctx, `INSERT INTO uploads (id, tenant_id, application_id, party_id, token_sha256, file_name, mime_type,
		max_bytes, created_by, expires_at) VALUES ($1,$2,$3,NULLIF($4,'')::uuid,$5,$6,$7,$8,$9,$10)`,
		newUUID(), c.tenantID, c.app.ID, partyID, sha256Hex([]byte(token)), str(c.input, "file_name"),
		str(c.input, "mime_type"), maxBytes, c.p.ID, expires); err != nil {
		return nil, err
	}
	return map[string]any{"upload_url": "/v1/uploads/" + token, "upload_token": token, "method": "PUT",
		"max_bytes": maxBytes, "expires_at": expires.Format(time.RFC3339)}, nil
}

// UploadError is returned by ReceiveUpload with its HTTP status.
type UploadError struct {
	HTTP int
	*ToolError
}

// ReceiveUpload stores the body of PUT /v1/uploads/{token}. The token is the credential.
func (g *Gateway) ReceiveUpload(ctx context.Context, token string, body io.Reader) (map[string]any, *UploadError) {
	tenantID, ok := tenantFromToken(token)
	notFound := &UploadError{http.StatusNotFound, newErr(ErrUploadNotFound, "upload link not found or expired")}
	if !ok {
		return nil, notFound
	}
	var id, appID, mime, status string
	var maxBytes int64
	var expires time.Time
	err := withTenantTx(ctx, g.DB, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id::text, application_id::text, mime_type, max_bytes, status, expires_at
			FROM uploads WHERE token_sha256 = $1`, sha256Hex([]byte(token))).Scan(&id, &appID, &mime, &maxBytes, &status, &expires)
	})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && g.Now().After(expires)) {
		return nil, notFound
	}
	if err != nil {
		g.Log.Error("upload lookup", "err", err)
		return nil, &UploadError{http.StatusInternalServerError, newErr(ErrInternal, "internal error")}
	}
	if status != "pending" {
		return nil, &UploadError{http.StatusConflict, newErr(ErrStateConflict, "a file was already uploaded with this link")}
	}

	key := tenantID + "/" + appID + "/" + id
	sha, size, err := g.Connectors.Blobs.Put(ctx, key, body, maxBytes)
	if errors.Is(err, connectors.ErrTooLarge) {
		return nil, &UploadError{http.StatusRequestEntityTooLarge, newErr(ErrValidation, "file is larger than %d bytes", maxBytes)}
	}
	if err != nil {
		g.Log.Error("blob store", "err", err)
		return nil, &UploadError{http.StatusInternalServerError, newErr(ErrInternal, "could not store the file")}
	}
	newStatus, reason := "uploaded", ""
	content, err := g.Connectors.Blobs.Get(ctx, key)
	if err != nil {
		return nil, &UploadError{http.StatusInternalServerError, newErr(ErrInternal, "could not read the file back")}
	}
	if size == 0 || !matchesMime(content, mime) {
		newStatus, reason = "rejected", "unsupported_file"
	} else if clean, finding, err := g.Connectors.Malware.Scan(ctx, content); err != nil {
		return nil, &UploadError{http.StatusServiceUnavailable, newErr(ErrProviderUnavailable, "malware scanner unavailable")}
	} else if !clean {
		newStatus, reason = "rejected", "malware: "+finding
	}
	err = withTenantTx(ctx, g.DB, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE uploads SET status = $2, reject_reason = NULLIF($3,''), storage_key = $4, sha256 = $5,
			size_bytes = $6, uploaded_at = now() WHERE id = $1 AND status = 'pending'`, id, newStatus, reason, key, sha, size)
		if err == nil && tag.RowsAffected() == 0 {
			return errReplayed // raced with another upload on the same link
		}
		return err
	})
	if errors.Is(err, errReplayed) {
		return nil, &UploadError{http.StatusConflict, newErr(ErrStateConflict, "a file was already uploaded with this link")}
	}
	if err != nil {
		return nil, &UploadError{http.StatusInternalServerError, newErr(ErrInternal, "internal error")}
	}
	if newStatus == "rejected" {
		code := ErrUnsupportedFile
		if strings.HasPrefix(reason, "malware") {
			code = ErrMalwareDetected
		}
		return nil, &UploadError{http.StatusUnprocessableEntity, newErr(code, "the file was rejected (%s)", reason)}
	}
	return map[string]any{"status": "uploaded", "sha256": sha, "size_bytes": size}, nil
}

// matchesMime checks the file's leading bytes against its declared type.
func matchesMime(b []byte, mime string) bool {
	switch mime {
	case "application/pdf":
		return bytes.HasPrefix(b, []byte("%PDF-"))
	case "image/jpeg":
		return bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF})
	case "image/png":
		return bytes.HasPrefix(b, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'})
	case "image/heic":
		return len(b) > 12 && string(b[4:8]) == "ftyp"
	}
	return false
}

// document.attach
func (g *Gateway) documentAttach(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	token := str(c.input, "upload_token")
	var uploadID, appID, partyID, fileName, mime, status, reason, key, sha string
	var size int64
	var expires time.Time
	err := tx.QueryRow(ctx, `SELECT id::text, application_id::text, COALESCE(party_id::text,''), file_name, mime_type, status,
		COALESCE(reject_reason,''), COALESCE(storage_key,''), COALESCE(sha256,''), COALESCE(size_bytes,0), expires_at
		FROM uploads WHERE token_sha256 = $1 FOR UPDATE`, sha256Hex([]byte(token))).
		Scan(&uploadID, &appID, &partyID, &fileName, &mime, &status, &reason, &key, &sha, &size, &expires)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && appID != c.app.ID) {
		return nil, newErr(ErrUploadNotFound, "no upload with this token for the application")
	}
	if err != nil {
		return nil, err
	}
	switch status {
	case "pending":
		return nil, newErr(ErrUploadNotFound, "nothing has been uploaded with this token yet")
	case "attached":
		return nil, newErr(ErrUploadNotFound, "this upload is already attached")
	case "rejected":
		if strings.HasPrefix(reason, "malware") {
			return nil, newErr(ErrMalwareDetected, "the file was rejected by the malware scan")
		}
		return nil, newErr(ErrUnsupportedFile, "the file content does not match %s", mime)
	}
	if p := str(c.input, "party_id"); p != "" {
		partyID = p
	}
	if partyID != "" && c.app.party(partyID) == nil {
		return nil, newErr(ErrNotFound, "party %s is not on this application", partyID)
	}
	declared := str(c.input, "declared_type")
	docID := newUUID()
	if _, err := tx.Exec(ctx, `INSERT INTO documents (id, tenant_id, application_id, party_id, declared_type, file_name, status,
		storage_key, sha256, mime_type, size_bytes, uploaded_by) VALUES ($1,$2,$3,NULLIF($4,'')::uuid,NULLIF($5,''),$6,'received',$7,$8,$9,$10,$11)`,
		docID, c.tenantID, c.app.ID, partyID, declared, fileName, key, sha, mime, size, c.p.ID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE uploads SET status = 'attached', document_id = $2 WHERE id = $1`, uploadID, docID); err != nil {
		return nil, err
	}
	c.addEvent("DocumentAttached", map[string]any{"document_id": docID, "party_id": partyID, "declared_type": declared,
		"mime_type": mime, "sha256": sha})
	return map[string]any{"document_id": docID, "sha256": sha}, nil
}

func (g *Gateway) documentContent(ctx context.Context, d *documentRecord) ([]byte, error) {
	b, err := g.Connectors.Blobs.Get(ctx, d.StorageKey)
	if err != nil {
		return nil, err
	}
	if sha256Hex(b) != d.SHA256 {
		return nil, fmt.Errorf("document %s: stored content does not match its hash", d.ID)
	}
	return b, nil
}

// document.classify
func (g *Gateway) documentClassify(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	d := c.app.document(str(c.input, "document_id"))
	if d == nil {
		return nil, newErr(ErrNotFound, "document not found")
	}
	if d.Status == "rejected" {
		return nil, newErr(ErrStateConflict, "document was rejected")
	}
	content, err := g.documentContent(ctx, d)
	if err != nil {
		return nil, err
	}
	cl, err := g.Connectors.Classify.Classify(ctx, content, d.FileName, d.DeclaredType)
	if err != nil {
		return nil, newErr(ErrProviderUnavailable, "classifier unavailable")
	}
	status := "classified"
	if d.Status == "extracted" && cl.DocumentType == d.DocumentType {
		status = "extracted"
	}
	if _, err := tx.Exec(ctx, `UPDATE documents SET document_type = $2, classification_confidence = $3, status = $4 WHERE id = $1`,
		d.ID, cl.DocumentType, cl.Confidence, status); err != nil {
		return nil, err
	}
	c.addEvent("DocumentClassified", map[string]any{"document_id": d.ID, "document_type": cl.DocumentType,
		"confidence": cl.Confidence, "model": cl.Model})
	if cl.DocumentType != "other" {
		if err := g.completeDocumentRequests(ctx, tx, c, d.ID, d.PartyID, cl.DocumentType); err != nil {
			return nil, err
		}
	}
	return map[string]any{"document_id": d.ID, "document_type": cl.DocumentType, "confidence": cl.Confidence,
		"declared_type": d.DeclaredType, "matches_declared": d.DeclaredType == "" || d.DeclaredType == cl.DocumentType}, nil
}

// document.extract
func (g *Gateway) documentExtract(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	d := c.app.document(str(c.input, "document_id"))
	if d == nil {
		return nil, newErr(ErrNotFound, "document not found")
	}
	if d.DocumentType == "" || d.DocumentType == "other" {
		return nil, newErr(ErrPrecondition, "classify the document before extracting")
	}
	for _, e := range c.app.Evidence {
		if e.DocumentID == d.ID {
			return nil, newErr(ErrStateConflict, "evidence was already extracted from this document")
		}
	}
	content, err := g.documentContent(ctx, d)
	if err != nil {
		return nil, err
	}
	fields, extractor, version, err := g.Connectors.Extract.Extract(ctx, content, d.DocumentType)
	if err != nil {
		return nil, newErr(ErrExtractionFailed, "extraction failed")
	}
	threshold := g.Pack.Evidence.MinConfidenceAutoVerify
	ids, evidence, low := []string{}, []map[string]any{}, []string{}
	for _, f := range fields {
		id := newUUID()
		val, _ := json.Marshal(f.Value)
		if _, err := tx.Exec(ctx, `INSERT INTO evidence (id, tenant_id, application_id, document_id, field, value, confidence, page,
			extractor, extractor_version) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			id, c.tenantID, c.app.ID, d.ID, f.Field, val, f.Confidence, f.Page, extractor, version); err != nil {
			return nil, err
		}
		ids = append(ids, id)
		evidence = append(evidence, map[string]any{"evidence_id": id, "field": f.Field, "value": f.Value, "confidence": f.Confidence})
		if f.Confidence < threshold {
			low = append(low, f.Field)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE documents SET status = 'extracted' WHERE id = $1`, d.ID); err != nil {
		return nil, err
	}
	// Values stay out of the event log; they are personal data.
	c.addEvent("DocumentExtracted", map[string]any{"document_id": d.ID, "document_type": d.DocumentType,
		"evidence_count": len(ids), "low_confidence_fields": low})
	return map[string]any{"document_id": d.ID, "evidence_ids": ids, "evidence": evidence, "low_confidence_fields": low,
		"auto_verify_min_confidence": threshold}, nil
}

// evidence.verify
func (g *Gateway) evidenceVerify(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	e := c.app.evidence(str(c.input, "evidence_id"))
	if e == nil {
		return nil, newErr(ErrNotFound, "evidence not found")
	}
	status := str(c.input, "status")
	corrected, hasCorrection := c.input["corrected_value"]
	if c.p.IsAgent() {
		if hasCorrection {
			return nil, newErr(ErrPolicyDenied, "agents cannot correct evidence; mark it needs_review instead")
		}
		if status == "verified" && e.Confidence < g.Pack.Evidence.MinConfidenceAutoVerify {
			return nil, &ToolError{Code: ErrPolicyDenied, Message: "confidence is below the auto-verify threshold; a person must verify this",
				Details: map[string]any{"confidence": e.Confidence, "min_confidence": g.Pack.Evidence.MinConfidenceAutoVerify}}
		}
	}
	if hasCorrection && status != "verified" {
		return nil, newErr(ErrValidation, "a corrected value can only be verified")
	}
	if hasCorrection {
		val, _ := json.Marshal(corrected)
		if _, err := tx.Exec(ctx, `UPDATE evidence SET original_value = COALESCE(original_value, value), value = $2 WHERE id = $1`,
			e.ID, val); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE evidence SET status = $2::evidence_status, verified_by = $3, verified_at = now(),
		note = NULLIF($4,'') WHERE id = $1`, e.ID, status, c.p.ID, str(c.input, "note")); err != nil {
		return nil, err
	}
	c.addEvent("EvidenceVerified", map[string]any{"evidence_id": e.ID, "document_id": e.DocumentID, "field": e.Field,
		"status": status, "corrected": hasCorrection})
	return map[string]any{"evidence_id": e.ID, "status": status}, nil
}

// document.request asks a party for a document: a task for the borrower, plus a notification.
func (g *Gateway) documentRequest(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	p, err := c.inputParty()
	if err != nil {
		return nil, err
	}
	docType := str(c.input, "document_type")
	// One open request per document and party: asking again returns the open one.
	var existing string
	err = tx.QueryRow(ctx, `SELECT id::text FROM tasks WHERE application_id = $1 AND kind = 'document_request'
		AND status IN ('open', 'in_progress') AND details->>'document_type' = $2 AND details->>'party_id' = $3 LIMIT 1`,
		c.app.ID, docType, p.PartyID).Scan(&existing)
	if err == nil {
		return map[string]any{"task_id": existing, "notified_via": "already_requested"}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	assignee := c.app.Borrower
	if assignee == "" {
		assignee = "role:loan_officer" // no portal user yet: staff collect it
	}
	details, _ := json.Marshal(map[string]any{"party_id": p.PartyID, "document_type": docType, "message": str(c.input, "message")})
	taskID := newUUID()
	if _, err := tx.Exec(ctx, `INSERT INTO tasks (id, tenant_id, application_id, kind, title, details, assignee, created_by, due_at, agent_run_id)
		VALUES ($1,$2,$3,'document_request',$4,$5,$6,$7,$8,$9)`, taskID, c.tenantID, c.app.ID,
		fmt.Sprintf("Provide %s for %s", strings.ReplaceAll(docType, "_", " "), p.LegalName), details, assignee, c.p.ID,
		g.Now().Add(g.Pack.Intake.DueAfter()), runRef(c)); err != nil {
		return nil, err
	}
	via := "task"
	if c.app.Borrower != "" {
		if _, err := g.Connectors.Notify.Send(ctx, "portal", c.app.Borrower, "document_requested", map[string]any{"document_type": docType}); err == nil {
			via = "portal"
		}
	}
	c.addEvent("DocumentRequested", map[string]any{"task_id": taskID, "party_id": p.PartyID, "document_type": docType})
	return map[string]any{"task_id": taskID, "notified_via": via}, nil
}
