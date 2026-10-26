package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Work queue (tasks) and agent runs (Reference Architecture §9.2).
//
// Agents escalate by creating a task for a role. Each agent run is recorded with
// what woke it, every tool call it made (tool_invocations.agent_run_id), its outcome
// and, when it escalated, the task it handed to a person.

// staleRunAfter is how long a run may stay open before the next run of the same agent
// on the same application closes it as failed (the agent most likely crashed).
const staleRunAfter = 10 * time.Minute

// noSnapshot lists mutating tools that do not change the application itself, so the
// gateway does not recompute its snapshot or run automatic transitions after them.
var noSnapshot = map[string]bool{
	"approval.respond": true, "task.create": true, "task.complete": true, "task.remind": true,
	"agent.run_start": true, "agent.run_finish": true,
	"webhook.subscribe": true, "webhook.remove": true,
}

// checkRun verifies that the envelope's run_id is an open run of the calling agent.
// A read inside a run that has already closed (an agent re-reading after a restart)
// is allowed but not linked to the run; a change is refused.
func (g *Gateway) checkRun(ctx context.Context, tx pgx.Tx, c *call) error {
	if c.env.RunID == "" {
		return nil
	}
	var agent, status string
	err := tx.QueryRow(ctx, `SELECT agent, status FROM agent_runs WHERE id = $1`, c.env.RunID).Scan(&agent, &status)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && agent != c.p.ID) {
		return newErr(ErrValidation, "envelope.run_id %s is not a run of %s", c.env.RunID, c.p.ID)
	}
	if err != nil {
		return err
	}
	if status != "running" {
		if !c.tool.Mutates {
			return nil
		}
		return newErr(ErrValidation, "envelope.run_id %s is already %s", c.env.RunID, status)
	}
	c.runOK = true
	return nil
}

// --- Tasks -------------------------------------------------------------------------

// assigneeValid accepts role:<name> for a role some principal in the tenant holds, or a
// principal id in the tenant.
func (g *Gateway) assigneeValid(c *call, assignee string) bool {
	if role, ok := strings.CutPrefix(assignee, "role:"); ok {
		for _, p := range g.Registry.Principals {
			if p.Tenant == c.p.Tenant && hasRole(p.Roles, role) {
				return true
			}
		}
		return false
	}
	p, ok := g.Registry.ByID(assignee)
	return ok && p.Tenant == c.p.Tenant
}

// assignedTo reports whether a task assignee is the principal or one of its roles.
func assignedTo(assignee, id string, roles []string) bool {
	if assignee == id {
		return true
	}
	role, ok := strings.CutPrefix(assignee, "role:")
	return ok && hasRole(roles, role)
}

// myAssignees lists the assignee values that mean "mine" for a principal.
func myAssignees(id string, roles []string) []string {
	out := []string{id}
	for _, r := range roles {
		out = append(out, "role:"+r)
		if r == "senior_credit_approver" {
			out = append(out, "role:credit_approver")
		}
	}
	return out
}

func (g *Gateway) taskCreate(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	assignee := str(c.input, "assignee")
	if !g.assigneeValid(c, assignee) {
		return nil, newErr(ErrValidation, "assignee %q is not a principal or role in this tenant", assignee)
	}
	var due *time.Time
	if s := str(c.input, "due_at"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, newErr(ErrValidation, "due_at must be an RFC 3339 timestamp")
		}
		due = &t
	}
	details := c.input["details"]
	if details == nil {
		details = map[string]any{}
	}
	detailsJSON, _ := json.Marshal(details)
	var appID *string
	if c.app != nil {
		appID = &c.app.ID
	}
	id := newUUID()
	if _, err := tx.Exec(ctx, `INSERT INTO tasks (id, tenant_id, application_id, kind, title, details, assignee, created_by, due_at, agent_run_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,'')::uuid)`,
		id, c.tenantID, appID, str(c.input, "kind"), str(c.input, "title"), detailsJSON, assignee, c.p.ID, due, c.env.RunID); err != nil {
		return nil, err
	}
	if err := g.taskEvent(ctx, tx, c, "TaskCreated", id, map[string]any{"task_id": id, "kind": str(c.input, "kind"),
		"assignee": assignee, "title": str(c.input, "title")}); err != nil {
		return nil, err
	}
	return map[string]any{"task_id": id, "assignee": assignee}, nil
}

