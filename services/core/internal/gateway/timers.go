package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Time-based triggers. Agents are woken by events, so the gateway turns the passage of
// time into events too:
//
//   IntakeReviewDue  the intake grace period after submission is over and the
//                    application is still in intake: the intake agent may now ask
//                    for whatever is missing.
//   TaskOverdue      an open task passed its due date (reported once per due date;
//                    task.remind moves the due date on).
//   AgentRunFinished for a run left open longer than staleRunAfter (the agent died
//                    before closing it), closed as failed.
//
// Ticks run in every replica; each event is claimed with a conditional update, so only
// one replica emits it.

// SchedulerActor is the principal recorded on events the scheduler emits.
const SchedulerActor = "svc:scheduler"

// RunTimers ticks until ctx ends.
func (g *Gateway) RunTimers(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := g.Tick(ctx); err != nil {
			g.Log.Warn("timer tick failed", "err", err)
		} else if n > 0 {
			g.Log.Info("timer tick", "events", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick emits the time-based events that are due now, for every tenant, and returns how
// many it emitted. Each event is its own short transaction that takes locks in the same
// order as a tool call (application row, then task row, then the event log), so a tick
// never deadlocks with in-flight calls. The claims are conditional, so replicas ticking
// at the same time do not emit twice. A tenant that fails does not stop the others.
func (g *Gateway) Tick(ctx context.Context) (int, error) {
	total := 0
	var errs []error
	for _, tenant := range g.tenantIDs {
		n, err := g.intakeReviews(ctx, tenant)
		total += n
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: intake reviews: %w", tenant, err))
		}
		m, err := g.overdueTasks(ctx, tenant)
		total += m
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: overdue tasks: %w", tenant, err))
		}
		k, err := g.abandonedRuns(ctx, tenant)
		total += k
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: abandoned runs: %w", tenant, err))
		}
	}
	return total, errors.Join(errs...)
}

