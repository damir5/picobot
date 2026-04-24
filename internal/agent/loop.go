package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/local/picobot/internal/agent/memory"
	"github.com/local/picobot/internal/agent/tools"
	"github.com/local/picobot/internal/chat"
	"github.com/local/picobot/internal/cron"
	"github.com/local/picobot/internal/providers"
	"github.com/local/picobot/internal/session"
)

var rememberRE = regexp.MustCompile(`(?i)^remember(?:\s+to)?\s+(.+)$`)
var summarizeURLFastPathRE = regexp.MustCompile(`(?i)^\s*summar(?:ize|ise)\s+(https?://\S+)\s*$`)

// AgentLoop is the core processing loop; it holds an LLM provider, tools, sessions and context builder.
type AgentLoop struct {
	hub           *chat.Hub
	provider      providers.LLMProvider
	tools         *tools.Registry
	sessions      *session.SessionManager
	context       *ContextBuilder
	memory        *memory.MemoryStore
	model         string
	maxIterations int
	running       bool
	workspace     string
}

type messageSendTracker interface {
	ResetSendState()
	SentCount() int
}

// NewAgentLoop creates a new AgentLoop with the given provider.
func NewAgentLoop(b *chat.Hub, provider providers.LLMProvider, model string, maxIterations int, workspace string, scheduler *cron.Scheduler) *AgentLoop {
	if model == "" {
		model = provider.GetDefaultModel()
	}
	if workspace == "" {
		workspace = "."
	}
	reg := tools.NewRegistry()
	// register default tools
	reg.Register(tools.NewMessageTool(b))

	// Open an os.Root anchored at the workspace for kernel-enforced sandboxing.
	root, err := os.OpenRoot(workspace)
	if err != nil {
		log.Fatalf("failed to open workspace root %q: %v", workspace, err)
	}

	fsTool, err := tools.NewFilesystemTool(workspace)
	if err != nil {
		log.Fatalf("failed to create filesystem tool: %v", err)
	}
	reg.Register(fsTool)

	// Some workflows triggered via Telegram legitimately need longer than 60s
	// (for example summarization pipelines that extract, synthesize, and render).
	reg.Register(tools.NewExecToolWithWorkspace(300, execDirForWorkspace(workspace)))
	reg.Register(tools.NewWebTool())
	reg.Register(tools.NewSpawnTool())
	reg.Register(tools.NewCronTool(scheduler, provider, model))

	sm := session.NewSessionManager(workspace)
	ctx := NewContextBuilder(workspace, memory.NewLLMRanker(provider, model), 5)
	mem := memory.NewMemoryStoreWithWorkspace(workspace, 100)
	// register memory tool (needs store instance)
	reg.Register(tools.NewWriteMemoryTool(mem))

	// register skill management tools (share the same os.Root)
	skillMgr := tools.NewSkillManager(root)
	reg.Register(tools.NewCreateSkillTool(skillMgr))
	reg.Register(tools.NewListSkillsTool(skillMgr))
	reg.Register(tools.NewReadSkillTool(skillMgr))
	reg.Register(tools.NewDeleteSkillTool(skillMgr))

	return &AgentLoop{hub: b, provider: provider, tools: reg, sessions: sm, context: ctx, memory: mem, model: model, maxIterations: maxIterations, workspace: workspace}
}

