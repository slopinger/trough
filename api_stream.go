package main

// Mastodon streaming API: WebSocket (/api/v1/streaming) and server-sent
// events (/api/v1/streaming/user, ...).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

type streamMsg struct {
	Stream  []string `json:"stream"`
	Event   string   `json:"event"`
	Payload string   `json:"payload"`
}

type streamSub struct {
	me   *Account
	mu   sync.Mutex
	subs map[string]bool // "user", "public", "public:local", "hashtag:foo", ...
	out  chan streamMsg
}

func (s *streamSub) has(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subs[name]
}

type streamHub struct {
	mu   sync.Mutex
	subs map[*streamSub]bool
}

func (h *streamHub) add(s *streamSub) {
	h.mu.Lock()
	h.subs[s] = true
	h.mu.Unlock()
}

func (h *streamHub) remove(s *streamSub) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

func (h *streamHub) list() []*streamSub {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*streamSub, 0, len(h.subs))
	for s := range h.subs {
		out = append(out, s)
	}
	return out
}

var hub = &streamHub{subs: map[*streamSub]bool{}}

func (s *streamSub) send(m streamMsg) {
	select {
	case s.out <- m:
	default: // slow client: drop rather than block publishers
	}
}

// publishStatus pushes a new (or edited) status to every subscriber allowed to see it.
func (app *App) publishStatus(statusID string, edited bool) {
	subs := hub.list()
	if len(subs) == 0 {
		return
	}
	go func() {
		raw, err := app.StatusByID(statusID)
		if err != nil {
			return
		}
		event := "update"
		if edited {
			event = "status.update"
		}
		for _, s := range subs {
			if s.me == nil {
				continue
			}
			st, err := app.loadStatus(s.me, statusID)
			if err != nil {
				continue // not visible to this viewer
			}
			if filtered := app.applyFilters(s.me, []*Status{st}); len(filtered) == 0 {
				continue
			}
			payload, _ := json.Marshal(app.apiStatus(s.me, st))
			author := raw.AccountID
			if st.BoostOf != nil {
				author = st.BoostOf.AccountID
			}
			muted := app.isMuting(s.me.ID, raw.AccountID) || app.isMuting(s.me.ID, author)
			if s.has("user") && !muted && (raw.AccountID == s.me.ID || app.relationship(s.me.ID, raw.AccountID).Following || mentions(st, s.me.ID)) {
				s.send(streamMsg{[]string{"user"}, event, string(payload)})
			}
			public := raw.Visibility == "public" && raw.BoostOfID == "" && !muted
			if public && s.has("public") {
				s.send(streamMsg{[]string{"public"}, event, string(payload)})
			}
			if public && raw.Local && s.has("public:local") {
				s.send(streamMsg{[]string{"public:local"}, event, string(payload)})
			}
			if public && !raw.Local && s.has("public:remote") {
				s.send(streamMsg{[]string{"public:remote"}, event, string(payload)})
			}
			for _, t := range st.Tags {
				if public && s.has("hashtag:"+t) {
					s.send(streamMsg{[]string{"hashtag", t}, event, string(payload)})
				}
			}
			if raw.Visibility == "direct" && s.has("direct") {
				s.send(streamMsg{[]string{"direct"}, event, string(payload)})
			}
		}
	}()
}

func mentions(st *Status, accountID string) bool {
	for _, m := range st.Mentions {
		if m.ID == accountID {
			return true
		}
	}
	return false
}

func (app *App) publishNotification(accountID, notifID string) {
	subs := hub.list()
	if len(subs) == 0 {
		return
	}
	go func() {
		for _, s := range subs {
			if s.me == nil || s.me.ID != accountID || !(s.has("user") || s.has("user:notification")) {
				continue
			}
			ns, err := app.notificationsWhere(s.me, `n.account_id=? AND n.id=?`, []any{accountID, notifID}, "DESC", 1)
			if err != nil || len(ns) == 0 {
				continue
			}
			payload, _ := json.Marshal(app.apiNotification(s.me, ns[0]))
			stream := "user"
			if !s.has("user") {
				stream = "user:notification"
			}
			s.send(streamMsg{[]string{stream}, "notification", string(payload)})
		}
	}()
}

