package api

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"backtrack/internal/clips"
	"backtrack/internal/config"
	"backtrack/internal/events"
	"backtrack/internal/ingest"
	"backtrack/internal/media"
	"backtrack/internal/model"
	"backtrack/internal/playback"
	"backtrack/internal/storage"
)

type Server struct {
	cfg               config.Config
	store             *storage.Store
	logger            *slog.Logger
	ingest            *ingest.Server
	clips             *clips.Service
	events            *events.Hub
	playlistLatencyNS atomic.Int64
	liveParts         *livePartHub
}

func New(cfg config.Config, store *storage.Store, logger *slog.Logger, ing *ingest.Server, clipService *clips.Service, eventHub *events.Hub) *Server {
	return &Server{cfg: cfg, store: store, logger: logger, ingest: ing, clips: clipService, events: eventHub, liveParts: newLivePartHub()}
}

func (s *Server) PublishPart(part media.Part) {
	s.liveParts.publish(part)
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/system/health", s.health)
	m.Handle("GET /api/system/playback", s.reader(http.HandlerFunc(s.playbackSettings)))
	m.Handle("GET /metrics", s.admin(http.HandlerFunc(s.metrics)))
	m.HandleFunc("GET /api/auth/session", s.sessionStatus)
	m.HandleFunc("POST /api/auth/session", s.login)
	m.HandleFunc("DELETE /api/auth/session", s.logout)
	m.Handle("GET /api/streams", s.reader(http.HandlerFunc(s.streams)))
	m.Handle("POST /api/streams", s.admin(http.HandlerFunc(s.createStream)))
	m.Handle("GET /api/streams/{id}/publish-key", s.admin(http.HandlerFunc(s.streamPublishKey)))
	m.HandleFunc("POST /api/whip/{id}", s.whipPublish)
	m.Handle("DELETE /api/whip/{id}/{session}", s.admin(http.HandlerFunc(s.stopWHIP)))
	m.Handle("GET /api/streams/{id}", s.reader(http.HandlerFunc(s.getStream)))
	m.Handle("PUT /api/streams/{id}", s.admin(http.HandlerFunc(s.renameStream)))
	m.Handle("DELETE /api/streams/{id}", s.admin(http.HandlerFunc(s.deleteStream)))
	m.Handle("PUT /api/streams/{id}/enabled", s.admin(http.HandlerFunc(s.setStreamEnabled)))
	m.Handle("GET /api/streams/{id}/status", s.reader(http.HandlerFunc(s.streamStatus)))
	m.Handle("GET /api/streams/{id}/timeline", s.reader(http.HandlerFunc(s.timeline)))
	m.Handle("GET /api/streams/{id}/sessions", s.reader(http.HandlerFunc(s.sessions)))
	m.Handle("POST /api/streams/{id}/sessions/merge", s.admin(http.HandlerFunc(s.mergeSessions)))
	m.Handle("GET /api/sessions/{id}", s.reader(http.HandlerFunc(s.session)))
	m.Handle("GET /api/playback/resolve", s.reader(http.HandlerFunc(s.resolve)))
	m.Handle("GET /api/playback/live/{file...}", s.reader(http.HandlerFunc(s.livePlaylist)))
	m.Handle("GET /api/playback/dvr/{file...}", s.reader(http.HandlerFunc(s.dvrPlaylist)))
	m.Handle("GET /api/playback/vod/{file...}", s.reader(http.HandlerFunc(s.vodPlaylist)))
	m.Handle("GET /api/playback/previews", s.reader(http.HandlerFunc(s.playbackPreviews)))
	m.Handle("GET /api/media/previews/{id}", s.reader(http.HandlerFunc(s.previewImage)))
	m.Handle("GET /api/media/parts/{id}", s.reader(http.HandlerFunc(s.part)))
	m.Handle("GET /api/media/{file...}", s.reader(http.HandlerFunc(s.segment)))
	m.Handle("GET /api/media/init/{id}", s.reader(http.HandlerFunc(s.initSegment)))
	m.Handle("PUT /api/media/{id}/pin", s.admin(http.HandlerFunc(s.pinSegment)))
	m.Handle("GET /api/storage", s.reader(http.HandlerFunc(s.storageStats)))
	m.Handle("GET /api/retention", s.reader(http.HandlerFunc(s.retentionPolicy)))
	m.Handle("PUT /api/retention", s.admin(http.HandlerFunc(s.updateRetention)))
	m.Handle("GET /api/clips", s.reader(http.HandlerFunc(s.clipJobs)))
	m.Handle("POST /api/clips", s.admin(http.HandlerFunc(s.createClip)))
	m.Handle("GET /api/clips/{id}", s.reader(http.HandlerFunc(s.getClipJob)))
	m.Handle("GET /api/clips/{id}/download", s.reader(http.HandlerFunc(s.downloadClip)))
	m.Handle("GET /api/clips/{id}/play", s.reader(http.HandlerFunc(s.playClip)))
	m.Handle("DELETE /api/clips/{id}", s.admin(http.HandlerFunc(s.deleteClip)))
	m.Handle("PUT /api/clips/{id}/pin", s.admin(http.HandlerFunc(s.pinClip)))
	m.Handle("GET /api/events", s.reader(http.HandlerFunc(s.eventStream)))
	m.Handle("/", s.staticFiles())
	return m
}