// candidates runs a read-only query for the ids due in a tenant.
func (g *Gateway) candidates(ctx context.Context, tenant, query string, args ...any) ([]string, error) {
	var ids []string
	err := withTenantTx(ctx, g.DB, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}

func (g *Gateway) intakeReviews(ctx context.Context, tenant string) (int, error) {
	now := g.Now().UTC()
	ids, err := g.candidates(ctx, tenant, `SELECT id::text FROM applications
		WHERE state = 'intake' AND intake_review_at IS NULL AND submitted_at <= $1`, now.Add(-g.IntakeGrace))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		err := withTenantTx(ctx, g.DB, tenant, func(tx pgx.Tx) error {
			// Claim the application (its row lock first, like a tool call); another
			// replica or a state change since the candidate query means nothing to do.
			var version int
			err := tx.QueryRow(ctx, `UPDATE applications SET intake_review_at = $2, state_version = state_version + 1, updated_at = now()
				WHERE id = $1 AND state = 'intake' AND intake_review_at IS NULL RETURNING state_version`, id, now).Scan(&version)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			n++
			return appendEvent(ctx, tx, Event{TenantID: tenant, AggregateType: "application", AggregateID: id,
				AggregateVersion: version, Type: "IntakeReviewDue", Actor: SchedulerActor, OccurredAt: now,
				Data: map[string]any{"grace_minutes": int(g.IntakeGrace.Minutes())}})
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (g *Gateway) overdueTasks(ctx context.Context, tenant string) (int, error) {
	now := g.Now().UTC()
	ids, err := g.candidates(ctx, tenant, `SELECT id::text FROM tasks
		WHERE status IN ('open', 'in_progress') AND due_at <= $1
		  AND (overdue_notified_at IS NULL OR overdue_notified_at < due_at)`, now)
	if err != nil {
		return 0, err
	}
	limit := g.Pack.Intake.ReminderLimit()
	n := 0
	for _, id := range ids {
		err := withTenantTx(ctx, g.DB, tenant, func(tx pgx.Tx) error {
			// Lock the application first, then claim the task, then write the event.
			var app *string
			if err := tx.QueryRow(ctx, `SELECT application_id::text FROM tasks WHERE id = $1`, id).Scan(&app); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil
				}
				return err
			}
			ev := Event{TenantID: tenant, Type: "TaskOverdue", Actor: SchedulerActor, OccurredAt: now}
			if app != nil {
				var version int
				if err := tx.QueryRow(ctx, `UPDATE applications SET state_version = state_version + 1, updated_at = now()
					WHERE id = $1 RETURNING state_version`, *app).Scan(&version); err != nil {
					return err
				}
				ev.AggregateType, ev.AggregateID, ev.AggregateVersion = "application", *app, version
			}
			var kind, assignee string
			var reminders int
			var details []byte
			err := tx.QueryRow(ctx, `UPDATE tasks SET overdue_notified_at = $2
				WHERE id = $1 AND status IN ('open', 'in_progress') AND due_at <= $2
				  AND (overdue_notified_at IS NULL OR overdue_notified_at < due_at)
				RETURNING kind, assignee, reminders, details`, id, now).Scan(&kind, &assignee, &reminders, &details)
			if errors.Is(err, pgx.ErrNoRows) {
				return errNothingToDo // someone else got there first: roll the version bump back
			}
			if err != nil {
				return err
			}
			if app == nil {
				ev.AggregateType, ev.AggregateID = "task", id
				if ev.AggregateVersion, err = nextVersion(ctx, tx, "task", id); err != nil {
					return err
				}
			}
			ev.Data = map[string]any{"task_id": id, "kind": kind, "assignee": assignee, "reminders": reminders,
				"reminders_left": max(limit-reminders, 0), "details": jsonMap(details)}
			n++
			return appendEvent(ctx, tx, ev)
		})
		if err != nil && !errors.Is(err, errNothingToDo) {
			return n, err
		}
	}
	return n, nil
}

func (g *Gateway) abandonedRuns(ctx context.Context, tenant string) (int, error) {
	now := g.Now().UTC()
	ids, err := g.candidates(ctx, tenant, `SELECT id::text FROM agent_runs WHERE status = 'running' AND started_at < $1`,
		now.Add(-staleRunAfter))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		err := withTenantTx(ctx, g.DB, tenant, func(tx pgx.Tx) error {
			var agent string
			var app *string
			err := tx.QueryRow(ctx, `UPDATE agent_runs SET status = 'failed', finished_at = $2,
				summary = 'abandoned: not finished within `+staleRunAfter.String()+`'
				WHERE id = $1 AND status = 'running' AND started_at < $3 RETURNING agent, application_id::text`,
				id, now, now.Add(-staleRunAfter)).Scan(&agent, &app)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			n++
			data := map[string]any{"run_id": id, "agent": agent, "status": "failed", "summary": "abandoned"}
			if app != nil {
				data["application_id"] = *app
			}
			return appendEvent(ctx, tx, Event{TenantID: tenant, AggregateType: "agent_run", AggregateID: id, AggregateVersion: 2,
				Type: "AgentRunFinished", Actor: SchedulerActor, OccurredAt: now, Data: data})
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

var errNothingToDo = errors.New("nothing to do")

// task.remind nudges the assignee of an open task and moves its due date on.
func (g *Gateway) taskRemind(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	id := str(c.input, "task_id")
	var assignee, status, title, kind string
	var reminders int
	var due *time.Time
	err := tx.QueryRow(ctx, `SELECT assignee, status::text, title, kind, reminders, due_at FROM tasks WHERE id = $1 FOR UPDATE`, id).
		Scan(&assignee, &status, &title, &kind, &reminders, &due)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, newErr(ErrNotFound, "task %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if status != "open" && status != "in_progress" {
		return nil, &ToolError{Code: ErrStateConflict, Message: "task is already " + status, Details: map[string]any{"status": status}}
	}
	// Reminders are for things asked of the borrower; staff tasks are not nudged this way.
	if kind != "document_request" {
		return nil, &ToolError{Code: ErrPrecondition, Message: "only document requests take reminders", Details: map[string]any{"kind": kind}}
	}
	limit := g.Pack.Intake.ReminderLimit()
	if reminders >= limit {
		return nil, &ToolError{Code: ErrStateConflict, Message: "the reminder limit is reached; hand the task to a person",
			Details: map[string]any{"reminders": reminders, "limit": limit}}
	}
	via := "task"
	if strings.HasPrefix(assignee, "borrower:") {
		if _, err := g.Connectors.Notify.Send(ctx, "portal", assignee, "task_reminder",
			map[string]any{"task_id": id, "title": title, "message": str(c.input, "message")}); err == nil {
			via = "portal"
		}
	}
	// The due date moves on from now or from the current due date, whichever is later,
	// so an early reminder never shortens the borrower's time.
	from := g.Now().UTC()
	if due != nil && due.After(from) {
		from = due.UTC()
	}
	newDue := from.Add(g.Pack.Intake.ReminderInterval())
	if _, err := tx.Exec(ctx, `UPDATE tasks SET reminders = reminders + 1, last_reminded_at = $2, due_at = $3 WHERE id = $1`,
		id, g.Now().UTC(), newDue); err != nil {
		return nil, err
	}
	due = &newDue
	reminders++
	if err := g.taskEvent(ctx, tx, c, "TaskReminded", id, map[string]any{"task_id": id, "reminders": reminders,
		"due_at": due.Format(time.RFC3339), "notified_via": via}); err != nil {
		return nil, err
	}
	return map[string]any{"task_id": id, "reminders": reminders, "reminders_left": limit - reminders,
		"due_at": due.Format(time.RFC3339), "notified_via": via}, nil
}