func (app *App) publishDelete(statusID string) {
	for _, s := range hub.list() {
		for _, name := range []string{"user", "public", "public:local", "public:remote"} {
			if s.has(name) {
				s.send(streamMsg{[]string{name}, "delete", statusID})
				break
			}
		}
	}
}

// streamName maps Mastodon stream names (and SSE paths) to subscription keys.
func streamName(stream, tag string) string {
	switch stream {
	case "user", "user:notification", "public", "public:local", "public:remote", "direct":
		return stream
	case "public:media", "public:local:media", "public:remote:media":
		return strings.TrimSuffix(stream, ":media")
	case "hashtag", "hashtag:local":
		if tag != "" {
			return "hashtag:" + strings.ToLower(tag)
		}
	}
	return ""
}

func (app *App) handleStreaming(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("access_token")
	proto := r.Header.Get("Sec-WebSocket-Protocol")
	if tok == "" && proto != "" {
		tok = strings.TrimSpace(strings.Split(proto, ",")[0])
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	c := app.apiAuth(r)
	if c == nil || c.Me == nil || !c.can("read") {
		apiError(w, 401, "Error: Missing access token")
		return
	}
	sub := &streamSub{me: c.Me, subs: map[string]bool{}, out: make(chan streamMsg, 64)}
	initial := r.URL.Query().Get("stream")
	if p := r.PathValue("stream"); p != "" {
		initial = strings.ReplaceAll(p, "/", ":")
	}
	if n := streamName(initial, r.URL.Query().Get("tag")); n != "" {
		sub.subs[n] = true
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		srv := websocket.Server{
			Handshake: func(cfg *websocket.Config, req *http.Request) error {
				if proto != "" {
					cfg.Protocol = []string{strings.TrimSpace(strings.Split(proto, ",")[0])}
				}
				return nil
			},
			Handler: func(ws *websocket.Conn) { app.serveWebsocket(ws, sub) },
		}
		srv.ServeHTTP(w, r)
		return
	}
	// server-sent events
	flusher, ok := w.(http.Flusher)
	if !ok || len(sub.subs) == 0 {
		apiError(w, 400, "Unknown stream type")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	hub.add(sub)
	defer hub.remove(sub)
	fmt.Fprint(w, ":)\n\n")
	flusher.Flush()
	tick := time.NewTicker(25 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			fmt.Fprint(w, ":thump\n\n")
			flusher.Flush()
		case m := <-sub.out:
			data := m.Payload
			if m.Event == "delete" {
				// SSE clients JSON-decode data; our ids aren't numbers, so quote them
				b, _ := json.Marshal(m.Payload)
				data = string(b)
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", m.Event, strings.ReplaceAll(data, "\n", ""))
			flusher.Flush()
		}
	}
}

func (app *App) serveWebsocket(ws *websocket.Conn, sub *streamSub) {
	hub.add(sub)
	defer hub.remove(sub)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var msg struct {
				Type, Stream, Tag string
			}
			if err := websocket.JSON.Receive(ws, &msg); err != nil {
				return
			}
			n := streamName(msg.Stream, msg.Tag)
			if n == "" {
				continue
			}
			sub.mu.Lock()
			switch msg.Type {
			case "subscribe":
				sub.subs[n] = true
			case "unsubscribe":
				delete(sub.subs, n)
			}
			sub.mu.Unlock()
		}
	}()
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			// keep proxies from closing an idle connection
			if _, err := ws.Write([]byte(`{"event":"ping"}`)); err != nil {
				return
			}
		case m := <-sub.out:
			if err := websocket.JSON.Send(ws, m); err != nil {
				return
			}
		}
	}
}
