package api_test

import (
	"context"
	"testing"
	"time"
)

// TestTasksAndAgentRuns: agent runs link every call an agent makes, agents escalate with
// tasks, people work the queue, and document requests close when the document arrives.
func TestTasksAndAgentRuns(t *testing.T) {
	e := setup(t)
	why := func(r string) map[string]any { return map[string]any{"reason": r} }
	inRun := func(run, r string) map[string]any { return map[string]any{"reason": r, "run_id": run} }

	r := e.call(officer, "application.create", map[string]any{"product_code": "SME-WC", "requested_amount_minor": 1500000,
		"currency": "USD", "requested_term_months": 12, "borrower": "borrower:mekong"}, nil)
	expect(t, r, "ok", "")
	appID := r.Result["application_id"].(string)
	r = e.call(officer, "party.add", map[string]any{"application_id": appID, "kind": "business", "role": "applicant",
		"legal_name": "Mekong Trading Co.", "registration_number": "0105561234567", "country": "TH"}, nil)
	applicant := r.Result["party_id"].(string)
	expect(t, e.call(officer, "party.add", map[string]any{"application_id": appID, "kind": "individual", "role": "director",
		"legal_name": "Somchai P.", "date_of_birth": "1980-04-02"}, nil), "ok", "")
	expect(t, e.call(officer, "application.submit", map[string]any{"application_id": appID}, nil), "ok", "")
	version := int(e.state(officer, appID)["state_version"].(float64))

	// --- Runs. ---
	start := map[string]any{"application_id": appID, "agent_version": "0.2.0", "trigger_event_type": "ApplicationStateChanged"}
	r = e.call(agent, "agent.run_start", start, why("intake: application submitted"))
	expect(t, r, "ok", "")
	run := r.Result["run_id"].(string)
	r = e.call(agent, "agent.run_start", start, why("intake: second run on the same application"))
	expect(t, r, "error", "STATE_CONFLICT")
	if r.Error.Details.(map[string]any)["run_id"] != run {
		t.Errorf("conflict should name the open run: %+v", r.Error.Details)
	}
	expect(t, e.call(officer, "agent.run_start", start, nil), "error", "POLICY_DENIED")
	// Another agent cannot borrow the run.
	r = e.call(verifier, "application.get", map[string]any{"application_id": appID}, inRun(run, "verification: read"))
	expect(t, r, "error", "VALIDATION_FAILED")
	// Run events do not change the application's version.
	if v := int(e.state(officer, appID)["state_version"].(float64)); v != version {
		t.Errorf("state_version moved from %d to %d", version, v)
	}

	// --- Document requests: one open request per document and party. ---
	req := map[string]any{"application_id": appID, "party_id": applicant, "document_type": "bank_statements"}
	r = e.call(agent, "document.request", req, inRun(run, "intake: bank statements are missing"))
	expect(t, r, "ok", "")
	docTask := r.Result["task_id"].(string)
	if r.Result["notified_via"] != "portal" {
		t.Errorf("borrower should be notified in the portal: %+v", r.Result)
	}
	r = e.call(agent, "document.request", req, inRun(run, "intake: asking again"))
	expect(t, r, "ok", "")
	if r.Result["task_id"] != docTask || r.Result["notified_via"] != "already_requested" {
		t.Errorf("second request should return the open one: %+v", r.Result)
	}

	// --- Escalation. ---
	esc := map[string]any{"application_id": appID, "kind": "resolve_exception", "title": "Check the applicant's address",
		"assignee": "role:astronaut"}
	expect(t, e.call(agent, "task.create", esc, inRun(run, "intake: escalate")), "error", "VALIDATION_FAILED")
	esc["assignee"] = "role:loan_officer"
	r = e.call(agent, "task.create", esc, inRun(run, "intake: escalate"))
	expect(t, r, "ok", "")
	escTask := r.Result["task_id"].(string)

	r = e.call(agent, "agent.run_finish", map[string]any{"run_id": run, "status": "escalated", "summary": "requested 1 document; 1 task",
		"escalation_task_id": escTask, "tokens_in": 0}, why("intake: done"))
	expect(t, r, "ok", "")
	if r.Result["steps"] != float64(4) { // two requests, a refused task and the escalation
		t.Errorf("steps: %+v", r.Result)
	}
	expect(t, e.call(agent, "agent.run_finish", map[string]any{"run_id": run, "status": "succeeded"}, why("again")), "error", "STATE_CONFLICT")
	expect(t, e.call(agent, "task.create", esc, inRun(run, "late")), "error", "VALIDATION_FAILED")
	expect(t, e.call(agent, "application.get", map[string]any{"application_id": appID}, inRun(run, "re-read")), "ok", "")

	r = e.call(officer, "agent.run_list", map[string]any{"run_id": run}, nil)
	expect(t, r, "ok", "")
	got := r.Result["runs"].([]any)[0].(map[string]any)
	if got["status"] != "escalated" || got["escalation_task_id"] != escTask || got["agent"] != "agent:intake" {
		t.Errorf("run: %+v", got)
	}
	calls := got["tool_calls"].([]any)
	if len(calls) != 4 || calls[0].(map[string]any)["tool"] != "document.request" || calls[2].(map[string]any)["outcome"] != "error" {
		t.Errorf("tool calls: %+v", calls)
	}
	r = e.call(officer, "agent.run_list", map[string]any{"application_id": appID}, nil)
	if n := len(r.Result["runs"].([]any)); n != 1 {
		t.Errorf("runs on the application: %d", n)
	}

	// --- The queue. ---
	ids := func(r resp) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, v := range r.Result["tasks"].([]any) {
			m := v.(map[string]any)
			out[m["task_id"].(string)] = m
		}
		return out
	}
	mine := ids(e.call(officer, "task.list", map[string]any{}, nil))
	if _, ok := mine[escTask]; !ok || !mine[escTask]["you_can_complete"].(bool) {
		t.Errorf("escalation should be in the loan officers' queue: %+v", mine)
	}
	if _, ok := mine[docTask]; ok {
		t.Error("the borrower's document request is not the officer's task")
	}
	all := ids(e.call(officer, "task.list", map[string]any{"application_id": appID, "assignee": "any"}, nil))
	if len(all) != 2 || all[escTask]["agent_run_id"] != run || all[docTask]["assignee"] != "borrower:mekong" {
		t.Errorf("all tasks on the application: %+v", all)
	}
	expect(t, e.call(agent, "task.list", map[string]any{"assignee": "any"}, why("intake: list")), "error", "POLICY_DENIED")

	expect(t, e.call(approver, "task.complete", map[string]any{"task_id": escTask, "status": "done"}, nil), "error", "POLICY_DENIED")
	r = e.call(officer, "task.complete", map[string]any{"task_id": escTask, "status": "done", "outcome": map[string]any{"note": "address confirmed by phone"}}, nil)
	expect(t, r, "ok", "")
	expect(t, e.call(officer, "task.complete", map[string]any{"task_id": escTask, "status": "done"}, nil), "error", "STATE_CONFLICT")

	// --- The borrower uploads the requested document; classification closes the request. ---
	r = e.uploadDoc(borrower, appID, applicant, "statements.pdf", "bank_statements",
		mockDoc("bank_statements", "account_holder: Mekong Trading Co.", "months_covered: 12"))
	expect(t, r, "ok", "")
	r = e.call(docAgent, "document.classify", map[string]any{"document_id": r.Result["document_id"]}, why("documents: classify"))
	expect(t, r, "ok", "")
	done := ids(e.call(officer, "task.list", map[string]any{"application_id": appID, "assignee": "any", "status": "done"}, nil))
	if done[docTask] == nil || done[docTask]["completed_by"] != "agent:documents" || done[escTask]["outcome"].(map[string]any)["note"] == nil {
		t.Errorf("done tasks: %+v", done)
	}

	// --- A run left open (the agent crashed) is closed when the next one starts. ---
	r = e.call(agent, "agent.run_start", start, why("intake: new event"))
	expect(t, r, "ok", "")
	stale := r.Result["run_id"].(string)
	err := e.withTenant(context.Background(), func(q querier) error {
		var id string
		return q.QueryRow(context.Background(), `UPDATE agent_runs SET started_at = now() - interval '11 minutes'
			WHERE id = $1 RETURNING id::text`, stale).Scan(&id)
	})
	if err != nil {
		t.Fatal(err)
	}
	r = e.call(agent, "agent.run_start", start, why("intake: after a restart"))
	expect(t, r, "ok", "")
	r = e.call(officer, "agent.run_list", map[string]any{"run_id": stale}, nil)
	if got := r.Result["runs"].([]any)[0].(map[string]any); got["status"] != "failed" {
		t.Errorf("stale run: %+v", got)
	}
}