func (s *Server) eventStream(w http.ResponseWriter, r *http.Request) {
	if s.events == nil {
		writeError(w, http.StatusServiceUnavailable, "event stream is unavailable")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming responses are unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	updates := s.events.Subscribe(r.Context())
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-updates:
			if !open {
				return
			}
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			if _, err = io.WriteString(w, "event: "+event.Type+"\ndata: "+string(data)+"\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) staticFiles() http.Handler {
	root := s.cfg.Server.StaticDir
	if root == "" {
		root = "./web/build"
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return http.NotFoundHandler()
	}
	fs := http.FileServer(http.Dir(abs))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(filepath.Clean("/"+r.URL.Path), string(os.PathSeparator))
		candidate := filepath.Join(abs, rel)
		if info, e := os.Stat(candidate); e == nil && !info.IsDir() {
			fs.ServeHTTP(w, r)
			return
		}
		fallback := filepath.Join(abs, "200.html")
		if _, e := os.Stat(fallback); e != nil {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, fallback)
	})
}

func (s *Server) admin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authRole(r) != "admin" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (s *Server) reader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if role := s.authRole(r); role != "admin" && role != "viewer" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (s *Server) authRole(r *http.Request) string {
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.Server.AdminToken)) == 1 {
			return "admin"
		}
		if s.cfg.Server.ReadOnlyToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.Server.ReadOnlyToken)) == 1 {
			return "viewer"
		}
	}
	if cookie, err := r.Cookie("dvr_session"); err == nil {
		role, _ := s.store.AuthSessionRole(r.Context(), cookie.Value)
		return role
	}
	return ""
}
func (s *Server) sessionStatus(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("dvr_session")
	if err != nil {
		writeJSON(w, 200, map[string]any{"authenticated": false, "role": ""})
		return
	}
	role, _ := s.store.AuthSessionRole(r.Context(), cookie.Value)
	writeJSON(w, 200, map[string]any{"authenticated": role != "", "role": role})
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	role := ""
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.cfg.Server.AdminToken)) == 1 {
		role = "admin"
	} else if s.cfg.Server.ReadOnlyToken != "" && subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.cfg.Server.ReadOnlyToken)) == 1 {
		role = "viewer"
	}
	if role == "" {
		writeError(w, 401, "invalid access token")
		return
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		writeError(w, 500, "could not create session")
		return
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	expires := time.Now().UTC().Add(12 * time.Hour)
	if err := s.store.SaveAuthSession(r.Context(), token, role, expires); err != nil {
		writeError(w, 500, "could not create session")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "dvr_session", Value: token, Path: "/", HttpOnly: true, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: 12 * 60 * 60})
	writeJSON(w, 200, map[string]any{"authenticated": true, "role": role})
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("dvr_session"); err == nil {
		_ = s.store.DeleteAuthSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "dvr_session", Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	writeJSON(w, 200, map[string]bool{"authenticated": false})
}
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.DB().PingContext(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC()})
}
func (s *Server) playbackSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]float64{
		"liveLatencyTargetSeconds": time.Duration(s.cfg.Playback.LiveLatencyTarget).Seconds(),
	})
}
func (s *Server) streams(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.ListStreams(r.Context())
	if err != nil {
		writeError(w, 500, "could not list streams")
		return
	}
	if v == nil {
		v = []model.Stream{}
	}
	writeJSON(w, 200, map[string]any{"streams": v})
}
func (s *Server) getStream(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.GetStream(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) setStreamEnabled(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled boolean is required")
		return
	}
	if err := s.store.SetStreamEnabled(r.Context(), r.PathValue("id"), *req.Enabled); err != nil {
		writeStorageError(w, err)
		return
	}
	v, err := s.store.GetStream(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if s.events != nil {
		s.events.Publish(events.Event{Type: "stream.updated", StreamID: v.ID})
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) renameStream(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 80 {
		writeError(w, http.StatusBadRequest, "channel name must be between 1 and 80 characters")
		return
	}
	if err := s.store.RenameStream(r.Context(), r.PathValue("id"), req.Name); err != nil {
		writeStorageError(w, err)
		return
	}
	v, err := s.store.GetStream(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if s.events != nil {
		s.events.Publish(events.Event{Type: "stream.updated", StreamID: v.ID})
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) deleteStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.ingest.IsActive(id) {
		writeError(w, http.StatusConflict, "频道正在推流，请先停止推流后再删除。")
		return
	}
	if err := s.store.DeleteStream(r.Context(), id); err != nil {
		if errors.Is(err, storage.ErrStreamHasActiveClipJobs) {
			writeError(w, http.StatusConflict, "频道仍有剪辑任务正在处理，请等待任务完成后再删除。")
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "频道不存在")
			return
		}
		writeError(w, http.StatusInternalServerError, "删除频道失败")
		return
	}
	if s.events != nil {
		s.events.Publish(events.Event{Type: "stream.deleted", StreamID: id})
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) streamStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v, err := s.store.GetStream(r.Context(), id)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"streamId":  id,
		"enabled":   v.Enabled,
		"live":      s.ingest.IsActive(id),
		"publisher": s.ingest.PublisherInfo(id),
	})
}

