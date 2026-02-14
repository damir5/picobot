package cron

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSchedulerFiresJob(t *testing.T) {
	var mu sync.Mutex
	var fired []Job

	s := NewScheduler(func(job Job) {
		mu.Lock()
		fired = append(fired, job)
		mu.Unlock()
	})

	done := make(chan struct{})
	go s.Start(done)

	s.Add("test-reminder", "buy cheesecake", 100*time.Millisecond, "telegram", "123")

	time.Sleep(2 * time.Second)
	close(done)

	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 1 {
		t.Fatalf("expected 1 fired job, got %d", len(fired))
	}
	if fired[0].Name != "test-reminder" {
		t.Errorf("expected name 'test-reminder', got %q", fired[0].Name)
	}
	if fired[0].Message != "buy cheesecake" {
		t.Errorf("expected message 'buy cheesecake', got %q", fired[0].Message)
	}
	if fired[0].Channel != "telegram" {
		t.Errorf("expected channel 'telegram', got %q", fired[0].Channel)
	}
}

func TestSchedulerList(t *testing.T) {
	s := NewScheduler(nil)
	s.Add("job-a", "do A", 5*time.Minute, "telegram", "1")
	s.Add("job-b", "do B", 10*time.Minute, "telegram", "2")

	jobs := s.List()
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(jobs))
	}
}

func TestSchedulerCancel(t *testing.T) {
	s := NewScheduler(nil)
	s.Add("cancel-me", "msg", 5*time.Minute, "telegram", "1")

	if !s.CancelByName("cancel-me") {
		t.Error("expected CancelByName to return true")
	}
	if len(s.List()) != 0 {
		t.Error("expected 0 jobs after cancel")
	}
}

func TestSchedulerDoesNotFireCancelled(t *testing.T) {
	var mu sync.Mutex
	var fired []Job

	s := NewScheduler(func(job Job) {
		mu.Lock()
		fired = append(fired, job)
		mu.Unlock()
	})

	done := make(chan struct{})
	go s.Start(done)

	s.Add("will-cancel", "nope", 100*time.Millisecond, "telegram", "1")
	s.CancelByName("will-cancel")

	time.Sleep(300 * time.Millisecond)
	close(done)

	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 0 {
		t.Errorf("expected 0 fired jobs after cancel, got %d", len(fired))
	}
}

func TestAddCronRejectsInvalidExpression(t *testing.T) {
	s := NewScheduler(nil)
	_, err := s.AddCron("bad", "msg", "not a cron expr", "telegram", "1")
	if err == nil {
		t.Fatal("expected error for invalid cron expression")
	}
}

func TestAddCronSchedulesJob(t *testing.T) {
	s := NewScheduler(nil)
	// "every minute" — next fire should be within ~60s
	id, err := s.AddCron("minutely", "ping", "* * * * *", "telegram", "1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty job ID")
	}

	jobs := s.List()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	j := jobs[0]
	if j.CronExpr != "* * * * *" {
		t.Errorf("expected CronExpr '* * * * *', got %q", j.CronExpr)
	}
	if j.Name != "minutely" {
		t.Errorf("expected name 'minutely', got %q", j.Name)
	}
	// FireAt should be in the future and within 60s
	until := time.Until(j.FireAt)
	if until <= 0 || until > 61*time.Second {
		t.Errorf("expected FireAt within 60s from now, got %v", until)
	}
}

func TestCronJobReschedulesAfterFiring(t *testing.T) {
	var mu sync.Mutex
	var fired []Job

	s := NewScheduler(func(job Job) {
		mu.Lock()
		fired = append(fired, job)
		mu.Unlock()
	})

	// Schedule a cron job that fires every minute
	_, err := s.AddCron("every-min", "tick", "* * * * *", "telegram", "1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Simulate a tick at a time past the fire time
	s.mu.Lock()
	for _, j := range s.jobs {
		j.FireAt = time.Now().Add(-1 * time.Second) // make it overdue
	}
	s.mu.Unlock()

	s.tick(time.Now())

	mu.Lock()
	if len(fired) != 1 {
		t.Fatalf("expected 1 fired job, got %d", len(fired))
	}
	mu.Unlock()

	// Job should still exist (rescheduled) and FireAt should be in the future
	jobs := s.List()
	if len(jobs) != 1 {
		t.Fatalf("expected job to be rescheduled, got %d jobs", len(jobs))
	}
	if time.Until(jobs[0].FireAt) <= 0 {
		t.Error("expected rescheduled FireAt to be in the future")
	}
}

func TestCronJobPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")

	s := NewScheduler(nil)
	s.persistPath = path

	_, err := s.AddCron("nightly", "digest", "0 0 * * *", "telegram", "42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify file was written
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read persisted file: %v", err)
	}

	var pjobs []persistedJob
	if err := json.Unmarshal(data, &pjobs); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if len(pjobs) != 1 {
		t.Fatalf("expected 1 persisted job, got %d", len(pjobs))
	}
	if pjobs[0].CronExpr != "0 0 * * *" {
		t.Errorf("expected CronExpr '0 0 * * *', got %q", pjobs[0].CronExpr)
	}

	// Load into a new scheduler
	s2 := NewSchedulerWithPersistence(nil, path)
	jobs := s2.List()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 loaded job, got %d", len(jobs))
	}
	if jobs[0].CronExpr != "0 0 * * *" {
		t.Errorf("expected loaded CronExpr '0 0 * * *', got %q", jobs[0].CronExpr)
	}
	if jobs[0].Name != "nightly" {
		t.Errorf("expected loaded name 'nightly', got %q", jobs[0].Name)
	}
}
