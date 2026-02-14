package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/local/picobot/internal/cron"
	"github.com/local/picobot/internal/providers"
)

// CronTool schedules delayed/recurring tasks via the cron scheduler.
// It holds a channel/chatID context (set per-incoming-message) so fired jobs
// know where to send their notification.
type CronTool struct {
	scheduler *cron.Scheduler
	provider  providers.LLMProvider
	model     string
	channel   string
	chatID    string
}

func NewCronTool(scheduler *cron.Scheduler, provider providers.LLMProvider, model string) *CronTool {
	return &CronTool{scheduler: scheduler, provider: provider, model: model}
}

func (t *CronTool) Name() string { return "cron" }
func (t *CronTool) Description() string {
	return "Schedule one-time or recurring reminders/tasks. Actions: add (schedule), list (show pending), cancel (remove by name)."
}

func (t *CronTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"action": map[string]interface{}{
				"type":        "string",
				"description": "The action: add (schedule a new job), list (show pending jobs), cancel (remove a job by name)",
				"enum":        []string{"add", "list", "cancel"},
			},
			"name": map[string]interface{}{
				"type":        "string",
				"description": "A short name for the job (used to identify it for cancellation)",
			},
			"message": map[string]interface{}{
				"type":        "string",
				"description": "The reminder message or task description to deliver when the job fires",
			},
			"delay": map[string]interface{}{
				"type":        "string",
				"description": "How long to wait before first firing, e.g. '2m', '1h30m', '30s', '1h'. Uses Go duration format.",
			},
			"recurring": map[string]interface{}{
				"type":        "boolean",
				"description": "If true, the job will repeat at the specified interval. If false or omitted, fires only once.",
			},
			"interval": map[string]interface{}{
				"type":        "string",
				"description": "For recurring jobs: how often to repeat (minimum 2m). Uses Go duration format.",
			},
			"schedule": map[string]interface{}{
				"type":        "string",
				"description": "A cron expression (5-field: minute hour day month weekday) e.g. '0 0 * * *' for midnight daily. When set, delay/recurring/interval are ignored.",
			},
			"when": map[string]interface{}{
				"type":        "string",
				"description": "Natural language schedule description e.g. 'every day at midnight', 'weekdays at 8am'. Converted to cron expression via LLM. When set, delay/recurring/interval are ignored.",
			},
		},
		"required": []string{"action"},
	}
}

// SetContext sets the originating channel and chat for scheduled jobs.
func (t *CronTool) SetContext(channel, chatID string) {
	t.channel = channel
	t.chatID = chatID
}

