-- Tamarind LOS: time-based triggers (intake review, overdue tasks) and reminders

BEGIN;
SET search_path = tamarind, public;

-- When the scheduler announced that the intake grace period was over (IntakeReviewDue).
ALTER TABLE applications ADD COLUMN intake_review_at timestamptz;

-- Reminders sent for a task, and when the scheduler last reported it overdue.
ALTER TABLE tasks ADD COLUMN reminders integer NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN last_reminded_at timestamptz;
ALTER TABLE tasks ADD COLUMN overdue_notified_at timestamptz;
CREATE INDEX tasks_due_idx ON tasks (due_at) WHERE status IN ('open', 'in_progress');

COMMIT;
