package cron

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	cronlib "github.com/robfig/cron/v3"
)

// Job represents a scheduled task.
type Job struct {
	ID        string
	Name      string
	Message   string
	FireAt    time.Time
	Channel   string // originating channel (e.g., "telegram")
	ChatID    string // originating chat ID
	Recurring bool   // if true, re-schedule after firing
	Interval  time.Duration
	CronExpr  string // if set, use cron expression for scheduling instead of fixed interval
	fired     bool
}

// persistedJob is the JSON-serializable form of Job.
type persistedJob struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Message   string `json:"message"`
	FireAt    string `json:"fireAt"`
	Channel   string `json:"channel"`
	ChatID    string `json:"chatID"`
	Recurring bool   `json:"recurring,omitempty"`
	IntervalS string `json:"interval,omitempty"` // Go duration string
	CronExpr  string `json:"cronExpr,omitempty"`
}

// FireCallback is called when a job fires. The scheduler passes the job details.
type FireCallback func(job Job)

// Scheduler manages scheduled jobs and fires them when due.
// If persistPath is set, jobs are saved to disk after every mutation.
type Scheduler struct {
	mu          sync.Mutex
	jobs        map[string]*Job
	callback    FireCallback
	nextID      int
	running     bool
	persistPath string // empty = no persistence
}

// NewScheduler creates a new scheduler with the given fire callback (no persistence).
func NewScheduler(callback FireCallback) *Scheduler {
	return &Scheduler{
		jobs:     make(map[string]*Job),
		callback: callback,
	}
}

// NewSchedulerWithPersistence creates a scheduler that saves/loads jobs to a JSON file.
func NewSchedulerWithPersistence(callback FireCallback, path string) *Scheduler {
	s := &Scheduler{
		jobs:        make(map[string]*Job),
		callback:    callback,
		persistPath: path,
	}
	if err := s.load(); err != nil {
		log.Printf("cron: could not load persisted jobs from %s: %v", path, err)
	} else {
		count := len(s.jobs)
		if count > 0 {
			log.Printf("cron: loaded %d persisted job(s) from %s", count, path)
		}
	}
	return s
}

// Add schedules a new job. Returns the job ID.
func (s *Scheduler) Add(name, message string, delay time.Duration, channel, chatID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := fmt.Sprintf("job-%d", s.nextID)
	s.jobs[id] = &Job{
		ID:      id,
		Name:    name,
		Message: message,
		FireAt:  time.Now().Add(delay),
		Channel: channel,
		ChatID:  chatID,
	}
	log.Printf("cron: scheduled job %q (%s) to fire in %v", name, id, delay)
	s.saveLocked()
	return id
}

// AddRecurring schedules a recurring job. Returns the job ID.
func (s *Scheduler) AddRecurring(name, message string, interval time.Duration, channel, chatID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := fmt.Sprintf("job-%d", s.nextID)
	s.jobs[id] = &Job{
		ID:        id,
		Name:      name,
		Message:   message,
		FireAt:    time.Now().Add(interval),
		Channel:   channel,
		ChatID:    chatID,
		Recurring: true,
		Interval:  interval,
	}
	log.Printf("cron: scheduled recurring job %q (%s) every %v", name, id, interval)
	s.saveLocked()
	return id
}

// cronParser is a standard 5-field cron parser (minute hour dom month dow).
var cronParser = cronlib.NewParser(cronlib.Minute | cronlib.Hour | cronlib.Dom | cronlib.Month | cronlib.Dow)

// AddCron schedules a job using a cron expression. Returns the job ID.
func (s *Scheduler) AddCron(name, message, expr, channel, chatID string) (string, error) {
	sched, err := cronParser.Parse(expr)
	if err != nil {
		return "", fmt.Errorf("invalid cron expression %q: %v", expr, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := fmt.Sprintf("job-%d", s.nextID)
	s.jobs[id] = &Job{
		ID:        id,
		Name:      name,
		Message:   message,
		FireAt:    sched.Next(time.Now()),
		Channel:   channel,
		ChatID:    chatID,
		Recurring: true,
		CronExpr:  expr,
	}
	log.Printf("cron: scheduled cron job %q (%s) expr=%s next=%v", name, id, expr, s.jobs[id].FireAt)
	s.saveLocked()
	return id, nil
}

// Cancel removes a job by ID. Returns true if found.
func (s *Scheduler) Cancel(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[id]; ok {
		delete(s.jobs, id)
		log.Printf("cron: cancelled job %s", id)
		s.saveLocked()
		return true
	}
	return false
}

// CancelByName removes a job by name. Returns true if found.
func (s *Scheduler) CancelByName(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, j := range s.jobs {
		if j.Name == name {
			delete(s.jobs, id)
			log.Printf("cron: cancelled job %q (%s)", name, id)
			s.saveLocked()
			return true
		}
	}
	return false
}

// List returns all pending jobs.
func (s *Scheduler) List() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		result = append(result, *j)
	}
	return result
}

