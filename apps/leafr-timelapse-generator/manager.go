package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
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
	// Active is set on snapshots while a conversion is queued or running: the oldest one.
	// ActiveJobs lists all of them, oldest first.
	Active     *Job  `json:"active,omitempty"`
	ActiveJobs []Job `json:"activeJobs,omitempty"`
}

// Subscriber receives job events. Its channel is closed when the subscriber is dropped,
// either because it did not keep up or because the manager is shutting down.
type Subscriber struct {
	C      chan Event
	jobID  string // empty = all jobs
	closed bool
}

// jobEntry is a job this replica is running. mu serialises its state changes together with
// the events that report them, so events reach Redis in order.
type jobEntry struct {
	mu     sync.Mutex
	job    Job
	ctx    context.Context
	cancel context.CancelFunc

	loggedStatus string // last status written to the log
	loggedStep   int    // last progress step (10 % each) written to the log
}

// Manager owns the job queue and the event fan-out. Job state, the queue and the events
// live in Redis, so any number of replicas can share them: every replica accepts requests and
// answers queries, runs one conversion at a time, and forwards the events of all replicas to
// its own WebSocket clients.
type Manager struct {
	cfg        *Config
	run        runFunc
	st         *store
	ps         *redis.PubSub
	workCtx    context.Context // cancelled on shutdown; stops the worker and the running job
	workCancel context.CancelFunc
	loopCtx    context.Context // outlives workCtx so the last events still get delivered
	loopCancel context.CancelFunc
	workWG     sync.WaitGroup
	loopWG     sync.WaitGroup

	mu     sync.Mutex
	local  map[string]*jobEntry // jobs this replica is running
	subs   map[*Subscriber]struct{}
	closed bool
}

func newManager(cfg *Config, run runFunc, rdb *redis.Client) (*Manager, error) {
	st := newStore(rdb, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := st.ensureGroup(ctx); err != nil {
		return nil, fmt.Errorf("create the job queue: %w", err)
	}
	ps := rdb.Subscribe(ctx, st.eventsChannel(), st.cancelChannel())
	for range 2 { // one confirmation per channel
		if _, err := ps.Receive(ctx); err != nil {
			ps.Close()
			return nil, fmt.Errorf("subscribe to job events: %w", err)
		}
	}

	m := &Manager{
		cfg: cfg, run: run, st: st, ps: ps,
		local: map[string]*jobEntry{},
		subs:  map[*Subscriber]struct{}{},
	}
	m.workCtx, m.workCancel = context.WithCancel(context.Background())
	m.loopCtx, m.loopCancel = context.WithCancel(context.Background())
	m.workWG.Add(1)
	go m.worker()
	m.loopWG.Add(2)
	go m.eventLoop(ps)
	go m.pruneLoop()
	return m, nil
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), redisOpTimeout)
}

func storeErr(err error) error {
	log.Printf("💥 Redis: %v", err)
	return errf(503, "store_unavailable", "the job store is unavailable")
}

// Submit accepts a new job and puts it in the queue; any replica may run it.
func (m *Manager) Submit(spec *Spec) (Job, error) {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return Job{}, errf(503, "shutting_down", "service is shutting down")
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

	ctx, cancel := opCtx()
	defer cancel()
	jobs, err := m.st.list(ctx)
	if err != nil {
		return Job{}, storeErr(err)
	}
	// Two jobs must not write the same partial files at the same time, possibly on different replicas.
	for _, j := range jobs {
		if isTerminal(j.Status) {
			continue
		}
		for _, o := range outs {
			if specWrites(&j.Request, o) {
				return Job{}, errf(409, "output_in_use", "%s is already the output of timelapse %s (%s)", o, j.ID, j.Status)
			}
		}
	}

	job := Job{
		ID: newID(), Status: StatusQueued, ProgressIndeterminate: true,
		Request: *spec, CreatedAt: time.Now().UTC(),
	}
	if err := m.st.create(ctx, &job); err != nil {
		return Job{}, storeErr(err)
	}
	m.emit(EventQueued, &jobEntry{job: job})
	return job, nil
}