// taskEvent records a task event on its application, or on the task when it has none.
func (g *Gateway) taskEvent(ctx context.Context, tx pgx.Tx, c *call, typ, taskID string, data map[string]any) error {
	if c.app != nil {
		c.addEvent(typ, data)
		return nil
	}
	v, err := nextVersion(ctx, tx, "task", taskID)
	if err != nil {
		return err
	}
	c.events = append(c.events, Event{Type: typ, Data: data, AggregateType: "task", AggregateID: taskID, AggregateVersion: v})
	return nil
}

// nextVersion is the next event version of an aggregate other than an application.
func nextVersion(ctx context.Context, tx pgx.Tx, aggregateType, id string) (int, error) {
	var v int
	err := tx.QueryRow(ctx, `SELECT COALESCE(max(aggregate_version), 0) + 1 FROM events
		WHERE aggregate_type = $1 AND aggregate_id = $2`, aggregateType, id).Scan(&v)
	return v, err
}

func (g *Gateway) taskComplete(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	id := str(c.input, "task_id")
	var assignee, createdBy, status string
	err := tx.QueryRow(ctx, `SELECT assignee, created_by, status::text FROM tasks WHERE id = $1 FOR UPDATE`, id).
		Scan(&assignee, &createdBy, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, newErr(ErrNotFound, "task %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if !assignedTo(assignee, c.p.ID, c.p.Roles) && createdBy != c.p.ID {
		return nil, newErr(ErrPolicyDenied, "only the assignee (%s) or the creator may complete this task", assignee)
	}
	if status != "open" && status != "in_progress" {
		return nil, &ToolError{Code: ErrStateConflict, Message: "task is already " + status, Details: map[string]any{"status": status}}
	}
	to := str(c.input, "status")
	outcome, _ := json.Marshal(c.input["outcome"])
	if _, err := tx.Exec(ctx, `UPDATE tasks SET status = $2::task_status, completed_by = $3, completed_at = now(),
		outcome = NULLIF($4::text, 'null')::jsonb WHERE id = $1`, id, to, c.p.ID, string(outcome)); err != nil {
		return nil, err
	}
	if err := g.taskEvent(ctx, tx, c, "TaskCompleted", id, map[string]any{"task_id": id, "status": to}); err != nil {
		return nil, err
	}
	return map[string]any{"task_id": id, "status": to}, nil
}

// completeDocumentRequests closes the open request for a document type once a document
// of that type has been classified for the party (a document that names no party counts
// for the applicant, as it does for completeness). A collect_document task the intake
// agent raised for that request closes with it.
func (g *Gateway) completeDocumentRequests(ctx context.Context, tx pgx.Tx, c *call, documentID, partyID, docType string) error {
	if partyID == "" {
		for _, p := range c.app.Parties {
			if p.Role == "applicant" {
				partyID = p.PartyID
			}
		}
	}
	ids, err := collectIDs(tx.Query(ctx, `UPDATE tasks SET status = 'done', completed_by = $1, completed_at = now(),
		outcome = jsonb_build_object('document_id', $2::text)
		WHERE application_id = $3 AND kind = 'document_request' AND status IN ('open', 'in_progress')
		  AND details->>'document_type' = $4 AND details->>'party_id' = $5
		RETURNING id::text`, c.p.ID, documentID, c.app.ID, docType, partyID))
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		followUps, err := collectIDs(tx.Query(ctx, `UPDATE tasks SET status = 'done', completed_by = $1, completed_at = now(),
			outcome = jsonb_build_object('document_id', $2::text)
			WHERE application_id = $3 AND kind = 'collect_document' AND status IN ('open', 'in_progress')
			  AND details->>'document_request_task_id' = ANY($4)
			RETURNING id::text`, c.p.ID, documentID, c.app.ID, ids))
		if err != nil {
			return err
		}
		ids = append(ids, followUps...)
	}
	for _, id := range ids {
		c.addEvent("TaskCompleted", map[string]any{"task_id": id, "status": "done", "document_id": documentID})
	}
	return nil
}

