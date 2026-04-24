package agent

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/local/picobot/internal/chat"
	"github.com/local/picobot/internal/providers"
)

// Fake provider that returns a tool call on first chat, then returns a final message on second chat.
type FakeProvider struct {
	count int
}

func (f *FakeProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	f.count++
	if f.count == 1 {
		// request message tool
		return providers.LLMResponse{
			Content:      "Invoking message tool",
			HasToolCalls: true,
			ToolCalls:    []providers.ToolCall{{ID: "1", Name: "message", Arguments: map[string]interface{}{"content": "hello from tool"}}},
		}, nil
	}
	return providers.LLMResponse{Content: "All done!"}, nil
}
func (f *FakeProvider) GetDefaultModel() string { return "fake" }

type recordingProvider struct {
	toolNames []string
}

func (p *recordingProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	p.toolNames = p.toolNames[:0]
	for _, tool := range tools {
		p.toolNames = append(p.toolNames, tool.Name)
	}
	return providers.LLMResponse{Content: "done"}, nil
}

func (p *recordingProvider) GetDefaultModel() string { return "fake" }

func TestAgentExecutesToolCall(t *testing.T) {
	b := chat.NewHub(10)
	p := &FakeProvider{}
	ag := NewAgentLoop(b, p, p.GetDefaultModel(), 3, "", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go ag.Run(ctx)

	// send inbound
	in := chat.Inbound{Channel: "cli", SenderID: "user", ChatID: "one", Content: "trigger"}
	select {
	case b.In <- in:
	default:
		t.Fatalf("couldn't send inbound")
	}

	// expect outbound
	deadline := time.After(1 * time.Second)
	for {
		select {
		case out := <-b.Out:
			if out.Content == "All done!" {
				return
			}
			// otherwise continue waiting until timeout
		case <-deadline:
			t.Fatalf("timeout waiting for final outbound message")
		}
	}
}

func TestScheduledReminderDoesNotExposeCronTool(t *testing.T) {
	b := chat.NewHub(10)
	p := &recordingProvider{}
	ag := NewAgentLoop(b, p, p.GetDefaultModel(), 1, "", nil)

	_, err := ag.ProcessDirect("[Scheduled reminder fired] reddit-daily-brief", time.Second)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	for _, name := range p.toolNames {
		if name == "cron" {
			t.Fatalf("scheduled reminder exposed cron tool: %v", p.toolNames)
		}
	}
}

func TestRegularMessageExposesCronTool(t *testing.T) {
	b := chat.NewHub(10)
	p := &recordingProvider{}
	ag := NewAgentLoop(b, p, p.GetDefaultModel(), 1, "", nil)

	_, err := ag.ProcessDirect("schedule a reminder", time.Second)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if !slices.Contains(p.toolNames, "cron") {
		t.Fatalf("regular message did not expose cron tool: %v", p.toolNames)
	}
}

func TestScheduledReminderRunPathDoesNotExposeCronTool(t *testing.T) {
	b := chat.NewHub(10)
	p := &recordingProvider{}
	ag := NewAgentLoop(b, p, p.GetDefaultModel(), 1, "", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go ag.Run(ctx)

	b.In <- chat.Inbound{
		Channel:  "telegram",
		SenderID: "cron",
		ChatID:   "5616124731",
		Content:  "[Scheduled reminder fired] reddit-daily-brief — Please relay this to the user in a friendly way.",
	}

	select {
	case <-b.Out:
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for outbound")
	}

	for _, name := range p.toolNames {
		if name == "cron" {
			t.Fatalf("scheduled reminder run path exposed cron tool: %v", p.toolNames)
		}
	}
}

func TestExecDirForPicobotWorkspaceUsesProjectRoot(t *testing.T) {
	workspace := filepath.Join(string(filepath.Separator), "tmp", "assistant", ".picobot", "workspace")
	want := filepath.Join(string(filepath.Separator), "tmp", "assistant")

	if got := execDirForWorkspace(workspace); got != want {
		t.Fatalf("execDirForWorkspace() = %q, want %q", got, want)
	}
}

func TestScheduledReminderSuppressesFinalConfirmationAfterMessageTool(t *testing.T) {
	b := chat.NewHub(10)
	p := &FakeProvider{}
	ag := NewAgentLoop(b, p, p.GetDefaultModel(), 3, "", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go ag.Run(ctx)

	in := chat.Inbound{
		Channel:  "telegram",
		SenderID: "cron",
		ChatID:   "5616124731",
		Content:  "[Scheduled reminder fired] Frisco tasks: Contact customer service, work on setup wizard, and webshop — Please relay this to the user in a friendly way.",
	}
	select {
	case b.In <- in:
	default:
		t.Fatalf("couldn't send inbound")
	}

	select {
	case out := <-b.Out:
		if out.Content != "hello from tool" {
			t.Fatalf("expected tool message outbound, got %q", out.Content)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("timeout waiting for tool outbound message")
	}

	select {
	case out := <-b.Out:
		t.Fatalf("expected no final confirmation outbound, got %q", out.Content)
	case <-time.After(200 * time.Millisecond):
	}
}
