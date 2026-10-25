package gateway

import (
	"context"
	"errors"
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
//
// Ticks run in every replica; a transaction-scoped advisory lock lets only one of them
// do the work at a time.

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

// Tick emits the time-based events that are due now, for every tenant. It returns how
// many events it emitted.
func (g *Gateway) Tick(ctx context.Context) (int, error) {
	total := 0
	for _, tenant := range g.tenantIDs {
		err := withTenantTx(ctx, g.DB, tenant, func(tx pgx.Tx) error {
			var leader bool
			if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtext('tamarind:timers:' || $1))`, tenant).
				Scan(&leader); err != nil || !leader {
				return err
			}
			n, err := g.intakeReviews(ctx, tx, tenant)
			if err != nil {
				return err
			}
			m, err := g.overdueTasks(ctx, tx, tenant)
			total += n + m
			return err
		})
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (g *Gateway) intakeReviews(ctx context.Context, tx pgx.Tx, tenant string) (int, error) {
	now := g.Now().UTC()
	rows, err := tx.Query(ctx, `UPDATE applications SET intake_review_at = $1, state_version = state_version + 1, updated_at = now()
		WHERE state = 'intake' AND intake_review_at IS NULL AND submitted_at <= $2
		RETURNING id::text, state_version`, now, now.Add(-g.IntakeGrace))
	if err != nil {
		return 0, err
	}
	type due struct {
		id      string
		version int
	}
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.version); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, d := range list {
		if err := appendEvent(ctx, tx, Event{TenantID: tenant, AggregateType: "application", AggregateID: d.id,
			AggregateVersion: d.version, Type: "IntakeReviewDue", Actor: SchedulerActor, OccurredAt: now,
			Data: map[string]any{"grace_minutes": int(g.IntakeGrace.Minutes())}}); err != nil {
			return 0, err
		}
	}
	return len(list), nil
}

func (g *Gateway) overdueTasks(ctx context.Context, tx pgx.Tx, tenant string) (int, error) {
	now := g.Now().UTC()
	rows, err := tx.Query(ctx, `UPDATE tasks SET overdue_notified_at = $1
		WHERE status IN ('open', 'in_progress') AND due_at <= $1
		  AND (overdue_notified_at IS NULL OR overdue_notified_at < due_at)
		RETURNING id::text, COALESCE(application_id::text, ''), kind, assignee, reminders, details`, now)
	if err != nil {
		return 0, err
	}
	type overdue struct {
		id, app, kind, assignee string
		reminders               int
		details                 []byte
	}
	var list []overdue
	for rows.Next() {
		var o overdue
		if err := rows.Scan(&o.id, &o.app, &o.kind, &o.assignee, &o.reminders, &o.details); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	limit := g.Pack.Intake.ReminderLimit()
	for _, o := range list {
		data := map[string]any{"task_id": o.id, "kind": o.kind, "assignee": o.assignee, "reminders": o.reminders,
			"reminders_left": max(limit-o.reminders, 0), "details": jsonMap(o.details)}
		ev := Event{TenantID: tenant, Type: "TaskOverdue", Actor: SchedulerActor, OccurredAt: now, Data: data}
		if o.app != "" {
			ev.AggregateType, ev.AggregateID = "application", o.app
			if err := tx.QueryRow(ctx, `UPDATE applications SET state_version = state_version + 1, updated_at = now()
				WHERE id = $1 RETURNING state_version`, o.app).Scan(&ev.AggregateVersion); err != nil {
				return 0, err
			}
		} else {
			ev.AggregateType, ev.AggregateID = "task", o.id
			if ev.AggregateVersion, err = nextVersion(ctx, tx, "task", o.id); err != nil {
				return 0, err
			}
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return 0, err
		}
	}
	return len(list), nil
}

// task.remind nudges the assignee of an open task and moves its due date on.
func (g *Gateway) taskRemind(ctx context.Context, tx pgx.Tx, c *call) (any, error) {
	id := str(c.input, "task_id")
	var assignee, status, title string
	var reminders int
	err := tx.QueryRow(ctx, `SELECT assignee, status::text, title, reminders FROM tasks WHERE id = $1 FOR UPDATE`, id).
		Scan(&assignee, &status, &title, &reminders)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, newErr(ErrNotFound, "task %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if status != "open" && status != "in_progress" {
		return nil, &ToolError{Code: ErrStateConflict, Message: "task is already " + status, Details: map[string]any{"status": status}}
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
	due := g.Now().UTC().Add(g.Pack.Intake.ReminderInterval())
	if _, err := tx.Exec(ctx, `UPDATE tasks SET reminders = reminders + 1, last_reminded_at = $2, due_at = $3 WHERE id = $1`,
		id, g.Now().UTC(), due); err != nil {
		return nil, err
	}
	reminders++
	if err := g.taskEvent(ctx, tx, c, "TaskReminded", id, map[string]any{"task_id": id, "reminders": reminders,
		"due_at": due.Format(time.RFC3339), "notified_via": via}); err != nil {
		return nil, err
	}
	return map[string]any{"task_id": id, "reminders": reminders, "reminders_left": limit - reminders,
		"due_at": due.Format(time.RFC3339), "notified_via": via}, nil
}