func parseTimeParam(r *http.Request, name string, required bool) (time.Time, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		if required {
			return time.Time{}, errors.New("missing " + name)
		}
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, errors.New(name + " must be RFC3339")
	}
	return t.UTC(), nil
}

func (s *Server) timeline(w http.ResponseWriter, r *http.Request) {
	start, err := parseTimeParam(r, "start", false)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	end, err := parseTimeParam(r, "end", false)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if end.IsZero() {
		end = time.Now().UTC()
	}
	if start.IsZero() {
		start = end.Add(-24 * time.Hour)
	}
	if !end.After(start) || end.Sub(start) > 90*24*time.Hour {
		writeError(w, 400, "timeline window must be positive and at most 90 days")
		return
	}
	if granularity := r.URL.Query().Get("granularity"); granularity != "" {
		var width time.Duration
		switch granularity {
		case "hour":
			width = time.Hour
		case "day":
			width = 24 * time.Hour
		default:
			writeError(w, 400, "granularity must be hour or day")
			return
		}
		buckets, err := s.store.TimelineBuckets(r.Context(), r.PathValue("id"), start, end, width)
		if err != nil {
			writeError(w, 500, "could not query coarse timeline")
			return
		}
		writeJSON(w, 200, map[string]any{"streamId": r.PathValue("id"), "start": start, "end": end, "granularity": granularity, "buckets": buckets, "live": s.ingest.IsActive(r.PathValue("id"))})
		return
	}
	ranges, err := s.store.Timeline(r.Context(), r.PathValue("id"), start, end)
	if err != nil {
		writeError(w, 500, "could not query timeline")
		return
	}
	writeJSON(w, 200, map[string]any{"streamId": r.PathValue("id"), "start": start, "end": end, "ranges": ranges, "live": s.ingest.IsActive(r.PathValue("id"))})
}
func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.Sessions(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 500, "could not query sessions")
		return
	}
	writeJSON(w, 200, map[string]any{"sessions": v})
}
func (s *Server) mergeSessions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionIDs []string                      `json:"sessionIds"`
		Ranges     []model.HistoryRangeSelection `json:"ranges"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeError(w, 400, "invalid merge request")
		return
	}
	var mergedID string
	var err error
	if len(req.Ranges) > 0 {
		mergedID, err = s.store.MergeHistoryRanges(r.Context(), r.PathValue("id"), req.Ranges)
	} else {
		mergedID, err = s.store.MergeSessions(r.Context(), r.PathValue("id"), req.SessionIDs)
	}
	if err != nil {
		if errors.Is(err, storage.ErrInvalidSessionMerge) {
			writeError(w, 400, strings.TrimPrefix(err.Error(), storage.ErrInvalidSessionMerge.Error()+": "))
		} else {
			writeError(w, 500, "could not merge sessions")
		}
		return
	}
	s.events.Publish(events.Event{Type: "timeline.updated", StreamID: r.PathValue("id")})
	writeJSON(w, 200, map[string]string{"sessionId": mergedID})
}
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.SessionSegments(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"sessionId": r.PathValue("id"), "segments": v})
}
func (s *Server) resolve(w http.ResponseWriter, r *http.Request) {
	streamID := r.URL.Query().Get("streamId")
	at, err := parseTimeParam(r, "at", true)
	if err != nil || streamID == "" {
		writeError(w, 400, "streamId and RFC3339 at are required")
		return
	}
	segs, err := s.store.Segments(r.Context(), streamID, at.Add(-time.Second), at.Add(time.Second))
	if err != nil {
		writeError(w, 500, "could not resolve playback time")
		return
	}
	for _, v := range segs {
		if !at.Before(v.StartTime) && at.Before(v.EndTime) {
			writeJSON(w, 200, map[string]any{"streamId": streamID, "segmentId": v.ID, "epochId": v.EpochID, "sessionId": v.SessionID, "segmentStart": v.StartTime, "segmentEnd": v.EndTime, "offset": at.Sub(v.StartTime).Seconds()})
			return
		}
	}
	writeJSON(w, 404, map[string]any{"error": "time is not available", "streamId": streamID, "at": at})
}
func playlistID(r *http.Request) (string, error) {
	file := r.PathValue("file")
	id, ok := strings.CutSuffix(file, ".m3u8")
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", errors.New("invalid playlist path")
	}
	return id, nil
}
func (s *Server) livePlaylist(w http.ResponseWriter, r *http.Request) {
	id, e := playlistID(r)
	if e != nil {
		writeError(w, 404, "playlist not found")
		return
	}
	active := s.ingest != nil && s.ingest.IsActive(id)
	if active {
		if msnValue := r.URL.Query().Get("_HLS_msn"); msnValue != "" {
			msn, msnErr := strconv.ParseUint(msnValue, 10, 64)
			partIndex := 0
			var partErr error
			if partValue := r.URL.Query().Get("_HLS_part"); partValue != "" {
				partIndex, partErr = strconv.Atoi(partValue)
			}
			if msnErr != nil || partErr != nil || partIndex < 0 {
				writeError(w, http.StatusBadRequest, "invalid LL-HLS reload position")
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			s.liveParts.waitForUpdate(ctx, id, msn, partIndex)
			cancel()
		}
	}
	segs, err := s.store.LatestSegments(r.Context(), id, time.Duration(s.cfg.Playback.LivePlaylistDuration))
	if err != nil {
		writeError(w, 500, "could not build live playlist")
		return
	}
	started := time.Now()
	var discontinuitySequence uint64
	if len(segs) > 0 {
		discontinuitySequence, err = s.store.DiscontinuitySequence(r.Context(), id, segs[0].StartTime)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not build playlist discontinuity sequence")
			return
		}
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	body := playback.LowLatencyPlaylist(segs, s.liveParts.list(id), func(epoch string) string { return "/api/media/init/" + epoch }, discontinuitySequence, media.PartTargetDuration, id, active)
	s.playlistLatencyNS.Store(time.Since(started).Nanoseconds())
	_, _ = w.Write([]byte(body))
}

func (s *Server) part(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(r.PathValue("id"), ".m4s")
	streamID := r.URL.Query().Get("streamId")
	if id == "" || streamID == "" || strings.ContainsAny(id, `/\\`) {
		writeError(w, http.StatusBadRequest, "invalid partial segment request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	part, ok := s.liveParts.waitForPart(ctx, streamID, id)
	if !ok {
		writeError(w, http.StatusNotFound, "partial segment is not available")
		return
	}
	w.Header().Set("Content-Type", "video/iso.segment")
	w.Header().Set("Cache-Control", "private, max-age=10")
	_, _ = w.Write(part.Data)
}
func (s *Server) dvrPlaylist(w http.ResponseWriter, r *http.Request) {
	id, e := playlistID(r)
	if e != nil {
		writeError(w, 404, "playlist not found")
		return
	}
	start, err := parseTimeParam(r, "start", false)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	end, err := parseTimeParam(r, "end", false)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if end.IsZero() {
		end = time.Now().UTC()
	}
	if start.IsZero() {
		start = end.Add(-time.Duration(s.cfg.Playback.DefaultDVRWindow))
	}
	if !end.After(start) || end.Sub(start) > 24*time.Hour {
		writeError(w, 400, "DVR range must be positive and at most 24 hours")
		return
	}
	segs, err := s.store.Segments(r.Context(), id, start, end)
	if err != nil {
		writeError(w, 500, "could not build DVR playlist")
		return
	}
	endList := false
	if r.URL.Query().Has("end") {
		latest, latestErr := s.store.LatestCommittedEndForStream(r.Context(), id)
		if latestErr != nil {
			writeError(w, http.StatusServiceUnavailable, "could not determine committed media frontier")
			return
		}
		endList = !latest.IsZero() && !end.After(latest)
	}
	s.writePlaylist(w, r, id, segs, endList)
}
func (s *Server) vodPlaylist(w http.ResponseWriter, r *http.Request) {
	id, e := playlistID(r)
	if e != nil {
		writeError(w, 404, "playlist not found")
		return
	}
	ended, err := s.store.SessionEnded(r.Context(), id)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if !ended {
		writeError(w, http.StatusConflict, "session is still active; use its live or DVR playlist")
		return
	}
	segs, err := s.store.SessionSegments(r.Context(), id)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	s.writePlaylist(w, r, "", segs, true)
}
func (s *Server) writePlaylist(w http.ResponseWriter, r *http.Request, streamID string, segs []model.Segment, endList bool) {
	started := time.Now()
	defer func() { s.playlistLatencyNS.Store(time.Since(started).Nanoseconds()) }()
	var discontinuitySequence uint64
	if !endList && len(segs) > 0 {
		var err error
		discontinuitySequence, err = s.store.DiscontinuitySequence(r.Context(), streamID, segs[0].StartTime)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not build playlist discontinuity sequence")
			return
		}
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	body := playback.Playlist(segs, func(epoch string) string { return "/api/media/init/" + epoch }, endList, discontinuitySequence)
	s.playlistLatencyNS.Store(time.Since(started).Nanoseconds())
	_, _ = w.Write([]byte(body))
}
func (s *Server) segment(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(r.PathValue("file"), ".m4s")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, 404, "media not found")
		return
	}
	v, err := s.store.GetSegment(r.Context(), id)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	f, err := s.store.OpenSegment(v)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "video/iso.segment")
	w.Header().Set("Cache-Control", "private, max-age="+strconv.Itoa(int(storage.SegmentURLCacheMaxAge/time.Second)))
	http.ServeContent(w, r, v.ID, v.StartTime, f)
}

func (s *Server) playbackPreviews(w http.ResponseWriter, r *http.Request) {
	streamID := r.URL.Query().Get("streamId")
	start, err := parseTimeParam(r, "start", true)
	if err != nil || streamID == "" {
		writeError(w, http.StatusBadRequest, "streamId and RFC3339 start/end are required")
		return
	}
	end, err := parseTimeParam(r, "end", true)
	if err != nil || !end.After(start) || end.Sub(start) > 24*time.Hour {
		writeError(w, http.StatusBadRequest, "preview range must be positive and at most 24 hours")
		return
	}
	segments, err := s.store.Segments(r.Context(), streamID, start, end)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not query playback previews")
		return
	}
	type preview struct {
		At  time.Time `json:"at"`
		URL string    `json:"url"`
	}
	previews := make([]preview, 0, len(segments)/3+1)
	for _, segment := range segments {
		if segment.Sequence%3 != 0 || !segment.RandomAccess || !s.store.HasPreview(segment.ID) {
			continue
		}
		previews = append(previews, preview{At: segment.StartTime, URL: "/api/media/previews/" + segment.ID})
	}
	w.Header().Set("Cache-Control", "private, max-age=5")
	writeJSON(w, http.StatusOK, map[string]any{"previews": previews})
}

func (s *Server) previewImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f, err := s.store.OpenPreview(id)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read preview image")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable")
	http.ServeContent(w, r, id+".jpg", info.ModTime(), f)
}

func (s *Server) initSegment(w http.ResponseWriter, r *http.Request) {
	f, err := s.store.OpenInit(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeError(w, 500, "could not read initialization segment")
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, "init-"+r.PathValue("id")+".mp4", st.ModTime(), f)
}

func (s *Server) retentionDefaults() storage.RetentionPolicy {
	return storage.RetentionPolicy{MaxAgeHours: time.Duration(s.cfg.Retention.MaxAge).Hours(), MaxSizeBytes: int64(s.cfg.Retention.MaxSize), PreservePinned: s.cfg.Retention.PreservePinned}
}
func (s *Server) retentionPolicy(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetRetentionPolicy(r.Context(), s.retentionDefaults())
	if err != nil {
		writeError(w, 500, "could not read retention policy")
		return
	}
	writeJSON(w, 200, p)
}
func (s *Server) updateRetention(w http.ResponseWriter, r *http.Request) {
	var p storage.RetentionPolicy
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, 400, "invalid policy")
		return
	}
	if err := s.store.SetRetentionPolicy(r.Context(), p); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, p)
}
func (s *Server) storageStats(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.Stats(r.Context())
	if err != nil {
		writeError(w, 500, "could not read storage stats")
		return
	}
	writeJSON(w, 200, struct {
		storage.StorageStats
		LibraryMaxSizeBytes       int64   `json:"libraryMaxSizeBytes"`
		TemporaryMaxSizeBytes     int64   `json:"temporaryMaxSizeBytes"`
		TemporaryRetentionSeconds float64 `json:"temporaryRetentionSeconds"`
		ReservedFreeSpaceBytes    int64   `json:"reservedFreeSpaceBytes"`
		CleanupIntervalSeconds    float64 `json:"cleanupIntervalSeconds"`
	}{
		StorageStats:              v,
		LibraryMaxSizeBytes:       int64(s.cfg.Clips.LibraryMaxSize),
		TemporaryMaxSizeBytes:     int64(s.cfg.Clips.TemporaryMaxSize),
		TemporaryRetentionSeconds: time.Duration(s.cfg.Clips.TemporaryFileRetention).Seconds(),
		ReservedFreeSpaceBytes:    int64(s.cfg.Storage.ReservedFreeSpace),
		CleanupIntervalSeconds:    time.Duration(s.cfg.Retention.CleanupInterval).Seconds(),
	})
}

func (s *Server) clipJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.store.ClipJobs(r.Context())
	if err != nil {
		writeError(w, 500, "could not list clip jobs")
		return
	}
	writeJSON(w, 200, map[string]any{"jobs": jobs})
}
func (s *Server) createClip(w http.ResponseWriter, r *http.Request) {
	var req model.ClipRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid clip request")
		return
	}
	if s.clips == nil {
		writeError(w, 503, "clip service is unavailable")
		return
	}
	job, err := s.clips.Create(r.Context(), req)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 202, job)
}
func (s *Server) getClipJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.ClipJob(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, 200, job)
}
func (s *Server) deleteClip(w http.ResponseWriter, r *http.Request) {
	if s.clips == nil {
		writeError(w, 503, "clip service is unavailable")
		return
	}
	if err := s.clips.Cancel(r.Context(), r.PathValue("id")); err != nil {
		writeStorageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) downloadClip(w http.ResponseWriter, r *http.Request) {
	s.serveClip(w, r, false)
}
func (s *Server) playClip(w http.ResponseWriter, r *http.Request) {
	s.serveClip(w, r, true)
}
func (s *Server) serveClip(w http.ResponseWriter, r *http.Request, inline bool) {
	job, err := s.store.ClipJob(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if job.Status != "COMPLETED" {
		writeError(w, 409, "clip is not ready")
		return
	}
	f, err := s.store.OpenClip(r.Context(), job.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeError(w, 500, "could not read exported clip")
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	disposition := "attachment"
	if inline {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", disposition+`; filename="clip-`+job.ID+`.mp4"`)
	http.ServeContent(w, r, "clip-"+job.ID+".mp4", st.ModTime(), f)
}
func (s *Server) pinSegment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Pinned bool `json:"pinned"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, 400, "pinned boolean required")
		return
	}
	if err := s.store.SetSegmentPinned(r.Context(), r.PathValue("id"), req.Pinned); err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"pinned": req.Pinned})
}
func (s *Server) pinClip(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Pinned bool `json:"pinned"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, 400, "pinned boolean required")
		return
	}
	if err := s.store.SetClipPinned(r.Context(), r.PathValue("id"), req.Pinned); err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"pinned": req.Pinned})
}

func (s *Server) createStream(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	key, err := ingest.NewPublishKey()
	if err != nil {
		writeError(w, 500, "could not generate publish key")
		return
	}
	v, err := s.store.CreateStream(r.Context(), strings.TrimSpace(req.Name), key)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	encrypted, err := s.encryptPublishKey(key)
	if err != nil {
		writeError(w, 500, "could not protect publish key")
		return
	}
	if err = s.store.SetStreamPublishKey(r.Context(), v.ID, key, encrypted); err != nil {
		writeStorageError(w, err)
		return
	}
	if s.events != nil {
		s.events.Publish(events.Event{Type: "stream.created", StreamID: v.ID})
	}
	v.PublishKey = key
	writeJSON(w, 201, map[string]any{"stream": v, "obs": map[string]string{"server": "rtmp://<host>:1935/live", "streamKey": v.ID + "?key=" + key}})
}

func (s *Server) streamPublishKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetStream(r.Context(), id); err != nil {
		writeStorageError(w, err)
		return
	}
	encrypted, err := s.store.StreamPublishKey(r.Context(), id)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	key := ""
	rotated := encrypted == ""
	if rotated {
		key, err = ingest.NewPublishKey()
		if err == nil {
			encrypted, err = s.encryptPublishKey(key)
		}
		if err == nil {
			err = s.store.SetStreamPublishKey(r.Context(), id, key, encrypted)
		}
	} else {
		key, err = s.decryptPublishKey(encrypted)
	}
	if err != nil {
		writeError(w, 500, "could not load publish key")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"publishKey": key, "obs": map[string]string{"server": "rtmp://<host>:1935/live", "streamKey": id + "?key=" + key}, "rotated": rotated})
}

func (s *Server) publishKeyAEAD() (cipher.AEAD, error) {
	key := sha256.Sum256([]byte("backtrack/publish-key/v1\x00" + s.cfg.Server.AdminToken))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *Server) encryptPublishKey(value string) (string, error) {
	aead, err := s.publishKeyAEAD()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	ciphertext := aead.Seal(nonce, nonce, []byte(value), []byte("backtrack/publish-key/v1"))
	return base64.RawStdEncoding.EncodeToString(ciphertext), nil
}

func (s *Server) decryptPublishKey(value string) (string, error) {
	ciphertext, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	aead, err := s.publishKeyAEAD()
	if err != nil {
		return "", err
	}
	if len(ciphertext) < aead.NonceSize() {
		return "", errors.New("invalid encrypted publish key")
	}
	nonce, body := ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, body, []byte("backtrack/publish-key/v1"))
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func (s *Server) whipPublish(w http.ResponseWriter, r *http.Request) {
	key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || strings.TrimSpace(key) == "" {
		writeError(w, http.StatusUnauthorized, "publish key required")
		return
	}
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/sdp") {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/sdp")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	offer, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid SDP offer")
		return
	}
	session, err := s.ingest.StartWHIP(r.Context(), r.PathValue("id"), key, string(offer))
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "invalid publish key") || strings.Contains(err.Error(), "publish") && strings.Contains(err.Error(), "key") {
			status = http.StatusUnauthorized
		} else if strings.Contains(err.Error(), "active publisher") {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/api/whip/"+r.PathValue("id")+"/"+session.ID)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, session.Answer)
}

func (s *Server) stopWHIP(w http.ResponseWriter, r *http.Request) {
	// WHIP DELETE is intentionally idempotent: the peer may already have
	// disconnected and released its server-side session before this request.
	s.ingest.CloseWHIP(r.PathValue("session"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) Serve(ctx context.Context) error {
	var connections atomic.Int64
	const maxHTTPConnections = 256
	h := &http.Server{
		Addr:              s.cfg.Server.HTTPListen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ConnState: func(conn net.Conn, state http.ConnState) {
			switch state {
			case http.StateNew:
				if current := connections.Add(1); current > maxHTTPConnections {
					s.logger.Warn("HTTP connection limit exceeded", "limit", maxHTTPConnections)
					_ = conn.Close()
				}
			case http.StateClosed, http.StateHijacked:
				connections.Add(-1)
			}
		},
	}
	errCh := make(chan error, 1)
	go func() { s.logger.Info("HTTP listener ready", "addr", h.Addr); errCh <- h.ListenAndServe() }()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownErr := h.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			_ = h.Close()
		}
		listenErr := <-errCh
		if errors.Is(listenErr, http.ErrServerClosed) {
			listenErr = nil
		}
		if shutdownErr != nil {
			return shutdownErr
		}
		return listenErr
	}
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func writeStorageError(w http.ResponseWriter, err error) {
	if errors.Is(err, io.EOF) {
		writeError(w, 404, "not found")
		return
	}
	writeError(w, 404, "not found")
}