// Start begins the scheduler tick loop. Call in a goroutine.
func (s *Scheduler) Start(done <-chan struct{}) {
	s.running = true
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	log.Println("cron: scheduler started")
	for {
		select {
		case <-done:
			s.running = false
			log.Println("cron: scheduler stopped")
			return
		case now := <-ticker.C:
			s.tick(now)
		}
	}
}

// tick checks all jobs and fires any that are due.
func (s *Scheduler) tick(now time.Time) {
	s.mu.Lock()
	// collect jobs to fire
	var toFire []*Job
	for _, j := range s.jobs {
		if !j.fired && now.After(j.FireAt) {
			toFire = append(toFire, j)
		}
	}
	// handle fired jobs while still holding lock
	needSave := len(toFire) > 0
	for _, j := range toFire {
		if j.CronExpr != "" {
			// Reschedule using cron expression
			sched, err := cronParser.Parse(j.CronExpr)
			if err == nil {
				j.FireAt = sched.Next(now)
			} else {
				log.Printf("cron: failed to reparse cron expr for job %q: %v", j.Name, err)
				delete(s.jobs, j.ID)
			}
		} else if j.Recurring {
			j.FireAt = now.Add(j.Interval)
		} else {
			j.fired = true
			delete(s.jobs, j.ID)
		}
	}
	if needSave {
		s.saveLocked()
	}
	s.mu.Unlock()

	// fire callbacks outside lock
	for _, j := range toFire {
		log.Printf("cron: firing job %q (%s): %s", j.Name, j.ID, j.Message)
		if s.callback != nil {
			s.callback(*j)
		}
	}
}

// saveLocked persists jobs to disk. Caller must hold s.mu.
func (s *Scheduler) saveLocked() {
	if s.persistPath == "" {
		return
	}
	pjobs := make([]persistedJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		pjobs = append(pjobs, persistedJob{
			ID:        j.ID,
			Name:      j.Name,
			Message:   j.Message,
			FireAt:    j.FireAt.Format(time.RFC3339),
			Channel:   j.Channel,
			ChatID:    j.ChatID,
			Recurring: j.Recurring,
			IntervalS: j.Interval.String(),
			CronExpr:  j.CronExpr,
		})
	}
	data, err := json.MarshalIndent(pjobs, "", "  ")
	if err != nil {
		log.Printf("cron: failed to marshal jobs: %v", err)
		return
	}
	dir := filepath.Dir(s.persistPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("cron: failed to create directory %s: %v", dir, err)
		return
	}
	if err := os.WriteFile(s.persistPath, data, 0644); err != nil {
		log.Printf("cron: failed to save jobs to %s: %v", s.persistPath, err)
	}
}

// load reads persisted jobs from disk. Caller should call before Start().
func (s *Scheduler) load() error {
	if s.persistPath == "" {
		return nil
	}
	data, err := os.ReadFile(s.persistPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no file yet, that's fine
		}
		return err
	}
	if len(data) == 0 {
		return nil
	}
	var pjobs []persistedJob
	if err := json.Unmarshal(data, &pjobs); err != nil {
		return err
	}
	now := time.Now()
	for _, pj := range pjobs {
		fireAt, err := time.Parse(time.RFC3339, pj.FireAt)
		if err != nil {
			log.Printf("cron: skipping job %s with invalid fireAt: %v", pj.ID, err)
			continue
		}
		interval, _ := time.ParseDuration(pj.IntervalS)
		// Skip expired one-shot jobs
		if !pj.Recurring && pj.CronExpr == "" && now.After(fireAt) {
			log.Printf("cron: skipping expired one-shot job %q", pj.Name)
			continue
		}
		// For cron jobs, compute next fire time from expression
		if pj.CronExpr != "" {
			sched, err := cronParser.Parse(pj.CronExpr)
			if err != nil {
				log.Printf("cron: skipping job %q with invalid cron expr: %v", pj.Name, err)
				continue
			}
			if now.After(fireAt) {
				fireAt = sched.Next(now)
			}
		} else if pj.Recurring && interval > 0 {
			// For recurring jobs, advance past missed fire times
			for now.After(fireAt) {
				fireAt = fireAt.Add(interval)
			}
		}
		// Extract numeric ID for nextID counter
		idNum, _ := strconv.Atoi(strings.TrimPrefix(pj.ID, "job-"))
		if idNum > s.nextID {
			s.nextID = idNum
		}
		s.jobs[pj.ID] = &Job{
			ID:        pj.ID,
			Name:      pj.Name,
			Message:   pj.Message,
			FireAt:    fireAt,
			Channel:   pj.Channel,
			ChatID:    pj.ChatID,
			Recurring: pj.Recurring,
			Interval:  interval,
			CronExpr:  pj.CronExpr,
		}
	}
	return nil
}
