package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type server struct {
	cfg   *Config
	media *mediaDir
	mgr   *Manager
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /api/timelapses", s.handleCreate)
	mux.HandleFunc("GET /api/timelapses", s.handleList)
	mux.HandleFunc("GET /api/timelapses/{id}", s.handleGet)
	mux.HandleFunc("DELETE /api/timelapses/{id}", s.handleCancel)
	mux.HandleFunc("GET /api/ws", s.handleWS)
	mux.HandleFunc("/", handleUnmatched)
	return mux
}

// handleUnmatched keeps unknown routes and wrong methods in the JSON error format.
func handleUnmatched(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if p == "/healthz" || p == "/api/ws" || p == "/api/timelapses" || strings.HasPrefix(p, "/api/timelapses/") {
		writeErr(w, errf(405, "method_not_allowed", "%s is not allowed on %s", r.Method, p))
		return
	}
	writeErr(w, errf(404, "not_found", "no such endpoint"))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		log.Printf("💥 internal error: %v", err)
		ae = &apiError{Status: 500, Code: "internal_error", Message: "internal error"}
	}
	writeJSON(w, ae.Status, map[string]any{"error": map[string]string{"code": ae.Code, "message": ae.Message}})
}

func (s *server) handleCreate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req CreateRequest
	if err := dec.Decode(&req); err != nil {
		writeErr(w, errf(400, "invalid_request", "invalid JSON body: %v", err))
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeErr(w, errf(400, "invalid_request", "unexpected data after the JSON body"))
		return
	}

	spec, err := validateRequest(&req, s.media, s.cfg)
	if err == nil {
		err = s.checkFiles(spec)
	}
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			log.Printf("🚫 invalid request from %s [%s]: %s", r.RemoteAddr, ae.Code, ae.Message)
		}
		writeErr(w, err)
		return
	}

	job, err := s.mgr.Submit(spec)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status < 500 {
			log.Printf("⏸️ request from %s refused [%s]: %s", r.RemoteAddr, ae.Code, ae.Message)
		}
		writeErr(w, err)
		return
	}
	w.Header().Set("Location", "/api/timelapses/"+job.ID)
	writeJSON(w, http.StatusAccepted, job)
}

// checkFiles runs the filesystem checks that validateRequest leaves out.
func (s *server) checkFiles(spec *Spec) error {
	if err := s.media.checkReal(spec.SequenceFile); err != nil {
		return errf(400, "invalid_path", "sequenceFile: %v", err)
	}
	st, err := os.Stat(spec.SequenceFile)
	if err != nil || !st.Mode().IsRegular() {
		return errf(404, "sequence_not_found", "sequenceFile does not exist")
	}
	outs := []string{spec.OutputFile}
	if spec.Thumbnail != nil {
		outs = append(outs, spec.Thumbnail.OutputFile)
	}
	for _, o := range outs {
		if err := s.media.checkReal(o); err != nil {
			return errf(400, "invalid_path", "%s: %v", s.media.relative(o), err)
		}
	}
	return nil
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	writeJSON(w, http.StatusOK, map[string]any{"timelapses": s.mgr.List(status)})
}

func (s *server) handleGet(w http.ResponseWriter, r *http.Request) {
	job, ok := s.mgr.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, errf(404, "not_found", "timelapse not found"))
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	log.Printf("🛑 cancel requested for timelapse %s by %s", id, r.RemoteAddr)
	job, err := s.mgr.Cancel(id)
	if err != nil {
		log.Printf("↩️ cancel of timelapse %s refused: %v", id, err)
		writeErr(w, err)
		return
	}
	status := http.StatusOK
	if !isTerminal(job.Status) {
		status = http.StatusAccepted // still winding down; the WebSocket reports the final state
	}
	writeJSON(w, status, job)
}

// handleWS streams job events. With ?jobId=<id> only that job is followed.
func (s *server) handleWS(w http.ResponseWriter, r *http.Request) {
	sub, snapshot, err := s.mgr.Subscribe(r.URL.Query().Get("jobId"))
	if err != nil {
		writeErr(w, err)
		return
	}
	defer s.mgr.Unsubscribe(sub)

	// Every origin is accepted: the service has no authentication and is only reachable on
	// the trusted network.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return // Accept has already written the HTTP error
	}
	defer c.CloseNow()
	log.Printf("🔌 WebSocket client %s connected (%d connected)", r.RemoteAddr, s.mgr.Subscribers())
	defer log.Printf("🔌 WebSocket client %s disconnected", r.RemoteAddr)

	// CloseRead handles control frames and cancels ctx when the client goes away; the
	// protocol is server -> client only.
	ctx := c.CloseRead(r.Context())

	if err := writeWS(ctx, c, snapshot); err != nil {
		return
	}
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.C:
			if !ok {
				c.Close(websocket.StatusTryAgainLater, "disconnected: slow consumer or shutdown")
				return
			}
			if err := writeWS(ctx, c, ev); err != nil {
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func writeWS(ctx context.Context, c *websocket.Conn, ev Event) error {
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return wsjson.Write(wctx, c, ev)
}
