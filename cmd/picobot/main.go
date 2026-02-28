package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"path/filepath"
	"strings"

	"log"

	"github.com/local/picobot/internal/agent"
	"github.com/local/picobot/internal/agent/memory"
	"github.com/local/picobot/internal/channels"
	"github.com/local/picobot/internal/chat"
	"github.com/local/picobot/internal/config"
	"github.com/local/picobot/internal/cron"
	"github.com/local/picobot/internal/heartbeat"
	"github.com/local/picobot/internal/providers"
)

const version = "0.1.0"

func NewRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "picobot",
		Short: "picobot — lightweight clawbot in Go",
	}

	rootCmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("🤖 picobot v%s\n", version)
		},
	})

	rootCmd.AddCommand(&cobra.Command{
		Use:   "onboard",
		Short: "Create default config and workspace",
		Run: func(cmd *cobra.Command, args []string) {
			cfgPath, workspacePath, err := config.Onboard()
			if err != nil {
				fmt.Fprintf(os.Stderr, "onboard failed: %v\n", err)
				return
			}
			fmt.Printf("Wrote config to %s\nInitialized workspace at %s\n", cfgPath, workspacePath)
		},
	})

	agentCmd := &cobra.Command{
		Use:   "agent",
		Short: "Run a single-shot agent query (use -m)",
		Run: func(cmd *cobra.Command, args []string) {
			msg, _ := cmd.Flags().GetString("message")
			modelFlag, _ := cmd.Flags().GetString("model")
			if msg == "" {
				fmt.Println("Specify a message with -m \"your message\"")
				return
			}

			hub := chat.NewHub(100)
			cfg, _ := config.LoadConfig()
			provider := providers.NewProviderFromConfig(cfg)

			// choose model: flag > config default > provider default
			model := modelFlag
			if model == "" && cfg.Agents.Defaults.Model != "" {
				model = cfg.Agents.Defaults.Model
			}
			if model == "" {
				model = provider.GetDefaultModel()
			}

			timeout := time.Duration(cfg.Agents.Defaults.TimeoutS) * time.Second
			if timeout <= 0 {
				timeout = 120 * time.Second
			}

			apiBase := ""
			if cfg.Providers.OpenAI != nil {
				apiBase = cfg.Providers.OpenAI.APIBase
			}
			log.Printf("agent: provider=%T model=%s timeout=%s apiBase=%s", provider, model, timeout, apiBase)

			// Create scheduler with persistence so agent can manage cron jobs.
			// Nil callback is fine — jobs fire when the gateway runs.
			persistPath := filepath.Join(cfg.Agents.Defaults.Workspace, "cron_jobs.yaml")
			scheduler := cron.NewSchedulerWithPersistence(nil, persistPath)

			ag := agent.NewAgentLoop(hub, provider, model, 50, cfg.Agents.Defaults.Workspace, scheduler)

			resp, err := ag.ProcessDirect(msg, timeout)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "error:", err)
				return
			}
			fmt.Fprintln(cmd.OutOrStdout(), resp)
		},
	}
	agentCmd.Flags().StringP("message", "m", "", "Message to send to the agent")
	agentCmd.Flags().StringP("model", "M", "", "Model to use (overrides config/provider default)")
	rootCmd.AddCommand(agentCmd)

	completeCmd := &cobra.Command{
		Use:   "complete",
		Short: "Run a single-shot provider completion without agent tools or workspace context",
		Run: func(cmd *cobra.Command, args []string) {
			msg, _ := cmd.Flags().GetString("message")
			filePath, _ := cmd.Flags().GetString("file")
			modelFlag, _ := cmd.Flags().GetString("model")

			if msg == "" && filePath == "" {
				fmt.Println("Specify a prompt with -m \"your prompt\" or -f <file>")
				return
			}
			if msg != "" && filePath != "" {
				fmt.Println("Use either -m or -f, not both")
				return
			}
			if filePath != "" {
				data, err := os.ReadFile(filePath)
				if err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "error:", err)
					return
				}
				msg = string(data)
			}

			cfg, _ := config.LoadConfig()
			provider := providers.NewProviderFromConfig(cfg)

			model := modelFlag
			if model == "" && cfg.Agents.Defaults.Model != "" {
				model = cfg.Agents.Defaults.Model
			}
			if model == "" {
				model = provider.GetDefaultModel()
			}

			timeout := time.Duration(cfg.Agents.Defaults.TimeoutS) * time.Second
			if timeout <= 0 {
				timeout = 120 * time.Second
			}

			apiBase := ""
			if cfg.Providers.OpenAI != nil {
				apiBase = cfg.Providers.OpenAI.APIBase
			}
			log.Printf("complete: provider=%T model=%s timeout=%s apiBase=%s", provider, model, timeout, apiBase)

			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			resp, err := provider.Chat(ctx, []providers.Message{{
				Role:    "user",
				Content: msg,
			}}, nil, model)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "error:", err)
				return
			}
			fmt.Fprintln(cmd.OutOrStdout(), resp.Content)
		},
	}
	completeCmd.Flags().StringP("message", "m", "", "Prompt to send to the provider")
	completeCmd.Flags().StringP("file", "f", "", "Read prompt from file")
	completeCmd.Flags().StringP("model", "M", "", "Model to use (overrides config/provider default)")
	rootCmd.AddCommand(completeCmd)

	gatewayCmd := &cobra.Command{
		Use:   "gateway",
		Short: "Start long-running gateway (agent, telegram, heartbeat)",
		Run: func(cmd *cobra.Command, args []string) {
			hub := chat.NewHub(200)
			cfg, _ := config.LoadConfig()
			provider := providers.NewProviderFromConfig(cfg)

			// choose model: flag > config > provider default
			modelFlag, _ := cmd.Flags().GetString("model")
			model := modelFlag
			if model == "" && cfg.Agents.Defaults.Model != "" {
				model = cfg.Agents.Defaults.Model
			}
			if model == "" {
				model = provider.GetDefaultModel()
			}

			// create scheduler with fire callback that routes back through the agent loop, so the LLM can process the reminder and respond naturally to the user.
			// Use persistence to survive restarts.
			persistPath := filepath.Join(cfg.Agents.Defaults.Workspace, "cron_jobs.yaml")
			scheduler := cron.NewSchedulerWithPersistence(func(job cron.Job) {
				log.Printf("cron fired: %s — %s", job.Name, job.Message)
				hub.In <- chat.Inbound{
					Channel:  job.Channel,
					SenderID: "cron",
					ChatID:   job.ChatID,
					Content:  fmt.Sprintf("[Scheduled reminder fired] %s — Please relay this to the user in a friendly way.", job.Message),
				}
			}, persistPath)

			ag := agent.NewAgentLoop(hub, provider, model, 20, cfg.Agents.Defaults.Workspace, scheduler)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// start agent loop
			go ag.Run(ctx)

			// start cron scheduler
			go scheduler.Start(ctx.Done())

			// start heartbeat
			hbInterval := time.Duration(cfg.Agents.Defaults.HeartbeatIntervalS) * time.Second
			if hbInterval <= 0 {
				hbInterval = 60 * time.Second
			}
			heartbeat.StartHeartbeat(ctx, cfg.Agents.Defaults.Workspace, hbInterval, hub)

			// start telegram if enabled
			if cfg.Channels.Telegram.Enabled {
				if err := channels.StartTelegram(ctx, hub, cfg.Channels.Telegram.Token, cfg.Channels.Telegram.AllowFrom); err != nil {
					fmt.Fprintf(os.Stderr, "failed to start telegram: %v\n", err)
				}
			}

			// wait for signal
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
			<-sigCh
			fmt.Println("shutting down gateway")
			cancel()
		},
	}
	gatewayCmd.Flags().StringP("model", "M", "", "Model to use (overrides config/provider default)")
	rootCmd.AddCommand(gatewayCmd)

	tickCmd := &cobra.Command{
		Use:   "tick",
		Short: "Process due cron jobs once and exit",
		Run: func(cmd *cobra.Command, args []string) {
			cfg, _ := config.LoadConfig()
			provider := providers.NewProviderFromConfig(cfg)

			modelFlag, _ := cmd.Flags().GetString("model")
			model := modelFlag
			if model == "" && cfg.Agents.Defaults.Model != "" {
				model = cfg.Agents.Defaults.Model
			}
			if model == "" {
				model = provider.GetDefaultModel()
			}

			persistPath := filepath.Join(cfg.Agents.Defaults.Workspace, "cron_jobs.yaml")
			scheduler := cron.NewSchedulerWithPersistence(nil, persistPath)

			due := scheduler.TickOnce(time.Now())
			if len(due) == 0 {
				fmt.Println("No due jobs.")
				return
			}

			hub := chat.NewHub(100)
			ag := agent.NewAgentLoop(hub, provider, model, 20, cfg.Agents.Defaults.Workspace, scheduler)

			for _, job := range due {
				log.Printf("tick: processing job %q: %s", job.Name, job.Message)
				prompt := fmt.Sprintf("[Scheduled reminder fired] %s — Please relay this to the user in a friendly way.", job.Message)
				timeout := time.Duration(cfg.Agents.Defaults.TimeoutS) * time.Second
				if timeout <= 0 {
					timeout = 120 * time.Second
				}
				resp, err := ag.ProcessDirect(prompt, timeout)
				if err != nil {
					log.Printf("tick: error processing job %q: %v", job.Name, err)
					continue
				}
				fmt.Printf("[%s] %s\n", job.Name, resp)
			}
		},
	}
	tickCmd.Flags().StringP("model", "M", "", "Model to use (overrides config/provider default)")
	rootCmd.AddCommand(tickCmd)

	fireCmd := &cobra.Command{
		Use:   "fire <job-name>",
		Short: "Force-fire a cron job by name with full gateway (Telegram, etc.)",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			jobName := args[0]
			cfg, _ := config.LoadConfig()
			provider := providers.NewProviderFromConfig(cfg)

			modelFlag, _ := cmd.Flags().GetString("model")
			model := modelFlag
			if model == "" && cfg.Agents.Defaults.Model != "" {
				model = cfg.Agents.Defaults.Model
			}
			if model == "" {
				model = provider.GetDefaultModel()
			}

			persistPath := filepath.Join(cfg.Agents.Defaults.Workspace, "cron_jobs.yaml")
			scheduler := cron.NewSchedulerWithPersistence(nil, persistPath)

			// Find job by name
			var target *cron.Job
			for _, j := range scheduler.List() {
				if j.Name == jobName {
					jCopy := j
					target = &jCopy
					break
				}
			}
			if target == nil {
				fmt.Fprintf(os.Stderr, "job %q not found\n", jobName)
				return
			}

			log.Printf("fire: forcing job %q: %s", target.Name, target.Message)

			hub := chat.NewHub(200)
			ag := agent.NewAgentLoop(hub, provider, model, 50, cfg.Agents.Defaults.Workspace, scheduler)

			// Use send-only Telegram so the agent's message tool can deliver mid-workflow
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cfg.Channels.Telegram.Enabled {
				channels.StartTelegramSendOnly(ctx, hub, cfg.Channels.Telegram.Token)
			}

			// Set message tool context so it can send to the right Telegram chat
			ag.SetToolContext(target.Channel, target.ChatID)

			// ProcessDirect is synchronous — runs the full tool loop and returns when done
			prompt := fmt.Sprintf("[Scheduled reminder fired] %s", target.Message)
			timeout := 10 * time.Minute
			resp, err := ag.ProcessDirect(prompt, timeout)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "error:", err)
				return
			}
			fmt.Fprintln(cmd.OutOrStdout(), resp)
		},
	}
	fireCmd.Flags().StringP("model", "M", "", "Model to use (overrides config/provider default)")
	rootCmd.AddCommand(fireCmd)

	// memory subcommands: read, append, write, recent
	memoryCmd := &cobra.Command{
		Use:   "memory",
		Short: "Inspect or modify workspace memory files",
	}

	readCmd := &cobra.Command{
		Use:   "read [today|long]",
		Short: "Read memory (today or long-term)",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			target := args[0]
			cfg, _ := config.LoadConfig()
			ws := cfg.Agents.Defaults.Workspace
			if ws == "" {
				ws = "~/.picobot/workspace"
			}
			home, _ := os.UserHomeDir()
			if strings.HasPrefix(ws, "~/") {
				ws = filepath.Join(home, ws[2:])
			}
			mem := memory.NewMemoryStoreWithWorkspace(ws, 100)
			switch target {
			case "today":
				out, _ := mem.ReadToday()
				fmt.Fprintln(cmd.OutOrStdout(), out)
			case "long":
				out, _ := mem.ReadLongTerm()
				fmt.Fprintln(cmd.OutOrStdout(), out)
			default:
				fmt.Fprintln(cmd.ErrOrStderr(), "unknown target: "+target)
			}
		},
	}

	appendCmd := &cobra.Command{
		Use:   "append [today|long] -c <content>",
		Short: "Append content to today's note or long-term memory",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			target := args[0]
			content, _ := cmd.Flags().GetString("content")
			if content == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "-c content required")
				return
			}
			cfg, _ := config.LoadConfig()
			ws := cfg.Agents.Defaults.Workspace
			if ws == "" {
				ws = "~/.picobot/workspace"
			}
			home, _ := os.UserHomeDir()
			if strings.HasPrefix(ws, "~/") {
				ws = filepath.Join(home, ws[2:])
			}
			mem := memory.NewMemoryStoreWithWorkspace(ws, 100)
			switch target {
			case "today":
				if err := mem.AppendToday(content); err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "append failed:", err)
					return
				}
				fmt.Fprintln(cmd.OutOrStdout(), "appended to today")
			case "long":
				lt, err := mem.ReadLongTerm()
				if err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "append long failed:", err)
					return
				}
				if err := mem.WriteLongTerm(lt + "\n" + content); err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "append long failed:", err)
					return
				}
				fmt.Fprintln(cmd.OutOrStdout(), "appended to long-term memory")
			default:
				fmt.Fprintln(cmd.ErrOrStderr(), "unknown target:", target)
			}
		},
	}
	appendCmd.Flags().StringP("content", "c", "", "Content to append")

	writeCmd := &cobra.Command{
		Use:   "write long -c <content>",
		Short: "Write (overwrite) long-term MEMORY.md",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			if args[0] != "long" {
				fmt.Fprintln(os.Stderr, "write currently only supports 'long'")
				return
			}
			content, _ := cmd.Flags().GetString("content")
			if content == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "-c content required")
				return
			}
			cfg, _ := config.LoadConfig()
			ws := cfg.Agents.Defaults.Workspace
			if ws == "" {
				ws = "~/.picobot/workspace"
			}
			home, _ := os.UserHomeDir()
			if strings.HasPrefix(ws, "~/") {
				ws = filepath.Join(home, ws[2:])
			}
			mem := memory.NewMemoryStoreWithWorkspace(ws, 100)
			if err := mem.WriteLongTerm(content); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "write failed:", err)
				return
			}
			fmt.Fprintln(cmd.OutOrStdout(), "wrote long-term memory")
		},
	}
	writeCmd.Flags().StringP("content", "c", "", "Content to write")

	recentCmd := &cobra.Command{
		Use:   "recent -days N",
		Short: "Show recent N days' notes",
		Run: func(cmd *cobra.Command, args []string) {
			days, _ := cmd.Flags().GetInt("days")
			cfg, _ := config.LoadConfig()
			ws := cfg.Agents.Defaults.Workspace
			if ws == "" {
				ws = "~/.picobot/workspace"
			}
			home, _ := os.UserHomeDir()
			if strings.HasPrefix(ws, "~/") {
				ws = filepath.Join(home, ws[2:])
			}
			mem := memory.NewMemoryStoreWithWorkspace(ws, 100)
			out, _ := mem.GetRecentMemories(days)
			fmt.Fprintln(cmd.OutOrStdout(), out)
		},
	}
	recentCmd.Flags().IntP("days", "d", 1, "Number of days to include")

	memoryCmd.AddCommand(readCmd)
	memoryCmd.AddCommand(appendCmd)
	memoryCmd.AddCommand(writeCmd)
	memoryCmd.AddCommand(recentCmd)

	// rank subcommand: rank recent memories by relevance to a query
	rankCmd := &cobra.Command{
		Use:   "rank -q <query>",
		Short: "Rank recent memories relative to a query",
		Run: func(cmd *cobra.Command, args []string) {
			q, _ := cmd.Flags().GetString("query")
			if q == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "-q query required")
				return
			}
			top, _ := cmd.Flags().GetInt("top")
			verbose, _ := cmd.Flags().GetBool("verbose")
			cfg, _ := config.LoadConfig()
			ws := cfg.Agents.Defaults.Workspace
			if ws == "" {
				ws = "~/.picobot/workspace"
			}
			home, _ := os.UserHomeDir()
			if strings.HasPrefix(ws, "~/") {
				ws = filepath.Join(home, ws[2:])
			}
			mem := memory.NewMemoryStoreWithWorkspace(ws, 100)
			// Build memory items from today's file (split into lines) and long-term memory
			items := make([]memory.MemoryItem, 0)
			if td, err := mem.ReadToday(); err == nil && td != "" {
				for _, line := range strings.Split(td, "\n") {
					line = strings.TrimSpace(line)
					if line == "" {
						continue
					}
					// strip leading timestamp [2026-02-07...] if present
					if idx := strings.Index(line, "] "); idx != -1 && strings.HasPrefix(line, "[") {
						line = strings.TrimSpace(line[idx+2:])
					}
					items = append(items, memory.MemoryItem{Kind: "today", Text: line})
				}
			}
			if lt, err := mem.ReadLongTerm(); err == nil && lt != "" {
				for _, line := range strings.Split(lt, "\n") {
					line = strings.TrimSpace(line)
					if line == "" {
						continue
					}
					items = append(items, memory.MemoryItem{Kind: "long", Text: line})
				}
			}
			provider := providers.NewProviderFromConfig(cfg)
			var logger *log.Logger
			if verbose {
				logger = log.New(cmd.OutOrStdout(), "ranker: ", 0)
			}
			ranker := memory.NewLLMRankerWithLogger(provider, provider.GetDefaultModel(), logger)
			res := ranker.Rank(q, items, top)
			for i, m := range res {
				fmt.Fprintf(cmd.OutOrStdout(), "%d: %s (%s)\n", i+1, m.Text, m.Kind)
			}
		},
	}
	rankCmd.Flags().StringP("query", "q", "", "Query to rank memories against")
	rankCmd.Flags().IntP("top", "k", 5, "Number of top memories to show")
	rankCmd.Flags().BoolP("verbose", "v", false, "Enable verbose diagnostic logging (to stdout)")
	memoryCmd.AddCommand(rankCmd)

	rootCmd.AddCommand(memoryCmd)
	return rootCmd
}

func main() {
	rootCmd := NewRootCmd()
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
