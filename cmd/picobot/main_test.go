package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/local/picobot/internal/agent/memory"
	"github.com/local/picobot/internal/config"
	"github.com/local/picobot/internal/cron"
)

func TestMemoryCLI_ReadAppendWriteRecent(t *testing.T) {
	// set HOME to a temp dir so onboard writes to temp
	tmp := t.TempDir()
	os.Setenv("HOME", tmp)

	// create default config + workspace
	if _, _, err := config.Onboard(); err != nil {
		t.Fatalf("onboard failed: %v", err)
	}

	// run: picobot memory append today -c "hello"
	cmd := NewRootCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"memory", "append", "today", "-c", "hello"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("append today failed: %v", err)
	}

	// verify today's file exists
	cfg, _ := config.LoadConfig()
	ws := cfg.Agents.Defaults.Workspace
	if strings.HasPrefix(ws, "~") {
		home, _ := os.UserHomeDir()
		ws = filepath.Join(home, ws[2:])
	}
	memFile := filepath.Join(ws, "memory")
	files, _ := os.ReadDir(memFile)
	found := false
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".md") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected memory files, none found in %s", memFile)
	}

	// write long-term
	cmd = NewRootCmd()
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"memory", "write", "long", "-c", "LONGCONTENT"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("write long failed: %v", err)
	}

	// read long-term
	cmd = NewRootCmd()
	readBuf := &bytes.Buffer{}
	cmd.SetOut(readBuf)
	cmd.SetArgs([]string{"memory", "read", "long"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("read long failed: %v", err)
	}
	out := readBuf.String()
	if !strings.Contains(out, "LONGCONTENT") {
		t.Fatalf("expected LONGCONTENT in output, got %q", out)
	}

	// recent days
	cmd = NewRootCmd()
	recentBuf := &bytes.Buffer{}
	cmd.SetOut(recentBuf)
	cmd.SetArgs([]string{"memory", "recent", "--days", "1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("recent failed: %v", err)
	}
	if recentBuf.String() == "" {
		t.Fatalf("expected recent output, got empty")
	}
}

func TestMemoryCLI_Rank(t *testing.T) {
	// set HOME to a temp dir so onboard writes to temp
	tmp := t.TempDir()
	os.Setenv("HOME", tmp)

	// create default config + workspace
	if _, _, err := config.Onboard(); err != nil {
		t.Fatalf("onboard failed: %v", err)
	}

	// append some memories
	cfg, _ := config.LoadConfig()
	ws := cfg.Agents.Defaults.Workspace
	if strings.HasPrefix(ws, "~") {
		home, _ := os.UserHomeDir()
		ws = filepath.Join(home, ws[2:])
	}
	mem := memory.NewMemoryStoreWithWorkspace(ws, 100)
	_ = mem.AppendToday("buy milk and eggs")
	_ = mem.AppendToday("call mom tomorrow")
	_ = mem.AppendToday("milkshake recipe")

	// run rank command
	cmd := NewRootCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"memory", "rank", "-q", "milk", "-k", "2"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("rank failed: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "buy milk") {
		t.Fatalf("expected 'buy milk' in output, got: %q", out)
	}
	if !(strings.Contains(out, "milkshake") || strings.Contains(out, "Important facts")) {
		t.Fatalf("expected either 'milkshake' or 'Important facts' in output, got: %q", out)
	}
}

func TestAgentCLI_ModelFlag(t *testing.T) {
	// set HOME to a temp dir so onboard writes to temp
	tmp := t.TempDir()
	os.Setenv("HOME", tmp)
	if _, _, err := config.Onboard(); err != nil {
		t.Fatalf("onboard failed: %v", err)
	}
	// remove OpenAI from config so stub provider is used
	cfgPath, _, _ := config.ResolveDefaultPaths()
	cfg2, _ := config.LoadConfig()
	cfg2.Providers.OpenAI = nil
	_ = config.SaveConfig(cfg2, cfgPath)

	cmd := NewRootCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"agent", "--model", "stub-model", "-m", "hello"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("agent failed: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "(stub) Echo") {
		t.Fatalf("expected stub echo output, got: %q", out)
	}
}

func TestSignalCronJobToGatewayDispatchesLiveSchedulerJob(t *testing.T) {
	workspace := shortTempDir(t)

	dispatched := make(chan cron.Job, 1)
	scheduler := cron.NewScheduler(nil)
	scheduler.Add("slack-daily-brief", "slack-daily-brief", time.Hour, "telegram", "1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := startCronSignalServer(ctx, workspace, scheduler, func(job cron.Job) {
		dispatched <- job
	}); err != nil {
		t.Fatalf("startCronSignalServer failed: %v", err)
	}

	if err := signalCronJobToGateway(workspace, "slack-daily-brief"); err != nil {
		t.Fatalf("signalCronJobToGateway failed: %v", err)
	}

	select {
	case job := <-dispatched:
		if job.Name != "slack-daily-brief" {
			t.Fatalf("dispatched job %q, want slack-daily-brief", job.Name)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for dispatched cron job")
	}
}

func TestSignalCronCommandUsesRunningGatewayState(t *testing.T) {
	tmp := shortTempDir(t)
	os.Setenv("HOME", tmp)
	if _, _, err := config.Onboard(); err != nil {
		t.Fatalf("onboard failed: %v", err)
	}

	cfg, _ := config.LoadConfig()
	ws := cfg.Agents.Defaults.Workspace
	dispatched := make(chan cron.Job, 1)
	scheduler := cron.NewScheduler(nil)
	scheduler.Add("discord-daily-brief", "discord-daily-brief", time.Hour, "telegram", "1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := startCronSignalServer(ctx, ws, scheduler, func(job cron.Job) {
		dispatched <- job
	}); err != nil {
		t.Fatalf("startCronSignalServer failed: %v", err)
	}

	cmd := NewRootCmd()
	outBuf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	cmd.SetOut(outBuf)
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{"signal-cron", "discord-daily-brief"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("signal-cron returned cobra error: %v", err)
	}

	if errBuf.String() != "" {
		t.Fatalf("expected no stderr, got %q", errBuf.String())
	}
	if !strings.Contains(outBuf.String(), "signaled gateway to trigger cron job \"discord-daily-brief\"") {
		t.Fatalf("unexpected stdout: %q", outBuf.String())
	}

	select {
	case job := <-dispatched:
		if job.Name != "discord-daily-brief" {
			t.Fatalf("dispatched job %q, want discord-daily-brief", job.Name)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for dispatched cron job")
	}
}

func TestSignalCronCommandReportsMissingLiveJob(t *testing.T) {
	tmp := shortTempDir(t)
	os.Setenv("HOME", tmp)
	if _, _, err := config.Onboard(); err != nil {
		t.Fatalf("onboard failed: %v", err)
	}

	cfg, _ := config.LoadConfig()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := startCronSignalServer(ctx, cfg.Agents.Defaults.Workspace, cron.NewScheduler(nil), func(job cron.Job) {}); err != nil {
		t.Fatalf("startCronSignalServer failed: %v", err)
	}

	cmd := NewRootCmd()
	errBuf := &bytes.Buffer{}
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{"signal-cron", "slack-daily-brief"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("signal-cron returned cobra error: %v", err)
	}

	if !strings.Contains(errBuf.String(), "job \"slack-daily-brief\" not found in running gateway") {
		t.Fatalf("expected missing live job error, got %q", errBuf.String())
	}
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pico-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("remove temp dir %s: %v", dir, err)
		}
	})
	return dir
}