func specWrites(s *Spec, path string) bool {
	return s.OutputFile == path || (s.Thumbnail != nil && s.Thumbnail.OutputFile == path)
}

// Get returns one job.
func (m *Manager) Get(id string) (Job, bool, error) {
	ctx, cancel := opCtx()
	defer cancel()
	j, ok, err := m.st.get(ctx, id)
	if err != nil {
		return Job{}, false, storeErr(err)
	}
	return j, ok, nil
}

// List returns all retained jobs, newest first, optionally filtered by status.
func (m *Manager) List(status string) ([]Job, error) {
	ctx, cancel := opCtx()
	defer cancel()
	jobs, err := m.st.list(ctx)
	if err != nil {
		return nil, storeErr(err)
	}
	if status == "" {
		return jobs, nil
	}
	out := jobs[:0]
	for _, j := range jobs {
		if j.Status == status {
			out = append(out, j)
		}
	}
	return out, nil
}

// Cancel stops a queued or running job, wherever it runs, and waits briefly for it to wind
// down. Cancelling a job that is already cancelled succeeds; cancelling a completed or failed
// job is a conflict.
func (m *Manager) Cancel(id string) (Job, error) {
	ctx, cancel := opCtx()
	defer cancel()
	job, ok, err := m.st.get(ctx, id)
	if err != nil {
		return Job{}, storeErr(err)
	}
	switch {
	case !ok:
		return Job{}, errf(404, "not_found", "timelapse %s not found", id)
	case job.Status == StatusCancelled:
		return job, nil
	case isTerminal(job.Status):
		return Job{}, errf(409, "already_finished", "timelapse %s already %s", id, job.Status)
	}

	if job.Status == StatusQueued {
		now := time.Now().UTC()
		c := job
		c.Status, c.ProgressIndeterminate, c.FinishedAt = StatusCancelled, false, &now
		moved, err := m.st.transition(ctx, &c, StatusQueued)
		if err != nil {
			return Job{}, storeErr(err)
		}
		if moved { // no worker had taken it yet; it is skipped when its queue entry comes up
			m.emit(EventFinished, &jobEntry{job: c})
			return c, nil
		}
	}

	if err := m.st.publishCancel(ctx, id); err != nil {
		return Job{}, storeErr(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		j, ok, err := m.Get(id)
		if err != nil {
			return Job{}, err
		}
		if !ok {
			return Job{}, errf(404, "not_found", "timelapse %s not found", id)
		}
		if isTerminal(j.Status) || time.Now().After(deadline) {
			return j, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Subscribe registers a subscriber and returns the snapshot event to send first: the
// current state of the jobs it follows, plus the active conversions. The subscriber is
// registered before the snapshot is read, so no update is missed in between (an event that
// is already part of the snapshot may arrive again).
func (m *Manager) Subscribe(jobID string) (*Subscriber, Event, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, Event{}, errf(503, "shutting_down", "service is shutting down")
	}
	s := &Subscriber{C: make(chan Event, 256), jobID: jobID}
	m.subs[s] = struct{}{}
	m.mu.Unlock()

	jobs, err := m.List("")
	if err != nil {
		m.Unsubscribe(s)
		return nil, Event{}, err
	}
	snap := Event{Type: EventSnapshot, Jobs: jobs}
	if jobID != "" {
		snap.Jobs = nil
		for _, j := range jobs {
			if j.ID == jobID {
				snap.Jobs = []Job{j}
			}
		}
		if snap.Jobs == nil {
			m.Unsubscribe(s)
			return nil, Event{}, errf(404, "not_found", "timelapse %s not found", jobID)
		}
	}
	for i := len(jobs) - 1; i >= 0; i-- { // oldest first
		if !isTerminal(jobs[i].Status) {
			snap.ActiveJobs = append(snap.ActiveJobs, jobs[i])
		}
	}
	if len(snap.ActiveJobs) > 0 {
		j := snap.ActiveJobs[0]
		snap.Active = &j
	}
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

func (m *Manager) dropAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for s := range m.subs {
		m.dropLocked(s)
	}
}

// dispatch hands an event received from Redis to the local WebSocket clients.
func (m *Manager) dispatch(ev Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for s := range m.subs {
		if s.jobID != "" && (ev.Job == nil || s.jobID != ev.Job.ID) {
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

// eventLoop receives the events and cancel requests of all replicas. When the connection to
// Redis breaks, events may have been missed, so the WebSocket clients are disconnected and
// resynchronise from a fresh snapshot when they reconnect.
func (m *Manager) eventLoop(ps *redis.PubSub) {
	defer m.loopWG.Done()
	defer ps.Close()
	for {
		msg, err := ps.ReceiveMessage(m.loopCtx)
		if err != nil {
			if m.loopCtx.Err() != nil {
				return
			}
			log.Printf("🔌 Redis event subscription interrupted: %v", err)
			m.dropAll()
			select {
			case <-m.loopCtx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		switch msg.Channel {
		case m.st.eventsChannel():
			var ev Event
			if err := json.Unmarshal([]byte(msg.Payload), &ev); err != nil {
				log.Printf("⚠️ ignoring a malformed event: %v", err)
				continue
			}
			m.dispatch(ev)
		case m.st.cancelChannel():
			m.mu.Lock()
			e := m.local[msg.Payload]
			m.mu.Unlock()
			if e != nil {
				e.cancel()
			}
		}
	}
}

// emit logs a job event, persists the job for the events that change it while it runs, and
// publishes the event to every replica. The caller holds e.mu if the job is running.
func (m *Manager) emit(typ string, e *jobEntry) {
	logEvent(typ, e)
	j := e.job
	attempts := 1
	if typ == EventFinished {
		attempts = 3
	}
	if typ == EventProgress || typ == EventFinished {
		for i := 0; i < attempts; i++ {
			ctx, cancel := opCtx()
			err := m.st.save(ctx, &j)
			cancel()
			if err == nil {
				break
			}
			log.Printf("💥 timelapse %s: could not save its state to Redis: %v", j.ID, err)
			time.Sleep(300 * time.Millisecond)
		}
	}
	ctx, cancel := opCtx()
	defer cancel()
	if err := m.st.publishEvent(ctx, Event{Type: typ, Job: &j}); err != nil {
		log.Printf("💥 timelapse %s: could not publish its %s event to Redis: %v", j.ID, typ, err)
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

// finish moves a running job into a terminal state exactly once.
func (m *Manager) finish(e *jobEntry, status string, res *Result, jerr *JobError) {
	e.mu.Lock()
	defer e.mu.Unlock()
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
	m.emit(EventFinished, e)
}

// worker takes jobs from the queue, one at a time.
func (m *Manager) worker() {
	defer m.workWG.Done()
	for m.workCtx.Err() == nil {
		q, ok, err := m.st.next(m.workCtx, m.cfg.ClaimIdle)
		if err != nil {
			if m.workCtx.Err() != nil {
				return
			}
			log.Printf("💥 Redis queue: %v", err)
			ctx, cancel := opCtx()
			m.st.ensureGroup(ctx) // Redis may have restarted without its data
			cancel()
			select {
			case <-m.workCtx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if ok {
			m.handle(q)
		}
	}
}

func (m *Manager) ack(q queued) {
	ctx, cancel := opCtx()
	defer cancel()
	if err := m.st.ack(ctx, q.msgID); err != nil {
		log.Printf("💥 timelapse %s: could not acknowledge its queue entry: %v", q.jobID, err)
	}
}

// handle runs one queue entry. The entry stays pending in Redis until the job reached a
// terminal state, so if this replica dies the entry is claimed by another one.
func (m *Manager) handle(q queued) {
	if m.workCtx.Err() != nil {
		return // left pending; another replica (or this one after a restart) takes it over
	}
	ctx, cancel := opCtx()
	job, ok, err := m.st.get(ctx, q.jobID)
	cancel()
	if err != nil {
		log.Printf("💥 timelapse %s: %v", q.jobID, err)
		return
	}
	if !ok || isTerminal(job.Status) { // cancelled while queued, or expired
		m.ack(q)
		return
	}
	if job.Status != StatusQueued {
		// The replica that was running it stopped without finishing it.
		now := time.Now().UTC()
		job.Status, job.ProgressIndeterminate, job.FinishedAt = StatusFailed, false, &now
		job.Error = &JobError{Code: "worker_lost", Message: "the replica running the conversion stopped before it finished"}
		m.emit(EventFinished, &jobEntry{job: job})
		m.ack(q)
		return
	}

	jctx, jcancel := context.WithCancel(m.workCtx)
	e := &jobEntry{job: job, ctx: jctx, cancel: jcancel}
	defer jcancel()
	m.mu.Lock()
	m.local[job.ID] = e
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.local, job.ID)
		m.mu.Unlock()
	}()

	now := time.Now().UTC()
	started := job
	started.Status, started.StartedAt = StatusReading, &now
	ctx, cancel = opCtx()
	moved, err := m.st.transition(ctx, &started, StatusQueued)
	cancel()
	if err != nil {
		log.Printf("💥 timelapse %s: could not start: %v", job.ID, err)
		return
	}
	if !moved { // cancelled in the meantime
		m.ack(q)
		return
	}
	e.mu.Lock()
	e.job = started
	m.emit(EventStarted, e)
	spec := e.job.Request
	e.mu.Unlock()

	stopBeat := make(chan struct{})
	beatDone := make(chan struct{})
	go m.heartbeat(q, stopBeat, beatDone)

	res, err := m.safeRun(e, &spec)
	logMemoryStats(job.ID, "before release")
	debug.FreeOSMemory()
	logMemoryStats(job.ID, "after release")

	switch {
	case err == nil:
		m.finish(e, StatusCompleted, res, nil)
	case e.ctx.Err() != nil || errors.Is(err, context.Canceled):
		m.finish(e, StatusCancelled, nil, m.cancelReason())
	default:
		m.finish(e, StatusFailed, nil, asJobError(err))
	}
	close(stopBeat)
	<-beatDone
	m.ack(q)
}

// heartbeat keeps the queue entry of a running job from being claimed by another replica.
func (m *Manager) heartbeat(q queued, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(max(m.cfg.ClaimIdle/3, time.Second))
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			ctx, cancel := opCtx()
			if err := m.st.touch(ctx, q.msgID); err != nil {
				log.Printf("⚠️ timelapse %s: could not renew its queue entry: %v", q.jobID, err)
			}
			cancel()
		}
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

// pruneLoop removes the index entries of jobs whose state expired.
func (m *Manager) pruneLoop() {
	defer m.loopWG.Done()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-m.loopCtx.Done():
			return
		case <-t.C:
			ctx, cancel := opCtx()
			if _, err := m.st.list(ctx); err != nil {
				log.Printf("⚠️ could not prune the job index: %v", err)
			}
			cancel()
		}
	}
}

// Close cancels the running job, waits for the worker and disconnects all subscribers.
// Jobs still waiting in the queue stay there for the other replicas.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()

	m.workCancel()
	m.workWG.Wait()
	m.loopCancel()
	m.ps.Close() // a blocked receive does not notice the cancelled context
	m.loopWG.Wait()
	m.dropAll()
}

// cancelReason returns the error attached to a cancelled job: nil when a client
// cancelled it, "service_shutdown" when the service stopped.
func (m *Manager) cancelReason() *JobError {
	if m.workCtx.Err() != nil {
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
	c.e.mu.Lock()
	defer c.e.mu.Unlock()
	if isTerminal(c.e.job.Status) {
		return
	}
	c.e.job.Status = status
	c.e.job.ProgressIndeterminate = indeterminate
	c.m.emit(EventProgress, c.e)
}

// SetProgress reports overall progress in percent. It is rounded to 0.1, never goes
// backwards and stays below 100 until the job is completed.
func (c *jobCtl) SetProgress(pct float64) {
	pct = math.Round(math.Min(math.Max(pct, 0), 99.9)*10) / 10
	c.e.mu.Lock()
	defer c.e.mu.Unlock()
	j := &c.e.job
	if isTerminal(j.Status) || (pct == j.ProgressPercent && !j.ProgressIndeterminate) || pct < j.ProgressPercent {
		return
	}
	j.ProgressPercent = pct
	j.ProgressIndeterminate = false
	c.m.emit(EventProgress, c.e)
}