// collectIDs reads a single text column from a query's rows.
func collectIDs(rows pgx.Rows, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (g *Gateway) taskList(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	limit, ok := num(c.input, "limit")
	if !ok {
		limit = 50
	}
	var appID *string
	if c.app != nil {
		appID = &c.app.ID
	}
	assignee := str(c.input, "assignee")
	var assignees []string
	switch assignee {
	case "", "mine":
		assignees = myAssignees(c.p.ID, c.p.Roles)
	case "any":
		if appID == nil && c.p.Kind() != "user" {
			return nil, newErr(ErrPolicyDenied, "assignee any requires application_id")
		}
	default:
		if appID == nil && c.p.Kind() != "user" && !assignedTo(assignee, c.p.ID, c.p.Roles) {
			return nil, newErr(ErrPolicyDenied, "listing another assignee's tasks requires application_id")
		}
		assignees = []string{assignee}
	}
	statuses := []string{"open", "in_progress"}
	switch s := str(c.input, "status"); s {
	case "", "active":
	case "all":
		statuses = []string{"open", "in_progress", "done", "cancelled"}
	default:
		statuses = []string{s}
	}
	rows, err := tx.Query(ctx, `SELECT t.id::text, t.kind, t.title, t.details, t.assignee, t.status::text,
		COALESCE(t.application_id::text, ''), COALESCE(a.reference, ''), t.due_at, t.created_by, t.created_at,
		COALESCE(t.completed_by, ''), t.completed_at, t.outcome, COALESCE(t.agent_run_id::text, '')
		FROM tasks t LEFT JOIN applications a ON a.id = t.application_id
		WHERE ($1::uuid IS NULL OR t.application_id = $1)
		  AND ($2::text[] IS NULL OR t.assignee = ANY($2))
		  AND t.status::text = ANY($3)
		  AND (NULLIF($4, '') IS NULL OR t.kind = $4)
		ORDER BY t.due_at NULLS LAST, t.created_at LIMIT $5`, appID, assignees, statuses, str(c.input, "kind"), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, kind, title, assignee, status, app, ref, createdBy, completedBy, runID string
		var details, outcome []byte
		var due, completed *time.Time
		var created time.Time
		if err := rows.Scan(&id, &kind, &title, &details, &assignee, &status, &app, &ref, &due, &createdBy, &created,
			&completedBy, &completed, &outcome, &runID); err != nil {
			return nil, err
		}
		t := map[string]any{"task_id": id, "kind": kind, "title": title, "details": jsonMap(details), "assignee": assignee,
			"status": status, "created_by": createdBy, "created_at": created.UTC().Format(time.RFC3339),
			"you_can_complete": (status == "open" || status == "in_progress") && (assignedTo(assignee, c.p.ID, c.p.Roles) || createdBy == c.p.ID)}
		if app != "" {
			t["application_id"], t["application_reference"] = app, ref
		}
		if due != nil {
			t["due_at"] = due.UTC().Format(time.RFC3339)
		}
		if completedBy != "" {
			t["completed_by"] = completedBy
		}
		if completed != nil {
			t["completed_at"] = completed.UTC().Format(time.RFC3339)
		}
		if len(outcome) > 0 {
			t["outcome"] = jsonMap(outcome)
		}
		if runID != "" {
			t["agent_run_id"] = runID
		}
		out = append(out, t)
	}
	return map[string]any{"tasks": out}, rows.Err()
}

// --- Agent runs ----------------------------------------------------------------------

func (g *Gateway) agentRunStart(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	if !c.p.IsAgent() {
		return nil, newErr(ErrPolicyDenied, "only agents open runs")
	}
	var appID *string
	if c.app != nil {
		appID = &c.app.ID
	}
	var existing string
	var stale bool
	err := tx.QueryRow(ctx, `SELECT id::text, started_at < $3 FROM agent_runs
		WHERE agent = $1 AND application_id IS NOT DISTINCT FROM $2 AND status = 'running' FOR UPDATE`,
		c.p.ID, appID, g.Now().UTC().Add(-staleRunAfter)).Scan(&existing, &stale)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, err
	case stale:
		if _, err := tx.Exec(ctx, `UPDATE agent_runs SET status = 'failed', finished_at = now(),
			summary = 'abandoned: not finished within `+staleRunAfter.String()+`' WHERE id = $1`, existing); err != nil {
			return nil, err
		}
		g.runEvent(c, "AgentRunFinished", existing, 2, map[string]any{"run_id": existing, "agent": c.p.ID, "status": "failed",
			"summary": "abandoned"})
	default:
		return nil, &ToolError{Code: ErrStateConflict, Message: "this agent already has an open run on the application",
			Details: map[string]any{"run_id": existing}}
	}
	var trigger *string
	if s := str(c.input, "trigger_event_id"); s != "" {
		trigger = &s
	}
	id := newUUID()
	if _, err := tx.Exec(ctx, `INSERT INTO agent_runs (id, tenant_id, agent, agent_version, application_id, trigger_event_id, trigger_event_type, started_at)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8)`, id, c.tenantID, c.p.ID, str(c.input, "agent_version"), appID, trigger,
		str(c.input, "trigger_event_type"), g.Now().UTC()); err != nil {
		return nil, err
	}
	data := map[string]any{"run_id": id, "agent": c.p.ID, "trigger_event_type": str(c.input, "trigger_event_type")}
	if appID != nil {
		data["application_id"] = *appID
	}
	g.runEvent(c, "AgentRunStarted", id, 1, data)
	return map[string]any{"run_id": id}, nil
}