// Run starts processing inbound messages. This is a blocking call until context is canceled.
func (a *AgentLoop) Run(ctx context.Context) {
	a.running = true
	log.Println("Agent loop started")

	for a.running {
		select {
		case <-ctx.Done():
			log.Println("Agent loop received shutdown signal")
			a.running = false
			return
		case msg, ok := <-a.hub.In:
			if !ok {
				log.Println("Inbound channel closed, stopping agent loop")
				a.running = false
				return
			}

			log.Printf("Processing message from %s:%s\n", msg.Channel, msg.SenderID)

			// Quick heuristic: if user asks the agent to remember something explicitly,
			// store it in today's note and reply immediately without calling the LLM.
			trimmed := strings.TrimSpace(msg.Content)
			isScheduledReminder := strings.HasPrefix(trimmed, "[Scheduled reminder fired]")
			if matches := summarizeURLFastPathRE.FindStringSubmatch(trimmed); len(matches) == 2 {
				reply, err := a.runSummarizerFastPath(matches[1])
				if err != nil {
					reply = fmt.Sprintf("Summarizer failed: %v", err)
				}

				session := a.sessions.GetOrCreate(msg.Channel + ":" + msg.ChatID)
				session.AddMessage("user", msg.Content)
				session.AddMessage("assistant", reply)
				a.sessions.Save(session)

				out := chat.Outbound{Channel: msg.Channel, ChatID: msg.ChatID, Content: reply}
				select {
				case a.hub.Out <- out:
				default:
					log.Println("Outbound channel full, dropping message")
				}
				continue
			}
			rememberRe := rememberRE
			if matches := rememberRe.FindStringSubmatch(trimmed); len(matches) == 2 {
				note := matches[1]
				if err := a.memory.AppendToday(note); err != nil {
					log.Printf("error appending to memory: %v", err)
				}
				out := chat.Outbound{Channel: msg.Channel, ChatID: msg.ChatID, Content: "OK, I've remembered that."}
				select {
				case a.hub.Out <- out:
				default:
					log.Println("Outbound channel full, dropping message")
				}
				// save to session as well
				session := a.sessions.GetOrCreate(msg.Channel + ":" + msg.ChatID)
				session.AddMessage("user", msg.Content)
				session.AddMessage("assistant", "OK, I've remembered that.")
				a.sessions.Save(session)
				continue
			}

			// Set tool context (so message tool knows channel+chat)
			if mt := a.tools.Get("message"); mt != nil {
				if mtool, ok := mt.(interface{ SetContext(string, string) }); ok {
					mtool.SetContext(msg.Channel, msg.ChatID)
				}
				if tracker, ok := mt.(messageSendTracker); ok {
					tracker.ResetSendState()
				}
			}
			if ct := a.tools.Get("cron"); ct != nil {
				if ctool, ok := ct.(interface{ SetContext(string, string) }); ok {
					ctool.SetContext(msg.Channel, msg.ChatID)
				}
			}

			// Build messages from session, long-term memory, and recent memory
			session := a.sessions.GetOrCreate(msg.Channel + ":" + msg.ChatID)
			// get file-backed memory context (long-term + today)
			memCtx, _ := a.memory.GetMemoryContext()
			memories := a.memory.Recent(5)
			messages := a.context.BuildMessages(session.GetHistory(), msg.Content, msg.Channel, msg.ChatID, memCtx, memories)

			iteration := 0
			finalContent := ""
			lastToolResult := ""
			toolDefs := toolDefinitionsForMessage(a.tools.Definitions(), isScheduledReminder)
			for iteration < a.maxIterations {
				iteration++
				log.Printf("agent: iteration %d/%d model=%s messages=%d tools=%d", iteration, a.maxIterations, a.model, len(messages), len(toolDefs))
				resp, err := a.provider.Chat(ctx, messages, toolDefs, a.model)
				if err != nil {
					log.Printf("provider error on iteration %d: %v", iteration, err)
					finalContent = fmt.Sprintf("Error processing request (attempt %d): %v", iteration, err)
					break
				}

				if resp.HasToolCalls {
					// append assistant message with tool_calls attached
					messages = append(messages, providers.Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls})
					// Execute each tool call and return results with "tool" role
					for _, tc := range resp.ToolCalls {
						if !toolAllowedForMessage(tc.Name, isScheduledReminder) {
							res := fmt.Sprintf("(tool error) tool %q is unavailable while processing a scheduled reminder", tc.Name)
							lastToolResult = res
							messages = append(messages, providers.Message{Role: "tool", Content: res, ToolCallID: tc.ID})
							continue
						}
						res, err := a.tools.Execute(ctx, tc.Name, tc.Arguments)
						if err != nil {
							res = "(tool error) " + err.Error()
						}
						lastToolResult = res
						messages = append(messages, providers.Message{Role: "tool", Content: res, ToolCallID: tc.ID})
					}
					// loop again
					continue
				} else {
					finalContent = resp.Content
					break
				}
			}

			if finalContent == "" && lastToolResult != "" {
				finalContent = lastToolResult
			} else if finalContent == "" {
				finalContent = "I've completed processing but have no response to give."
			}

			suppressFinalOutbound := false
			if mt := a.tools.Get("message"); mt != nil {
				if tracker, ok := mt.(messageSendTracker); ok {
					suppressFinalOutbound = isScheduledReminder && tracker.SentCount() > 0
				}
			}

			// Save session
			session.AddMessage("user", msg.Content)
			if suppressFinalOutbound {
				session.AddMessage("assistant", "(sent scheduled reminder)")
			} else {
				session.AddMessage("assistant", finalContent)
			}
			a.sessions.Save(session)

			if suppressFinalOutbound {
				continue
			}

			out := chat.Outbound{Channel: msg.Channel, ChatID: msg.ChatID, Content: finalContent}
			select {
			case a.hub.Out <- out:
			default:
				log.Println("Outbound channel full, dropping message")
			}
		default:
			// idle tick
			time.Sleep(100 * time.Millisecond)
		}
	}
}

type summarizeScriptResult struct {
	Title        string `json:"title"`
	ArtifactDir  string `json:"artifactDir"`
	PublishedURL string `json:"publishedUrl"`
	Gist         string `json:"gist"`
	Status       string `json:"status"`
}

type summarizeArtifact struct {
	Sections struct {
		TLDR struct {
			CoreTakeaway string `json:"coreTakeaway"`
			Markdown     string `json:"markdown"`
		} `json:"tldr"`
	} `json:"sections"`
}

