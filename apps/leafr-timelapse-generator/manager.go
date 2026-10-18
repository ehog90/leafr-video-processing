package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// runFunc executes one job. It must honour ctx cancellation and clean up after itself.
type runFunc func(ctx context.Context, c *jobCtl) (*Result, error)

// Event types sent to WebSocket clients. Every event except "snapshot" carries the full job.
const (
	EventSnapshot = "snapshot" // once, right after connecting: the current jobs in "jobs"
	EventQueued   = "queued"   // a conversion was accepted
	EventStarted  = "started"  // a conversion left the queue and began (status "reading")
	EventProgress = "progress" // status stage or rounded percentage changed
	EventFinished = "finished" // conversion ended: completed, failed or cancelled
)

// Event is what WebSocket clients receive.
type Event struct {
	Type string `json:"type"`
	Job  *Job   `json:"job,omitempty"`
	Jobs []Job  `json:"jobs,omitempty"`
	// Active is set on snapshots while a conversion is queued or running; it is the
	// conversion that blocks new requests.
	Active *Job `json:"active,omitempty"`
}

// Subscriber receives job events. Its channel is closed when the subscriber is dropped,
// either because it did not keep up or because the manager is shutting down.
type Subscriber struct {
	C      chan Event
	jobID  string // empty = all jobs
	closed bool
}

type jobEntry struct {
	job    Job
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{} // closed once the job reached a terminal state

	loggedStatus string // last status written to the log
	loggedStep   int    // last progress step (10 % each) written to the log
}

// Manager owns the job store, the worker and the event fan-out. Only one conversion may
// be active (queued or running) at a time; Submit refuses new ones until it has finished.
type Manager struct {
	cfg *Config
	run runFunc

	baseCtx    context.Context
	baseCancel context.CancelFunc
	wg         sync.WaitGroup
	queue      chan *jobEntry

	mu     sync.Mutex
	jobs   map[string]*jobEntry
	subs   map[*Subscriber]struct{}
	closed bool
}

func newManager(cfg *Config, run runFunc) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg: cfg, run: run,
		baseCtx: ctx, baseCancel: cancel,
		queue: make(chan *jobEntry, 1),
		jobs:  map[string]*jobEntry{},
		subs:  map[*Subscriber]struct{}{},
	}
	m.wg.Add(2)
	go m.worker()
	go m.pruneLoop()
	return m
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Submit accepts a new job, unless a conversion is already queued or running.
func (m *Manager) Submit(spec *Spec) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Job{}, errf(503, "shutting_down", "service is shutting down")
	}
	for _, e := range m.jobs {
		if !isTerminal(e.job.Status) {
			return Job{}, errf(409, "conversion_in_progress",
				"timelapse %s is still %s; wait for it to finish or cancel it first", e.job.ID, e.job.Status)
		}
	}

	outs := []string{spec.OutputFile}
	if spec.Thumbnail != nil {
		outs = append(outs, spec.Thumbnail.OutputFile)
	}
	for _, o := range outs {
		if st, err := os.Lstat(o); err == nil {
			if st.IsDir() {
				return Job{}, errf(400, "invalid_path", "%s is a directory", o)
			}
			if !spec.Overwrite {
				return Job{}, errf(409, "output_exists", "%s already exists (set overwrite to replace it)", o)
			}
		}
	}

	ctx, cancel := context.WithCancel(m.baseCtx)
	e := &jobEntry{
		job: Job{
			ID: newID(), Status: StatusQueued, ProgressIndeterminate: true,
			Request: *spec, CreatedAt: time.Now().UTC(),
		},
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
	}
	m.queue <- e // never blocks: at most one active job exists and the buffer holds one
	m.jobs[e.job.ID] = e
	m.emitLocked(EventQueued, e)
	return e.job, nil
}

// Get returns one job.
func (m *Manager) Get(id string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.jobs[id]
	if !ok {
		return Job{}, false
	}
	return e.job, true
}

