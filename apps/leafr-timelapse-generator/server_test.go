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

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func testServer(t *testing.T, run runFunc, mutate func(*Config)) (*server, *httptest.Server, *mediaDir) {
	t.Helper()
	media := testMedia(t)
	cfg := &Config{MediaDir: media.root, TmpDir: t.TempDir(),
		ThumbFormat: "webp", ThumbWidth: 600, JobRetention: time.Hour}
	if mutate != nil {
		mutate(cfg)
	}
	mgr := newManager(cfg, run)
	s := &server{cfg: cfg, media: media, mgr: mgr}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(func() { ts.Close(); mgr.Close() })
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
		if j, _ := s.mgr.Get(id); j.Status == want {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, _ := s.mgr.Get(id)
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

func TestNewConversionRefusedWhileOneIsInProgress(t *testing.T) {
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
	waitStatus(t, s, first.ID, StatusPreparing)
	resp, _ := postJob(t, ts, media, "2.mp4")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second conversion: %d", resp.StatusCode)
	}
	if n := len(s.mgr.List("")); n != 1 {
		t.Fatalf("refused job was stored: %d jobs", n)
	}

	// Still refused after the first one was cancelled but only until it has finished.
	if _, err := s.mgr.Cancel(first.ID); err != nil {
		t.Fatal(err)
	}
	if j, _ := s.mgr.Get(first.ID); j.Status != StatusCancelled {
		t.Fatalf("first job: %s", j.Status)
	}
	resp, second := postJob(t, ts, media, "2.mp4")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("after cancel: %d", resp.StatusCode)
	}
	close(block)
	waitStatus(t, s, second.ID, StatusCompleted)
	resp, third := postJob(t, ts, media, "3.mp4")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("after completion: %d", resp.StatusCode)
	}
	waitStatus(t, s, third.ID, StatusCompleted)
	if _, err := s.mgr.Cancel(third.ID); err == nil {
		t.Fatal("cancelling a completed job should fail")
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
	j, _ := s.mgr.Get(job.ID)
	if j.Status != StatusCancelled || j.Error == nil || j.Error.Code != "service_shutdown" {
		t.Fatalf("job after shutdown: %+v", j)
	}
	if _, err := s.mgr.Submit(&j.Request); err == nil {
		t.Fatal("submit after shutdown should fail")
	}
}

func TestPrune(t *testing.T) {
	run := func(ctx context.Context, c *jobCtl) (*Result, error) { return &Result{}, nil }
	s, ts, media := testServer(t, run, func(c *Config) { c.JobRetention = time.Minute })
	_, job := postJob(t, ts, media, "a.mp4")
	waitStatus(t, s, job.ID, StatusCompleted)
	s.mgr.prune(time.Now())
	if _, ok := s.mgr.Get(job.ID); !ok {
		t.Fatal("pruned too early")
	}
	s.mgr.prune(time.Now().Add(2 * time.Minute))
	if _, ok := s.mgr.Get(job.ID); ok {
		t.Fatal("not pruned")
	}
}