// TestTimers: the scheduler turns time into events. The intake grace period ends with
// IntakeReviewDue; an overdue request is reported once per due date, task.remind moves
// the due date on, and the reminder limit hands the task to a person.
func TestTimers(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	start := time.Now()
	at := func(d time.Duration) { e.g.Now = func() time.Time { return start.Add(d) } }
	defer func() { e.g.Now = time.Now }()
	tick := func(want int) {
		t.Helper()
		n, err := e.g.Tick(ctx)
		if err != nil || n != want {
			t.Fatalf("tick: %d events, %v (want %d)", n, err, want)
		}
	}
	events := func(appID, typ string) int {
		var n int
		if err := e.withTenant(ctx, func(q querier) error {
			return q.QueryRow(ctx, `SELECT count(*) FROM events WHERE aggregate_id = $1 AND type = $2`, appID, typ).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}

	r := e.call(officer, "application.create", map[string]any{"product_code": "SME-WC", "requested_amount_minor": 1500000,
		"currency": "USD", "requested_term_months": 12, "borrower": "borrower:mekong"}, nil)
	appID := r.Result["application_id"].(string)
	applicant := e.call(officer, "party.add", map[string]any{"application_id": appID, "kind": "business", "role": "applicant",
		"legal_name": "Mekong Trading Co.", "registration_number": "0105561234567", "country": "TH"}, nil).Result["party_id"].(string)
	e.call(officer, "party.add", map[string]any{"application_id": appID, "kind": "individual", "role": "director",
		"legal_name": "Somchai P.", "date_of_birth": "1980-04-02"}, nil)
	expect(t, e.call(officer, "application.submit", map[string]any{"application_id": appID}, nil), "ok", "")

	// --- Intake grace period (pack: 10 minutes). ---
	at(time.Minute)
	tick(0)
	at(11 * time.Minute)
	tick(1)
	tick(0)
	if events(appID, "IntakeReviewDue") != 1 {
		t.Fatal("IntakeReviewDue not emitted once")
	}

	// --- A document request falls due (pack: 7 days), is reminded twice, then escalates. ---
	why := map[string]any{"reason": "intake: bank statements missing"}
	r = e.call(agent, "document.request", map[string]any{"application_id": appID, "party_id": applicant,
		"document_type": "bank_statements"}, why)
	expect(t, r, "ok", "")
	taskID := r.Result["task_id"].(string)
	at(6 * 24 * time.Hour)
	tick(0)
	at(8 * 24 * time.Hour)
	tick(1)
	tick(0) // reported once per due date
	r = e.call(agent, "task.remind", map[string]any{"task_id": taskID, "message": "A reminder to upload your bank statements."}, why)
	expect(t, r, "ok", "")
	if r.Result["notified_via"] != "portal" || r.Result["reminders"] != float64(1) || r.Result["reminders_left"] != float64(1) {
		t.Fatalf("first reminder: %+v", r.Result)
	}
	tick(0) // the due date moved on 3 days
	at(12 * 24 * time.Hour)
	tick(1)
	expect(t, e.call(agent, "task.remind", map[string]any{"task_id": taskID}, why), "ok", "")
	expect(t, e.call(agent, "task.remind", map[string]any{"task_id": taskID}, why), "error", "STATE_CONFLICT")
	if events(appID, "TaskOverdue") != 2 || events(appID, "TaskReminded") != 2 {
		t.Errorf("events: overdue %d, reminded %d", events(appID, "TaskOverdue"), events(appID, "TaskReminded"))
	}
	// A borrower cannot send reminders, and staff tasks are not reminded this way.
	expect(t, e.call(borrower, "task.remind", map[string]any{"task_id": taskID}, nil), "error", "POLICY_DENIED")
	r = e.call(officer, "task.create", map[string]any{"application_id": appID, "kind": "review_evidence", "title": "Look",
		"assignee": "role:loan_officer"}, nil)
	expect(t, e.call(officer, "task.remind", map[string]any{"task_id": r.Result["task_id"]}, nil), "error", "PRECONDITION_FAILED")

	// --- A run left open by a crashed agent is closed by the scheduler. ---
	r = e.call(agent, "agent.run_start", map[string]any{"application_id": appID, "agent_version": "t"}, why)
	expect(t, r, "ok", "")
	stale := r.Result["run_id"].(string)
	tick(0)
	at(12*24*time.Hour + 11*time.Minute)
	tick(1)
	r = e.call(officer, "agent.run_list", map[string]any{"run_id": stale}, nil)
	if got := r.Result["runs"].([]any)[0].(map[string]any); got["status"] != "failed" {
		t.Errorf("abandoned run: %+v", got)
	}
	expect(t, e.call(agent, "agent.run_finish", map[string]any{"run_id": stale, "status": "succeeded"}, why), "error", "STATE_CONFLICT")
}