// List returns all retained jobs, newest first, optionally filtered by status.
func (m *Manager) List(status string) []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listLocked(status)
}

func (m *Manager) listLocked(status string) []Job {
	out := make([]Job, 0, len(m.jobs))
	for _, e := range m.jobs {
		if status == "" || e.job.Status == status {
			out = append(out, e.job)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Cancel stops a queued or running job and waits briefly for it to wind down. Cancelling a
// job that is already cancelled succeeds; cancelling a completed or failed job is a conflict.
func (m *Manager) Cancel(id string) (Job, error) {
	m.mu.Lock()
	e, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return Job{}, errf(404, "not_found", "timelapse %s not found", id)
	}
	switch {
	case e.job.Status == StatusCancelled:
		j := e.job
		m.mu.Unlock()
		return j, nil
	case isTerminal(e.job.Status):
		m.mu.Unlock()
		return Job{}, errf(409, "already_finished", "timelapse %s already %s", id, e.job.Status)
	case e.job.Status == StatusQueued:
		m.finishLocked(e, StatusCancelled, nil, nil)
	}
	e.cancel()
	m.mu.Unlock()

	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
	}
	j, _ := m.Get(id)
	return j, nil
}

// Subscribe registers a subscriber and returns the snapshot event to send first: the
// current state of the jobs it follows, plus the active conversion if there is one. Both are
// taken atomically with the registration, so no update is missed in between.
func (m *Manager) Subscribe(jobID string) (*Subscriber, Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, Event{}, errf(503, "shutting_down", "service is shutting down")
	}
	snap := Event{Type: EventSnapshot}
	if jobID != "" {
		e, ok := m.jobs[jobID]
		if !ok {
			return nil, Event{}, errf(404, "not_found", "timelapse %s not found", jobID)
		}
		snap.Jobs = []Job{e.job}
	} else {
		snap.Jobs = m.listLocked("")
	}
	for _, e := range m.jobs {
		if !isTerminal(e.job.Status) {
			j := e.job
			snap.Active = &j
		}
	}
	s := &Subscriber{C: make(chan Event, 256), jobID: jobID}
	m.subs[s] = struct{}{}
	return s, snap, nil
}

func (m *Manager) Unsubscribe(s *Subscriber) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropLocked(s)
}

// Subscribers returns the number of connected WebSocket clients.
func (m *Manager) Subscribers() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.subs)
}

func (m *Manager) dropLocked(s *Subscriber) {
	if s.closed {
		return
	}
	s.closed = true
	delete(m.subs, s)
	close(s.C)
}

func (m *Manager) emitLocked(typ string, e *jobEntry) {
	logEvent(typ, e)
	j := e.job
	ev := Event{Type: typ, Job: &j}
	for s := range m.subs {
		if s.jobID != "" && s.jobID != j.ID {
			continue
		}
		select {
		case s.C <- ev:
		default:
			log.Printf("🐌 dropping a WebSocket client that cannot keep up (%d events queued)", cap(s.C))
			m.dropLocked(s)
		}
	}
}