// runEvent records a run event on the run itself, so it does not change the
// application's state_version.
func (g *Gateway) runEvent(c *call, typ, runID string, version int, data map[string]any) {
	c.events = append(c.events, Event{Type: typ, Data: data, AggregateType: "agent_run", AggregateID: runID, AggregateVersion: version})
}

func (g *Gateway) agentRunFinish(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	id := str(c.input, "run_id")
	var agent, status string
	var appID *string
	err := tx.QueryRow(ctx, `SELECT agent, status, application_id::text FROM agent_runs WHERE id = $1 FOR UPDATE`, id).
		Scan(&agent, &status, &appID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, newErr(ErrNotFound, "run %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if agent != c.p.ID {
		return nil, newErr(ErrPolicyDenied, "run %s belongs to %s", id, agent)
	}
	if status != "running" {
		return nil, &ToolError{Code: ErrStateConflict, Message: "run is already " + status, Details: map[string]any{"status": status}}
	}
	var task *string
	if t := str(c.input, "escalation_task_id"); t != "" {
		var found bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE id = $1)`, t).Scan(&found); err != nil {
			return nil, err
		}
		if !found {
			return nil, newErr(ErrNotFound, "escalation task %s not found", t)
		}
		task = &t
	}
	var steps int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tool_invocations WHERE agent_run_id = $1
		AND tool NOT IN ('agent.run_start', 'agent.run_finish')`, id).Scan(&steps); err != nil {
		return nil, err
	}
	tokensIn, _ := num(c.input, "tokens_in")
	tokensOut, _ := num(c.input, "tokens_out")
	cost, _ := num(c.input, "cost_micro_usd")
	to := str(c.input, "status")
	if _, err := tx.Exec(ctx, `UPDATE agent_runs SET status = $2, summary = NULLIF($3, ''), escalation_task_id = $4, steps = $5,
		tokens_in = $6, tokens_out = $7, cost_micro_usd = $8, finished_at = now() WHERE id = $1`,
		id, to, str(c.input, "summary"), task, steps, tokensIn, tokensOut, cost); err != nil {
		return nil, err
	}
	data := map[string]any{"run_id": id, "agent": agent, "status": to, "steps": steps}
	if appID != nil {
		data["application_id"] = *appID
	}
	if task != nil {
		data["escalation_task_id"] = *task
	}
	g.runEvent(c, "AgentRunFinished", id, 2, data)
	return map[string]any{"run_id": id, "status": to, "steps": steps}, nil
}

