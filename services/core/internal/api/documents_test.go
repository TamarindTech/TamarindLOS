package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const docAgent = "dev-agent-documents"

// TestDocumentPipeline: upload links, file checks, classification, extraction and
// evidence verification with the agent confidence guardrail.
func TestDocumentPipeline(t *testing.T) {
	e := setup(t)
	da := map[string]any{"reason": "processing an uploaded document"}

	r := e.call(officer, "application.create", map[string]any{"product_code": "SME-WC", "requested_amount_minor": 1500000,
		"currency": "USD", "requested_term_months": 12, "borrower": "borrower:mekong"}, nil)
	expect(t, r, "ok", "")
	appID := r.Result["application_id"].(string)
	r = e.call(officer, "party.add", map[string]any{"application_id": appID, "kind": "business", "role": "applicant",
		"legal_name": "Mekong Trading Co.", "registration_number": "0105561234567", "country": "TH"}, nil)
	applicant := r.Result["party_id"].(string)
	r = e.call(officer, "party.add", map[string]any{"application_id": appID, "kind": "individual", "role": "director",
		"legal_name": "Somchai P.", "date_of_birth": "1980-04-02"}, nil)
	director := r.Result["party_id"].(string)
	expect(t, e.call(officer, "application.submit", map[string]any{"application_id": appID}, nil), "ok", "")
	app := e.state(officer, appID)
	if n := len(app["missing_documents"].([]any)); n != 4 { // 1.5M: no financial statements needed
		t.Fatalf("missing documents: %d %+v", n, app["missing_documents"])
	}

	// --- Upload checks. ---
	link := func(size int) map[string]any {
		in := map[string]any{"application_id": appID, "party_id": applicant, "file_name": "reg.pdf", "mime_type": "application/pdf"}
		if size > 0 {
			in["size_bytes"] = size
		}
		r := e.call(borrower, "document.upload_url", in, nil)
		expect(t, r, "ok", "")
		return r.Result
	}
	if status, _ := e.put("/v1/uploads/up_00000000000000000000000000000000_"+strings.Repeat("ab", 32), []byte("%PDF-x")); status != http.StatusNotFound {
		t.Fatalf("unknown link: %d", status)
	}
	l := link(0)
	if status, body := e.put(l["upload_url"].(string), []byte("GIF89a not a pdf")); status != http.StatusUnprocessableEntity {
		t.Fatalf("wrong file type: %d %+v", status, body)
	}
	expect(t, e.call(borrower, "document.attach", map[string]any{"application_id": appID, "upload_token": l["upload_token"]}, nil),
		"error", "UNSUPPORTED_FILE")
	l = link(0)
	eicar := []byte(`%PDF-1.4 X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`)
	e.put(l["upload_url"].(string), eicar)
	expect(t, e.call(borrower, "document.attach", map[string]any{"application_id": appID, "upload_token": l["upload_token"]}, nil),
		"error", "MALWARE_DETECTED")
	l = link(64)
	if status, _ := e.put(l["upload_url"].(string), mockDoc("business_registration", strings.Repeat("filler: x", 20))); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload: %d", status)
	}

	// --- A good upload by the borrower; links are single use. ---
	l = link(0)
	content := mockDoc("business_registration", "registration_number: 0105561234567?", "legal_name: Mekong Trading Co.")
	if status, body := e.put(l["upload_url"].(string), content); status != http.StatusCreated {
		t.Fatalf("upload: %d %+v", status, body)
	}
	if status, _ := e.put(l["upload_url"].(string), content); status != http.StatusConflict {
		t.Fatalf("second upload on the same link: %d", status)
	}
	r = e.call(borrower, "document.attach", map[string]any{"application_id": appID, "upload_token": l["upload_token"],
		"declared_type": "business_registration"}, nil)
	expect(t, r, "ok", "")
	docID := r.Result["document_id"].(string)
	expect(t, e.call(borrower, "document.attach", map[string]any{"application_id": appID, "upload_token": l["upload_token"]}, nil),
		"error", "UPLOAD_NOT_FOUND")

	// --- Classification and extraction (document agent, L3). ---
	expect(t, e.call(borrower, "document.classify", map[string]any{"document_id": docID}, nil), "error", "POLICY_DENIED")
	expect(t, e.call(foreign, "document.classify", map[string]any{"document_id": docID}, nil), "error", "NOT_FOUND")
	expect(t, e.call(docAgent, "document.extract", map[string]any{"document_id": docID}, da), "error", "PRECONDITION_FAILED")
	r = e.call(docAgent, "document.classify", map[string]any{"document_id": docID}, da)
	expect(t, r, "ok", "")
	if r.Result["document_type"] != "business_registration" || r.Result["confidence"].(float64) < 0.9 {
		t.Fatalf("classify: %+v", r.Result)
	}
	r = e.call(docAgent, "document.extract", map[string]any{"document_id": docID}, da)
	expect(t, r, "ok", "")
	low := r.Result["low_confidence_fields"].([]any)
	if len(low) != 1 || low[0] != "registration_number" {
		t.Fatalf("low confidence fields: %+v", r.Result)
	}
	ev := map[string]string{}
	for _, x := range r.Result["evidence"].([]any) {
		m := x.(map[string]any)
		ev[m["field"].(string)] = m["evidence_id"].(string)
	}
	expect(t, e.call(docAgent, "document.extract", map[string]any{"document_id": docID}, da), "error", "STATE_CONFLICT")

	// --- Evidence: the agent may verify only confident values and never correct them. ---
	expect(t, e.call(docAgent, "evidence.verify", map[string]any{"evidence_id": ev["registration_number"], "status": "verified"}, da),
		"error", "POLICY_DENIED")
	expect(t, e.call(docAgent, "evidence.verify", map[string]any{"evidence_id": ev["legal_name"], "status": "verified",
		"corrected_value": "x"}, da), "error", "POLICY_DENIED")
	expect(t, e.call(docAgent, "evidence.verify", map[string]any{"evidence_id": ev["legal_name"], "status": "verified"}, da), "ok", "")
	expect(t, e.call(docAgent, "evidence.verify", map[string]any{"evidence_id": ev["registration_number"], "status": "needs_review",
		"note": "low extraction confidence"}, da), "ok", "")
	expect(t, e.call(officer, "evidence.verify", map[string]any{"evidence_id": ev["registration_number"], "status": "verified",
		"corrected_value": "0105561234567", "note": "checked against the registry extract"}, nil), "ok", "")

	app = e.state(officer, appID)
	if n := len(app["missing_documents"].([]any)); n != 3 {
		t.Fatalf("after classification, missing documents: %d", n)
	}
	for _, x := range app["evidence"].([]any) {
		m := x.(map[string]any)
		if m["status"] != "verified" {
			t.Errorf("evidence %s: %v", m["field"], m["status"])
		}
	}

	// --- Asking for a missing document creates a task for the borrower. ---
	r = e.call(officer, "document.request", map[string]any{"application_id": appID, "party_id": director,
		"document_type": "identity_document", "message": "Please upload a passport or national ID card."}, nil)
	expect(t, r, "ok", "")
	if r.Result["notified_via"] != "portal" {
		t.Errorf("document.request: %+v", r.Result)
	}

	// --- Upload links never reach the invocation log. ---
	var leaked int
	ctx := context.Background()
	if err := e.withTenant(ctx, func(q querier) error {
		return q.QueryRow(ctx, `SELECT count(*) FROM tool_invocations WHERE tool = 'document.upload_url' AND response::text LIKE '%up\_%'`).Scan(&leaked)
	}); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Errorf("%d upload links stored in the invocation log", leaked)
	}
}
