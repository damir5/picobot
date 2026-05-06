package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/local/picobot/internal/chat"
	"github.com/local/picobot/internal/providers"
)

type scheduledBriefCountingProvider struct {
	calls atomic.Int32
}

func (p *scheduledBriefCountingProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	p.calls.Add(1)
	return providers.LLMResponse{Content: "provider should not handle scheduled brief"}, nil
}

func (p *scheduledBriefCountingProvider) GetDefaultModel() string { return "test" }

func TestScheduledBriefRunPathExecutesHelperWithoutProvider(t *testing.T) {
	projectRoot := t.TempDir()
	workspace := filepath.Join(projectRoot, ".picobot", "workspace")
	scriptsDir := filepath.Join(projectRoot, "scripts")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	script := `#!/bin/sh
printf '%s\n' '{"status":"published","date":"2026-05-02","url":"https://example.test/slack.html","summary":["- one useful item"],"markdownPath":"/tmp/slack.md","htmlPath":"/tmp/slack.html"}'
`
	if err := os.WriteFile(filepath.Join(scriptsDir, "slack-daily-brief.ts"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	hub := chat.NewHub(10)
	provider := &scheduledBriefCountingProvider{}
	agent := NewAgentLoop(hub, provider, provider.GetDefaultModel(), 3, workspace, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go agent.Run(ctx)

	hub.In <- chat.Inbound{
		Channel:  "telegram",
		SenderID: "cron",
		ChatID:   "5616124731",
		Content:  "[Scheduled reminder fired] slack-daily-brief - Please relay this to the user in a friendly way.",
	}

	select {
	case out := <-hub.Out:
		if !strings.Contains(out.Content, "Slack daily brief published for 2026-05-02.") {
			t.Fatalf("unexpected outbound content: %q", out.Content)
		}
		if !strings.Contains(out.Content, "https://example.test/slack.html") {
			t.Fatalf("expected published URL in outbound content: %q", out.Content)
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for scheduled brief outbound")
	}

	if calls := provider.calls.Load(); calls != 0 {
		t.Fatalf("scheduled brief called provider %d time(s)", calls)
	}
}

func TestScheduledBriefTriggerRequiresExactTriggerToken(t *testing.T) {
	tests := []struct {
		content string
		wantOK  bool
	}{
		{"slack-daily-brief", true},
		{"slack-daily-brief — Please relay this", true},
		{"[Scheduled reminder fired] slack-daily-brief — Please relay this", true},
		{"slack-daily-briefing", false},
		{"[Scheduled reminder fired] discord-daily-brief-old", false},
	}

	for _, tt := range tests {
		_, gotOK := scheduledBriefTrigger(tt.content)
		if gotOK != tt.wantOK {
			t.Fatalf("scheduledBriefTrigger(%q) ok = %v, want %v", tt.content, gotOK, tt.wantOK)
		}
	}
}
