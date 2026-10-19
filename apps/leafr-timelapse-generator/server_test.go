package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/redis/go-redis/v9"
)

func testServer(t *testing.T, run runFunc, mutate func(*Config)) (*server, *httptest.Server, *mediaDir) {
	t.Helper()
	return testServerOn(t, miniredis.RunT(t), testMedia(t), run, mutate)
}

// testServerOn starts a replica on the given Redis and media directory, so several of them can share both.
func testServerOn(t *testing.T, mr *miniredis.Miniredis, media *mediaDir, run runFunc, mutate func(*Config)) (*server, *httptest.Server, *mediaDir) {
	t.Helper()
	cfg := &Config{MediaDir: media.root, TmpDir: t.TempDir(),
		ThumbFormat: "webp", ThumbWidth: 600, JobRetention: time.Hour,
		RedisKeyPrefix: "test", ClaimIdle: 5 * time.Second}
	if mutate != nil {
		mutate(cfg)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mgr, err := newManager(cfg, run, rdb)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{cfg: cfg, media: media, mgr: mgr}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(func() { ts.Close(); mgr.Close(); rdb.Close() })
	return s, ts, media
}

func postJob(t *testing.T, ts *httptest.Server, media *mediaDir, out string) (*http.Response, Job) {
	t.Helper()
	seq := filepath.Join(media.root, "seq.imageseq")
	touch(t, seq)
	body := `{"sequenceFile":"seq.imageseq","outputFile":"` + out + `","codec":"H264","width":640,"height":480}`
	resp, err := http.Post(ts.URL+"/api/timelapses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	t.Logf("POST %s -> %d %s", out, resp.StatusCode, raw)
	var j Job
	json.Unmarshal(raw, &j)
	return resp, j
}

func waitStatus(t *testing.T, s *server, id string, want string) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j, _, _ := s.mgr.Get(id); j.Status == want {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, _, _ := s.mgr.Get(id)
	t.Fatalf("job %s: status %q, want %q", id, j.Status, want)
	return j
}

func TestJobCompletesAndReportsProgressOverWebSocket(t *testing.T) {
	release := make(chan struct{})
	run := func(ctx context.Context, c *jobCtl) (*Result, error) {
		c.SetStatus(StatusEncoding, false)
		c.SetProgress(25)
		<-release
		c.SetProgress(75)
		return &Result{OutputFile: c.Spec.OutputFile}, nil
	}
	s, ts, media := testServer(t, run, nil)

	resp, job := postJob(t, ts, media, "out/a.mp4")
	if resp.StatusCode != http.StatusAccepted || job.ID == "" || resp.Header.Get("Location") != "/api/timelapses/"+job.ID {
		t.Fatalf("create: %d %+v", resp.StatusCode, job)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/ws?jobId="+job.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()

	var snap Event
	if err := wsjson.Read(ctx, c, &snap); err != nil || snap.Type != EventSnapshot || len(snap.Jobs) != 1 || snap.Active == nil || snap.Active.ID != job.ID {
		t.Fatalf("snapshot: %+v %v", snap, err)
	}
	waitStatus(t, s, job.ID, StatusEncoding)
	close(release)

	var last Job
	var percents []float64
	for last.Status != StatusCompleted {
		var ev Event
		if err := wsjson.Read(ctx, c, &ev); err != nil {
			t.Fatalf("read: %v (seen %v)", err, percents)
		}
		if (isTerminal(ev.Job.Status)) != (ev.Type == EventFinished) || ev.Job.ID != job.ID {
			t.Fatalf("unexpected event %+v", ev)
		}
		last = *ev.Job
		percents = append(percents, last.ProgressPercent)
	}
	if last.ProgressPercent != 100 || last.Result == nil || last.FinishedAt == nil {
		t.Fatalf("final job: %+v", last)
	}
	for i := 1; i < len(percents); i++ {
		if percents[i] < percents[i-1] {
			t.Fatalf("progress went backwards: %v", percents)
		}
	}
}

// One WebSocket carries the whole lifecycle of every conversion, and a reconnect starts
// with the current state.
func TestSingleWebSocketLifecycleAndReconnect(t *testing.T) {
	release := make(chan struct{})
	run := func(ctx context.Context, c *jobCtl) (*Result, error) {
		c.SetStatus(StatusEncoding, false)
		c.SetProgress(40)
		select {
		case <-release:
			return &Result{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s, ts, media := testServer(t, run, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"

	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	var snap Event
	if err := wsjson.Read(ctx, c, &snap); err != nil || snap.Type != EventSnapshot || snap.Active != nil || len(snap.Jobs) != 0 {
		t.Fatalf("empty snapshot: %+v %v", snap, err)
	}

	_, job := postJob(t, ts, media, "a.mp4")
	var types []string
	for len(types) < 3 {
		var ev Event
		if err := wsjson.Read(ctx, c, &ev); err != nil {
			t.Fatalf("read: %v (seen %v)", err, types)
		}
		types = append(types, ev.Type)
	}
	if types[0] != EventQueued || types[1] != EventStarted || types[2] != EventProgress {
		t.Fatalf("event order: %v", types)
	}

	// A client that reconnects mid-conversion gets the current state first.
	c2, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.CloseNow()
	var snap2 Event
	if err := wsjson.Read(ctx, c2, &snap2); err != nil || snap2.Type != EventSnapshot || snap2.Active == nil ||
		snap2.Active.ID != job.ID || snap2.Active.Status != StatusEncoding || snap2.Active.ProgressPercent != 40 {
		t.Fatalf("reconnect snapshot: %+v %v", snap2, err)
	}

	close(release)
	waitStatus(t, s, job.ID, StatusCompleted)
	for _, conn := range []*websocket.Conn{c, c2} {
		for {
			var ev Event
			if err := wsjson.Read(ctx, conn, &ev); err != nil {
				t.Fatal(err)
			}
			if ev.Type == EventFinished {
				if ev.Job.Status != StatusCompleted {
					t.Fatalf("finished: %+v", ev.Job)
				}
				break
			}
		}
	}

	// After the conversion the snapshot of a new connection has no active job.
	c3, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c3.CloseNow()
	var snap3 Event
	if err := wsjson.Read(ctx, c3, &snap3); err != nil || snap3.Active != nil || len(snap3.Jobs) != 1 || snap3.Jobs[0].Status != StatusCompleted {
		t.Fatalf("final snapshot: %+v %v", snap3, err)
	}
}

func TestCancelRunningJob(t *testing.T) {
	started := make(chan struct{})
	run := func(ctx context.Context, c *jobCtl) (*Result, error) {
		c.SetStatus(StatusEncoding, false)
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	_, ts, media := testServer(t, run, nil)
	_, job := postJob(t, ts, media, "a.mp4")
	<-started

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/timelapses/"+job.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var got Job
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || got.Status != StatusCancelled || got.Error != nil {
		t.Fatalf("cancel: %d %+v", resp.StatusCode, got)
	}

	// Cancelling again is idempotent.
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second cancel: %d", resp.StatusCode)
	}
}

func TestJobsQueueBehindTheRunningOne(t *testing.T) {
	block := make(chan struct{})
	run := func(ctx context.Context, c *jobCtl) (*Result, error) {
		select {
		case <-block:
			return &Result{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s, ts, media := testServer(t, run, nil)

	_, first := postJob(t, ts, media, "1.mp4")
	waitStatus(t, s, first.ID, StatusReading)
	resp, second := postJob(t, ts, media, "2.mp4")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("second conversion: %d", resp.StatusCode)
	}
	if j, _, _ := s.mgr.Get(second.ID); j.Status != StatusQueued {
		t.Fatalf("second job: %s", j.Status)
	}
	if n, _ := s.mgr.List(""); len(n) != 2 {
		t.Fatalf("%d jobs stored", len(n))
	}
	if resp, _ := postJob(t, ts, media, "2.mp4"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("same output as a queued job: %d", resp.StatusCode)
	}

	sub, snap, err := s.mgr.Subscribe("")
	if err != nil {
		t.Fatal(err)
	}
	s.mgr.Unsubscribe(sub)
	if len(snap.ActiveJobs) != 2 || snap.Active == nil || snap.Active.ID != first.ID || snap.ActiveJobs[1].ID != second.ID {
		t.Fatalf("snapshot active jobs: %+v", snap)
	}

	// A queued job can be cancelled; it is skipped when its turn comes.
	third := func() Job { _, j := postJob(t, ts, media, "3.mp4"); return j }()
	if j, err := s.mgr.Cancel(third.ID); err != nil || j.Status != StatusCancelled {
		t.Fatalf("cancel queued: %+v %v", j, err)
	}
	if _, err := s.mgr.Cancel(first.ID); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, s, first.ID, StatusCancelled)
	waitStatus(t, s, second.ID, StatusReading)
	close(block)
	waitStatus(t, s, second.ID, StatusCompleted)
	if j, _, _ := s.mgr.Get(third.ID); j.Status != StatusCancelled || j.StartedAt != nil {
		t.Fatalf("cancelled queued job: %+v", j)
	}
	if _, err := s.mgr.Cancel(second.ID); err == nil {
		t.Fatal("cancelling a completed job should fail")
	}
}

// Replicas share the queue, the job state and the events: a job is run by one of them, and
// every replica reports it and can cancel it.
func TestReplicasShareQueueStateAndEvents(t *testing.T) {
	mr := miniredis.RunT(t)
	media := testMedia(t)
	started := make(chan [2]string, 4)
	block := make(chan struct{})
	mk := func(name string) runFunc {
		return func(ctx context.Context, c *jobCtl) (*Result, error) {
			started <- [2]string{name, c.e.job.ID}
			c.SetProgress(30)
			select {
			case <-block:
				return &Result{}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	replicas := map[string]*server{}
	var tsA *httptest.Server
	replicas["a"], tsA, _ = testServerOn(t, mr, media, mk("a"), nil)
	var tsB *httptest.Server
	replicas["b"], tsB, _ = testServerOn(t, mr, media, mk("b"), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsB, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(tsB.URL, "http")+"/api/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer wsB.CloseNow()
	var snap Event
	if err := wsjson.Read(ctx, wsB, &snap); err != nil {
		t.Fatal(err)
	}

	// Two jobs submitted to replica A are run by both replicas at the same time.
	_, j1 := postJob(t, tsA, media, "1.mp4")
	_, j2 := postJob(t, tsA, media, "2.mp4")
	runner := map[string]string{} // job id -> replica
	for range 2 {
		s := <-started
		runner[s[1]] = s[0]
	}
	if len(runner) != 2 || runner[j1.ID] == runner[j2.ID] {
		t.Fatalf("jobs ran on %v, want one per replica", runner)
	}

	// Replica B's WebSocket sees the events of the job that runs on replica A, too.
	seen := map[string]bool{}
	for len(seen) < 2 {
		var ev Event
		if err := wsjson.Read(ctx, wsB, &ev); err != nil {
			t.Fatalf("read: %v", err)
		}
		if ev.Type == EventProgress && ev.Job.ProgressPercent == 30 {
			seen[ev.Job.ID] = true
		}
	}

	// Cancelling through the replica that does not run the job works.
	other := replicas["a"]
	if runner[j1.ID] == "a" {
		other = replicas["b"]
	}
	if j, err := other.mgr.Cancel(j1.ID); err != nil || j.Status != StatusCancelled {
		t.Fatalf("cancel: %+v %v", j, err)
	}
	close(block)
	waitStatus(t, other, j2.ID, StatusCompleted)
}

// A job whose replica died is taken over: a queued entry is run, a running one is failed.
func TestAbandonedJobsAreClaimed(t *testing.T) {
	mr := miniredis.RunT(t)
	media := testMedia(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	cfg := &Config{RedisKeyPrefix: "test", JobRetention: time.Hour}
	st := newStore(rdb, cfg)
	ctx := context.Background()
	if err := st.ensureGroup(ctx); err != nil {
		t.Fatal(err)
	}

	spec := Spec{SequenceFile: "s", OutputFile: filepath.Join(media.root, "x.mp4")}
	queuedJob := Job{ID: "queuedjob", Status: StatusQueued, Request: spec, CreatedAt: time.Now().UTC()}
	runningJob := Job{ID: "runningjob", Status: StatusEncoding, Request: spec, CreatedAt: time.Now().UTC()}
	for _, j := range []*Job{&queuedJob, &runningJob} {
		if err := st.create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	// A replica that read both entries and then died.
	dead := &store{rdb: rdb, prefix: "test", consumer: "dead"}
	if _, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: queueGroup, Consumer: dead.consumer, Streams: []string{st.streamKey(), ">"}, Count: 2,
	}).Result(); err != nil {
		t.Fatal(err)
	}
	mr.SetTime(time.Now().Add(time.Minute))

	s, _, _ := testServerOn(t, mr, media, func(ctx context.Context, c *jobCtl) (*Result, error) {
		return &Result{}, nil
	}, nil)
	waitStatus(t, s, queuedJob.ID, StatusCompleted)
	j := waitStatus(t, s, runningJob.ID, StatusFailed)
	if j.Error == nil || j.Error.Code != "worker_lost" {
		t.Fatalf("running job of a dead replica: %+v", j)
	}
}

func TestFailedJobAndExistingOutput(t *testing.T) {
	run := func(ctx context.Context, c *jobCtl) (*Result, error) {
		return nil, failf("encode_failed", "boom")
	}
	s, ts, media := testServer(t, run, nil)
	_, job := postJob(t, ts, media, "a.mp4")
	j := waitStatus(t, s, job.ID, StatusFailed)
	if j.Error == nil || j.Error.Code != "encode_failed" || j.Error.Message != "boom" {
		t.Fatalf("error: %+v", j.Error)
	}

	touch(t, filepath.Join(media.root, "exists.mp4"))
	if resp, _ := postJob(t, ts, media, "exists.mp4"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("existing output: %d", resp.StatusCode)
	}
}

func TestCreateValidation(t *testing.T) {
	_, ts, _ := testServer(t, func(context.Context, *jobCtl) (*Result, error) { return &Result{}, nil }, nil)
	cases := map[string]struct {
		body   string
		status int
	}{
		"bad json":      {`{`, 400},
		"unknown field": {`{"nope":1}`, 400},
		"trailing":      {`{"sequenceFile":"s"} {}`, 400},
		"no seq file":   {`{"sequenceFile":"missing.imageseq","outputFile":"a.mp4","codec":"H264","width":640,"height":480}`, 404},
		"escape":        {`{"sequenceFile":"../s","outputFile":"a.mp4","codec":"H264","width":640,"height":480}`, 400},
	}
	for name, tc := range cases {
		resp, err := http.Post(ts.URL+"/api/timelapses", "application/json", strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Errorf("%s: got %d want %d", name, resp.StatusCode, tc.status)
		}
	}
	if resp, _ := http.Get(ts.URL + "/api/timelapses/nope"); resp.StatusCode != 404 {
		t.Errorf("get unknown: %d", resp.StatusCode)
	}
	if resp, _ := http.Get(ts.URL + "/api/ws?jobId=nope"); resp.StatusCode != 404 {
		t.Errorf("ws unknown: %d", resp.StatusCode)
	}
}

func TestUnmatchedRoutesReturnJSONErrors(t *testing.T) {
	_, ts, _ := testServer(t, func(context.Context, *jobCtl) (*Result, error) { return &Result{}, nil }, nil)
	cases := []struct {
		method, path string
		status       int
		code         string
	}{
		{"GET", "/nope", 404, "not_found"},
		{"PUT", "/api/timelapses", 405, "method_not_allowed"},
		{"POST", "/api/timelapses/abc", 405, "method_not_allowed"},
		{"POST", "/healthz", 405, "method_not_allowed"},
		{"DELETE", "/api/timelapses/nope", 404, "not_found"},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest(tc.method, ts.URL+tc.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Error JobError `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != tc.status || body.Error.Code != tc.code {
			t.Errorf("%s %s: %d %q, want %d %q", tc.method, tc.path, resp.StatusCode, body.Error.Code, tc.status, tc.code)
		}
	}
}

func TestShutdownCancelsWithServiceShutdownCode(t *testing.T) {
	started := make(chan struct{})
	run := func(ctx context.Context, c *jobCtl) (*Result, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	s, ts, media := testServer(t, run, nil)
	_, job := postJob(t, ts, media, "a.mp4")
	<-started
	s.mgr.Close()
	j, _, _ := s.mgr.Get(job.ID)
	if j.Status != StatusCancelled || j.Error == nil || j.Error.Code != "service_shutdown" {
		t.Fatalf("job after shutdown: %+v", j)
	}
	if _, err := s.mgr.Submit(&j.Request); err == nil {
		t.Fatal("submit after shutdown should fail")
	}
}

func TestRetention(t *testing.T) {
	run := func(ctx context.Context, c *jobCtl) (*Result, error) { return &Result{}, nil }
	mr := miniredis.RunT(t)
	s, ts, media := testServerOn(t, mr, testMedia(t), run, func(c *Config) { c.JobRetention = time.Minute })
	_, job := postJob(t, ts, media, "a.mp4")
	waitStatus(t, s, job.ID, StatusCompleted)
	mr.FastForward(30 * time.Second)
	if _, ok, _ := s.mgr.Get(job.ID); !ok {
		t.Fatal("expired too early")
	}
	mr.FastForward(2 * time.Minute)
	if _, ok, _ := s.mgr.Get(job.ID); ok {
		t.Fatal("not expired")
	}
	if jobs, _ := s.mgr.List(""); len(jobs) != 0 {
		t.Fatalf("expired job still listed: %+v", jobs)
	}
}