func (t *CronTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	action, _ := args["action"].(string)

	switch action {
	case "add":
		name, _ := args["name"].(string)
		message, _ := args["message"].(string)
		delayStr, _ := args["delay"].(string)
		recurring, _ := args["recurring"].(bool)
		intervalStr, _ := args["interval"].(string)
		schedule, _ := args["schedule"].(string)
		when, _ := args["when"].(string)

		if name == "" {
			name = "reminder"
		}
		if message == "" {
			return "", fmt.Errorf("cron add: 'message' is required")
		}

		// If "when" is provided, convert to cron expression via LLM
		if when != "" && schedule == "" {
			expr, err := t.naturalLanguageToCron(ctx, when)
			if err != nil {
				return "", fmt.Errorf("cron add: failed to parse schedule %q: %v", when, err)
			}
			schedule = expr
		}

		// If schedule (cron expression) is provided, use cron-based scheduling
		if schedule != "" {
			id, err := t.scheduler.AddCron(name, message, schedule, t.channel, t.chatID)
			if err != nil {
				return "", fmt.Errorf("cron add: %v", err)
			}
			jobs := t.scheduler.List()
			for _, j := range jobs {
				if j.ID == id {
					return fmt.Sprintf("Scheduled cron job %q (id: %s, schedule: %s). Next fire: %s.", name, id, schedule, j.FireAt.Format(time.RFC822)), nil
				}
			}
			return fmt.Sprintf("Scheduled cron job %q (id: %s, schedule: %s).", name, id, schedule), nil
		}

		// Fall back to delay-based scheduling
		if delayStr == "" {
			return "", fmt.Errorf("cron add: 'delay', 'schedule', or 'when' is required")
		}

		delay, err := time.ParseDuration(delayStr)
		if err != nil {
			return "", fmt.Errorf("cron add: invalid delay %q: %v", delayStr, err)
		}
		if delay <= 0 {
			return "", fmt.Errorf("cron add: delay must be positive")
		}

		// Handle recurring jobs
		if recurring {
			if intervalStr == "" {
				intervalStr = delayStr // use delay as interval if not specified
			}
			interval, err := time.ParseDuration(intervalStr)
			if err != nil {
				return "", fmt.Errorf("cron add: invalid interval %q: %v", intervalStr, err)
			}
			// Enforce minimum 2-minute interval to prevent abuse
			if interval < 2*time.Minute {
				return "", fmt.Errorf("cron add: recurring interval must be at least 2m (got %v)", interval)
			}
			id := t.scheduler.AddRecurring(name, message, interval, t.channel, t.chatID)
			return fmt.Sprintf("Scheduled recurring job %q (id: %s). Will fire in %v, then repeat every %v.", name, id, delay, interval), nil
		}

		// One-time job
		id := t.scheduler.Add(name, message, delay, t.channel, t.chatID)
		return fmt.Sprintf("Scheduled job %q (id: %s). Will fire in %v.", name, id, delay), nil

	case "list":
		jobs := t.scheduler.List()
		if len(jobs) == 0 {
			return "No pending jobs.", nil
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("%d pending job(s):\n", len(jobs)))
		for _, j := range jobs {
			remaining := time.Until(j.FireAt).Round(time.Second)
			if j.CronExpr != "" {
				sb.WriteString(fmt.Sprintf("- %s (%s): %q — schedule: %s, next: %s (in %v)\n", j.Name, j.ID, j.Message, j.CronExpr, j.FireAt.Format(time.RFC822), remaining))
			} else {
				sb.WriteString(fmt.Sprintf("- %s (%s): %q — fires in %v\n", j.Name, j.ID, j.Message, remaining))
			}
		}
		return sb.String(), nil

	case "cancel":
		name, _ := args["name"].(string)
		if name == "" {
			return "", fmt.Errorf("cron cancel: 'name' is required")
		}
		if t.scheduler.CancelByName(name) {
			return fmt.Sprintf("Cancelled job %q.", name), nil
		}
		return fmt.Sprintf("No job found with name %q.", name), nil

	default:
		return "", fmt.Errorf("cron: unknown action %q (use add, list, or cancel)", action)
	}
}

// naturalLanguageToCron uses the LLM to convert a natural language schedule to a cron expression.
func (t *CronTool) naturalLanguageToCron(ctx context.Context, when string) (string, error) {
	if t.provider == nil {
		return "", fmt.Errorf("LLM provider not configured; use 'schedule' with a cron expression instead of 'when'")
	}

	prompt := fmt.Sprintf(
		"Convert this to a 5-field cron expression (minute hour day-of-month month day-of-week). "+
			"Only respond with the cron expression, nothing else. No explanation, no backticks.\n\nInput: %q", when)

	messages := []providers.Message{
		{Role: "user", Content: prompt},
	}

	resp, err := t.provider.Chat(ctx, messages, nil, t.model)
	if err != nil {
		return "", fmt.Errorf("LLM call failed: %v", err)
	}

	expr := strings.TrimSpace(resp.Content)
	// Basic validation: should have exactly 5 fields
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return "", fmt.Errorf("LLM returned invalid cron expression %q (expected 5 fields, got %d)", expr, len(fields))
	}

	return expr, nil
}