// logEvent writes the lifecycle of a job to the log: every state change, and progress
// every 10 %.
func logEvent(typ string, e *jobEntry) {
	j := &e.job
	switch typ {
	case EventQueued:
		r := j.Request
		log.Printf("📥 timelapse %s queued: %s -> %s (%s, %d fps, fit %dx%d%s)", j.ID, r.SequenceFile, r.OutputFile,
			r.Codec, r.FPS, r.Width, r.Height, thumbSummary(r.Thumbnail))
	case EventStarted:
		log.Printf("🚀 timelapse %s started", j.ID)
		e.loggedStatus = j.Status
	case EventProgress:
		if j.Status != e.loggedStatus {
			e.loggedStatus = j.Status
			e.loggedStep = int(j.ProgressPercent / 10)
			log.Printf("%s timelapse %s %s", stageEmoji(j.Status), j.ID, j.Status)
			return
		}
		if step := int(j.ProgressPercent / 10); step > e.loggedStep {
			e.loggedStep = step
			log.Printf("⏳ timelapse %s %s %.0f%%", j.ID, j.Status, j.ProgressPercent)
		}
	case EventFinished:
		took := time.Duration(0)
		if j.StartedAt != nil && j.FinishedAt != nil {
			took = j.FinishedAt.Sub(*j.StartedAt).Round(100 * time.Millisecond)
		}
		switch j.Status {
		case StatusCompleted:
			r := j.Result
			if r == nil {
				r = &Result{}
			}
			log.Printf("✅ timelapse %s completed in %s: %.1fs video, %d pictures (%d missing), %s%s",
				j.ID, took, r.DurationSec, r.FramesUsed, r.MissingFrames, humanSize(r.SizeBytes), thumbResult(r))
		case StatusFailed:
			log.Printf("❌ timelapse %s failed after %s [%s]: %s", j.ID, took, j.Error.Code, j.Error.Message)
		default:
			if j.Error != nil {
				log.Printf("🛑 timelapse %s cancelled after %s [%s]", j.ID, took, j.Error.Code)
			} else {
				log.Printf("🛑 timelapse %s cancelled after %s", j.ID, took)
			}
		}
	}
}

func stageEmoji(status string) string {
	switch status {
	case StatusReading:
		return "📖"
	case StatusPreparing:
		return "📋"
	case StatusEncoding:
		return "🎬"
	case StatusThumbnail:
		return "🖼️"
	case StatusSaving:
		return "💾"
	}
	return "ℹ️"
}

func thumbSummary(t *ThumbSpec) string {
	if t == nil {
		return ""
	}
	return fmt.Sprintf(", thumbnail %s max %dpx", t.Format, t.Width)
}

func thumbResult(r *Result) string {
	if r.ThumbnailFile == "" {
		return ""
	}
	return ", thumbnail " + humanSize(r.ThumbnailSizeBytes)
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// finishLocked moves a job into a terminal state exactly once.
func (m *Manager) finishLocked(e *jobEntry, status string, res *Result, jerr *JobError) {
	if isTerminal(e.job.Status) {
		return
	}
	now := time.Now().UTC()
	e.job.Status = status
	e.job.ProgressIndeterminate = false
	e.job.FinishedAt = &now
	e.job.Result = res
	e.job.Error = jerr
	if status == StatusCompleted {
		e.job.ProgressPercent = 100
	}
	close(e.done)
	e.cancel()
	m.emitLocked(EventFinished, e)
}

func (m *Manager) worker() {
	defer m.wg.Done()
	for {
		select {
		case <-m.baseCtx.Done():
			return
		case e := <-m.queue:
			m.execute(e)
		}
	}
}

func (m *Manager) execute(e *jobEntry) {
	m.mu.Lock()
	if e.job.Status != StatusQueued {
		m.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	e.job.Status = StatusReading
	e.job.StartedAt = &now
	m.emitLocked(EventStarted, e)
	spec := e.job.Request
	m.mu.Unlock()

	res, err := m.safeRun(e, &spec)
	logMemoryStats(e.job.ID, "before release")
	debug.FreeOSMemory()
	logMemoryStats(e.job.ID, "after release")

	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case err == nil:
		m.finishLocked(e, StatusCompleted, res, nil)
	case e.ctx.Err() != nil || errors.Is(err, context.Canceled):
		m.finishLocked(e, StatusCancelled, nil, m.cancelReasonLocked())
	default:
		m.finishLocked(e, StatusFailed, nil, asJobError(err))
	}
}

func logMemoryStats(jobID, phase string) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	rss, err := processRSS()
	if err != nil {
		log.Printf("🧠 timelapse %s memory %s: process RSS unavailable (%v); Go heap alloc=%s in-use=%s sys=%s released=%s",
			jobID, phase, err, humanSize(int64(mem.HeapAlloc)), humanSize(int64(mem.HeapInuse)),
			humanSize(int64(mem.HeapSys)), humanSize(int64(mem.HeapReleased)))
		return
	}
	log.Printf("🧠 timelapse %s memory %s: process RSS=%s; Go heap alloc=%s in-use=%s sys=%s released=%s",
		jobID, phase, humanSize(int64(rss)), humanSize(int64(mem.HeapAlloc)), humanSize(int64(mem.HeapInuse)),
		humanSize(int64(mem.HeapSys)), humanSize(int64(mem.HeapReleased)))
}