func (g *Gateway) agentRunList(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	limit, ok := num(c.input, "limit")
	if !ok {
		limit = 50
	}
	var appID *string
	if c.app != nil {
		appID = &c.app.ID
	}
	runID := str(c.input, "run_id")
	rows, err := tx.Query(ctx, `SELECT r.id::text, r.agent, r.agent_version, COALESCE(r.application_id::text, ''),
		COALESCE(a.reference, ''), COALESCE(r.trigger_event_type, ''), COALESCE(r.trigger_event_id::text, ''), r.status,
		r.steps, r.tokens_in, r.tokens_out, r.cost_micro_usd, COALESCE(r.summary, ''), COALESCE(r.escalation_task_id::text, ''),
		r.started_at, r.finished_at
		FROM agent_runs r LEFT JOIN applications a ON a.id = r.application_id
		WHERE ($1::uuid IS NULL OR r.application_id = $1) AND (NULLIF($2, '') IS NULL OR r.id = $2::uuid)
		  AND (NULLIF($3, '') IS NULL OR r.agent = $3) AND (NULLIF($4, '') IS NULL OR r.status = $4)
		ORDER BY r.started_at DESC, r.id DESC LIMIT $5`, appID, runID, str(c.input, "agent"), str(c.input, "status"), limit)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for rows.Next() {
		var id, agent, version, app, ref, trigType, trigID, status, summary, task string
		var steps int
		var tin, tout, cost int64
		var started time.Time
		var finished *time.Time
		if err := rows.Scan(&id, &agent, &version, &app, &ref, &trigType, &trigID, &status, &steps, &tin, &tout, &cost,
			&summary, &task, &started, &finished); err != nil {
			rows.Close()
			return nil, err
		}
		r := map[string]any{"run_id": id, "agent": agent, "agent_version": version, "status": status, "steps": steps,
			"tokens_in": tin, "tokens_out": tout, "cost_micro_usd": cost, "started_at": started.UTC().Format(time.RFC3339)}
		for k, v := range map[string]string{"application_id": app, "application_reference": ref, "trigger_event_type": trigType,
			"trigger_event_id": trigID, "summary": summary, "escalation_task_id": task} {
			if v != "" {
				r[k] = v
			}
		}
		if finished != nil {
			r["finished_at"] = finished.UTC().Format(time.RFC3339)
			r["duration_ms"] = finished.Sub(started).Milliseconds()
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if runID != "" {
		if len(out) == 0 {
			return nil, newErr(ErrNotFound, "run %s not found", runID)
		}
		calls, err := g.runCalls(ctx, tx, runID)
		if err != nil {
			return nil, err
		}
		out[0]["tool_calls"] = calls
	}
	return map[string]any{"runs": out}, nil
}

// runCalls lists the tool calls a run made, in order.
func (g *Gateway) runCalls(ctx context.Context, tx pgx.Tx, runID string) ([]map[string]any, error) {
	rows, err := tx.Query(ctx, `SELECT id::text, tool, outcome::text, COALESCE(error_code, ''), COALESCE(reason, ''), autonomy, started_at
		FROM tool_invocations WHERE agent_run_id = $1 ORDER BY started_at, id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	calls := []map[string]any{}
	for rows.Next() {
		var id, tool, outcome, code, reason, autonomy string
		var at time.Time
		if err := rows.Scan(&id, &tool, &outcome, &code, &reason, &autonomy, &at); err != nil {
			return nil, err
		}
		call := map[string]any{"invocation_id": id, "tool": tool, "outcome": outcome, "reason": reason, "autonomy": autonomy,
			"at": at.UTC().Format(time.RFC3339Nano)}
		if code != "" {
			call["error_code"] = code
		}
		calls = append(calls, call)
	}
	return calls, rows.Err()
}