func (a *AgentLoop) runSummarizerFastPath(sourceURL string) (string, error) {
	scriptPath := filepath.Clean(filepath.Join(a.workspace, "..", "..", "scripts", "summarize-source.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, scriptPath, "--source", sourceURL, "--no-publish", "--skip-audio")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}

	var result summarizeScriptResult
	if err := json.Unmarshal(out, &result); err != nil {
		return "", fmt.Errorf("parse summarizer output: %w", err)
	}
	if result.ArtifactDir == "" {
		return "", fmt.Errorf("summarizer returned no artifact directory")
	}

	data, err := os.ReadFile(filepath.Join(result.ArtifactDir, "summary.json"))
	if err != nil {
		return "", fmt.Errorf("read summary artifact: %w", err)
	}
	var artifact summarizeArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return "", fmt.Errorf("parse summary artifact: %w", err)
	}

	tldr := strings.TrimSpace(artifact.Sections.TLDR.Markdown)
	if tldr == "" {
		tldr = strings.TrimSpace(result.Gist)
	}
	if tldr == "" {
		return "", fmt.Errorf("summary artifact missing TLDR")
	}
	return tldr, nil
}

// SetToolContext sets channel and chatID on tools that need routing context (message, cron).
func (a *AgentLoop) SetToolContext(channel, chatID string) {
	if mt := a.tools.Get("message"); mt != nil {
		if mtool, ok := mt.(interface{ SetContext(string, string) }); ok {
			mtool.SetContext(channel, chatID)
		}
	}
	if ct := a.tools.Get("cron"); ct != nil {
		if ctool, ok := ct.(interface{ SetContext(string, string) }); ok {
			ctool.SetContext(channel, chatID)
		}
	}
}

func toolDefinitionsForMessage(defs []providers.ToolDefinition, isScheduledReminder bool) []providers.ToolDefinition {
	if !isScheduledReminder {
		return defs
	}

	filtered := make([]providers.ToolDefinition, 0, len(defs))
	for _, def := range defs {
		if toolAllowedForMessage(def.Name, isScheduledReminder) {
			filtered = append(filtered, def)
		}
	}
	return filtered
}

func toolAllowedForMessage(name string, isScheduledReminder bool) bool {
	if !isScheduledReminder {
		return true
	}
	return name != "cron"
}

func execDirForWorkspace(workspace string) string {
	clean := filepath.Clean(workspace)
	if filepath.Base(clean) == "workspace" && filepath.Base(filepath.Dir(clean)) == ".picobot" {
		return filepath.Dir(filepath.Dir(clean))
	}
	return clean
}

// ProcessDirect sends a message directly to the provider and returns the response.
// It supports tool calling - if the model requests tools, they will be executed.
func (a *AgentLoop) ProcessDirect(content string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Build full context (bootstrap files, skills, memory) just like the main loop
	memCtx, _ := a.memory.GetMemoryContext()
	memories := a.memory.Recent(5)
	messages := a.context.BuildMessages(nil, content, "cli", "direct", memCtx, memories)
	isScheduledReminder := strings.HasPrefix(strings.TrimSpace(content), "[Scheduled reminder fired]")

	// Support tool calling iterations (similar to main loop)
	toolDefs := toolDefinitionsForMessage(a.tools.Definitions(), isScheduledReminder)
	var lastToolResult string
	for iteration := 0; iteration < a.maxIterations; iteration++ {
		log.Printf("direct: iteration %d/%d model=%s messages=%d tools=%d", iteration+1, a.maxIterations, a.model, len(messages), len(toolDefs))
		resp, err := a.provider.Chat(ctx, messages, toolDefs, a.model)
		if err != nil {
			log.Printf("direct: provider error on iteration %d: %v", iteration+1, err)
			return "", fmt.Errorf("provider error (iteration %d/%d): %w", iteration+1, a.maxIterations, err)
		}

		if !resp.HasToolCalls {
			// No tool calls, return the response (fall back to last tool result if empty)
			if resp.Content != "" {
				return resp.Content, nil
			}
			if lastToolResult != "" {
				return lastToolResult, nil
			}
			return resp.Content, nil
		}

		// Execute tool calls
		messages = append(messages, providers.Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls})
		for _, tc := range resp.ToolCalls {
			if !toolAllowedForMessage(tc.Name, isScheduledReminder) {
				result := fmt.Sprintf("(tool error) tool %q is unavailable while processing a scheduled reminder", tc.Name)
				lastToolResult = result
				messages = append(messages, providers.Message{Role: "tool", Content: result, ToolCallID: tc.ID})
				continue
			}
			result, err := a.tools.Execute(ctx, tc.Name, tc.Arguments)
			if err != nil {
				result = "(tool error) " + err.Error()
			}
			lastToolResult = result
			messages = append(messages, providers.Message{Role: "tool", Content: result, ToolCallID: tc.ID})
		}
	}

	return "Max iterations reached without final response", nil
}