func processRSS() (uint64, error) {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(status), "\n") {
		if value, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			fields := strings.Fields(value)
			if len(fields) < 2 || fields[1] != "kB" {
				return 0, fmt.Errorf("unexpected VmRSS format")
			}
			kb, err := strconv.ParseUint(fields[0], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse VmRSS: %w", err)
			}
			return kb * 1024, nil
		}
	}
	return 0, fmt.Errorf("VmRSS not found in /proc/self/status")
}

func (m *Manager) safeRun(e *jobEntry, spec *Spec) (res *Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("💥 timelapse %s panicked: %v", e.job.ID, r)
			err = fmt.Errorf("internal error")
		}
	}()
	return m.run(e.ctx, &jobCtl{m: m, e: e, Spec: spec})
}

// pruneLoop forgets finished jobs after JobRetention.
func (m *Manager) pruneLoop() {
	defer m.wg.Done()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-m.baseCtx.Done():
			return
		case <-t.C:
			m.prune(time.Now())
		}
	}
}

func (m *Manager) prune(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, e := range m.jobs {
		if e.job.FinishedAt != nil && now.Sub(*e.job.FinishedAt) > m.cfg.JobRetention {
			delete(m.jobs, id)
		}
	}
}

// Close cancels every job, waits for the workers and disconnects all subscribers.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()

	m.baseCancel()
	m.wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.jobs {
		m.finishLocked(e, StatusCancelled, nil, m.cancelReasonLocked())
	}
	for s := range m.subs {
		m.dropLocked(s)
	}
}

// cancelReasonLocked returns the error attached to a cancelled job: nil when a client
// cancelled it, "service_shutdown" when the service stopped.
func (m *Manager) cancelReasonLocked() *JobError {
	if m.baseCtx.Err() != nil {
		return &JobError{Code: "service_shutdown", Message: "the service was shut down before the conversion finished"}
	}
	return nil
}

// jobCtl is the handle a runFunc uses to report its status and progress.
type jobCtl struct {
	m *Manager
	e *jobEntry
	// Spec is a private copy of the job's validated request.
	Spec *Spec
}

// Logf writes a log line prefixed with the job id.
func (c *jobCtl) Logf(format string, a ...any) {
	log.Printf("%s (timelapse %s)", fmt.Sprintf(format, a...), c.e.job.ID)
}

// SetStatus moves the job into a new stage; progress is indeterminate until it is set.
func (c *jobCtl) SetStatus(status string, indeterminate bool) {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	if isTerminal(c.e.job.Status) {
		return
	}
	c.e.job.Status = status
	c.e.job.ProgressIndeterminate = indeterminate
	c.m.emitLocked(EventProgress, c.e)
}

// SetProgress reports overall progress in percent. It is rounded to 0.1, never goes
// backwards and stays below 100 until the job is completed.
func (c *jobCtl) SetProgress(pct float64) {
	pct = math.Round(math.Min(math.Max(pct, 0), 99.9)*10) / 10
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	j := &c.e.job
	if isTerminal(j.Status) || (pct == j.ProgressPercent && !j.ProgressIndeterminate) || pct < j.ProgressPercent {
		return
	}
	j.ProgressPercent = pct
	j.ProgressIndeterminate = false
	c.m.emitLocked(EventProgress, c.e)
}
