package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/local/picobot/internal/agent/memory"
)

func TestBuildMessagesIncludesMemories(t *testing.T) {
	cb := NewContextBuilder(".", memory.NewSimpleRanker(), 5)
	history := []string{"user: hi"}
	mems := []memory.MemoryItem{{Kind: "short", Text: "remember this"}, {Kind: "long", Text: "big fact"}}
	memCtx := "Long-term memory: important fact"
	msgs := cb.BuildMessages(history, "hello", "telegram", "123", memCtx, mems)

	// Expect at least system prompt + some system messages + user history + current
	if len(msgs) < 4 {
		t.Fatalf("expected at least 4 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Fatalf("expected first message to be system prompt, got %s", msgs[0].Role)
	}
	// find a system message containing the memory context
	foundMemCtx := false
	foundSummary := false
	for _, m := range msgs {
		if m.Role == "system" && strings.Contains(m.Content, "Long-term memory: important fact") {
			foundMemCtx = true
		}
		if m.Role == "system" && strings.Contains(m.Content, "remember this") && strings.Contains(m.Content, "big fact") {
			foundSummary = true
		}
	}
	if !foundMemCtx {
		t.Fatalf("expected memory context system message to be present in messages: %v", msgs)
	}
	if !foundSummary {
		t.Fatalf("expected memory summary to be present in messages: %v", msgs)
	}
}

func TestBuildMessagesAddsSummarizerSkillInstructionForSummarizeURL(t *testing.T) {
	t.Helper()

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "SOUL.md"), []byte("soul"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("agents"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "USER.md"), []byte("user"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "TOOLS.md"), []byte("tools"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "skills", "summarizer"), 0o755); err != nil {
		t.Fatal(err)
	}
	skill := "---\nname: summarizer\ndescription: Summarize web pages\n---\n\nUse the summarizer workflow."
	if err := os.WriteFile(filepath.Join(workspace, "skills", "summarizer", "SKILL.md"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}

	cb := NewContextBuilder(workspace, memory.NewSimpleRanker(), 5)
	msgs := cb.BuildMessages(nil, "summarize https://web4.ai/", "telegram", "123", "", nil)

	found := false
	for _, m := range msgs {
		if m.Role == "system" && strings.Contains(m.Content, "matches the skill 'summarizer'") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected summarizer skill instruction in messages: %#v", msgs)
	}
}
