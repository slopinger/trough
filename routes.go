package main

import (
	"log"
	"net/http"
	"runtime/debug"
	"time"
)

func (app *App) Handler() http.Handler {
	mux := http.NewServeMux()

	// discovery
	mux.HandleFunc("GET /.well-known/webfinger", app.handleWebfinger)
	mux.HandleFunc("GET /.well-known/host-meta", app.handleHostMeta)
	mux.HandleFunc("GET /.well-known/nodeinfo", app.handleNodeInfoIndex)
	mux.HandleFunc("GET /nodeinfo/{version}", app.handleNodeInfo)
	mux.HandleFunc("GET /api/v1/instance", app.handleInstanceAPI)

	// ActivityPub
	mux.HandleFunc("POST /inbox", app.handleInbox)
	mux.HandleFunc("POST /users/{name}/inbox", app.handleInbox)
	mux.HandleFunc("POST /actor/inbox", app.handleInbox)
	mux.HandleFunc("GET /actor", app.handleInstanceActorDoc)
	mux.HandleFunc("GET /actor/outbox", app.handleEmptyCollection)
	mux.HandleFunc("GET /users/{name}", app.handleActor)
	mux.HandleFunc("GET /users/{name}/outbox", app.handleOutbox)
	mux.HandleFunc("GET /users/{name}/followers", app.handleFollowCollection(true))
	mux.HandleFunc("GET /users/{name}/following", app.handleFollowCollection(false))
	mux.HandleFunc("GET /users/{name}/collections/featured", app.handleFeatured)
	mux.HandleFunc("GET /users/{name}/statuses/{id}", app.handleAPStatus)
	mux.HandleFunc("GET /users/{name}/statuses/{id}/activity", app.handleAPStatusActivity)
	mux.HandleFunc("GET /users/{name}/statuses/{id}/replies", app.handleAPReplies)

	// files
	mux.HandleFunc("GET /media/{file}", app.handleMedia)
	mux.HandleFunc("GET /proxy/{sig}", app.handleProxy)
	mux.HandleFunc("GET /static/{file}", app.handleStatic)
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/favicon.svg", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("User-agent: *\nDisallow: /proxy/\nDisallow: /search\nDisallow: /login\n"))
	})

	// web interface
	pub := func(h handler) http.HandlerFunc { return app.web(false, h) }
	auth := func(h handler) http.HandlerFunc { return app.web(true, h) }
	admin := func(h handler) http.HandlerFunc { return app.web(true, app.requireAdmin(h)) }

	mux.HandleFunc("GET /{$}", pub(app.handleRoot))
	mux.HandleFunc("GET /login", pub(app.handleLoginForm))
	mux.HandleFunc("POST /login", pub(app.handleLogin))
	mux.HandleFunc("POST /logout", auth(app.handleLogout))
	mux.HandleFunc("GET /home", auth(app.handleHome))
	mux.HandleFunc("GET /local", pub(app.handleLocal))
	mux.HandleFunc("GET /federated", auth(app.handleFederated))
	mux.HandleFunc("GET /tags/{tag}", pub(app.handleTag))
	mux.HandleFunc("GET /about", pub(app.handleAbout))
	mux.HandleFunc("GET /s/{id}", pub(app.handleThread))
	mux.HandleFunc("GET /compose", auth(app.handleComposePage))
	mux.HandleFunc("POST /statuses", auth(app.handlePost))
	mux.HandleFunc("POST /compose/ai", auth(app.handleAICompose))
	mux.HandleFunc("POST /admin/ai", admin(app.handleAdminAI))
	mux.HandleFunc("POST /statuses/{id}/{action}", auth(app.handleStatusAction))
	mux.HandleFunc("POST /accounts/{id}/{action}", auth(app.handleAccountAction))
	mux.HandleFunc("GET /notifications", auth(app.handleNotifications))
	mux.HandleFunc("POST /notifications/clear", auth(app.handleClearNotifications))
	mux.HandleFunc("GET /follow_requests", auth(app.handleFollowRequests))
	mux.HandleFunc("GET /search", auth(app.handleSearch))
	mux.HandleFunc("GET /authorize_interaction", auth(app.handleAuthorizeInteraction))
	mux.HandleFunc("GET /settings", auth(app.handleSettings))
	mux.HandleFunc("POST /settings/profile", auth(app.handleSettingsProfile))
	mux.HandleFunc("POST /settings/password", auth(app.handleSettingsPassword))
	mux.HandleFunc("POST /settings/account", auth(app.handleSettingsAccount))
	mux.HandleFunc("GET /signup", pub(app.handleSignupForm))
	mux.HandleFunc("POST /signup", pub(app.handleSignup))
	mux.HandleFunc("GET /timeline/new", auth(app.handleNewCount))
	mux.HandleFunc("POST /admin/registrations", admin(app.handleAdminRegistrations))
	mux.HandleFunc("POST /admin/invites", admin(app.handleAdminInvite))
	mux.HandleFunc("POST /settings/filters", auth(app.handleAddFilter))
	mux.HandleFunc("POST /settings/filters/{id}/delete", auth(app.handleDeleteFilter))
	mux.HandleFunc("GET /bookmarks", auth(app.handleBookmarks))
	mux.HandleFunc("GET /report", auth(app.handleReportForm))
	mux.HandleFunc("POST /report", auth(app.handleReport))
	mux.HandleFunc("POST /admin/reports/{id}/resolve", admin(app.handleResolveReport))
	mux.HandleFunc("GET /admin", admin(app.handleAdmin))
	mux.HandleFunc("POST /admin/users", admin(app.handleAdminCreateUser))
	mux.HandleFunc("POST /admin/users/{id}/password", admin(app.handleAdminResetPassword))
	mux.HandleFunc("POST /admin/settings", admin(app.handleAdminSettings))
	mux.HandleFunc("POST /admin/domain_blocks", admin(app.handleAdminDomainBlock))
	mux.HandleFunc("GET /{at}", pub(app.handleProfileOrStatus))
	mux.HandleFunc("GET /{at}/{id}", pub(app.handleProfileOrStatus))
	mux.HandleFunc("/", pub(func(w http.ResponseWriter, r *http.Request, p *Page) { app.notFound(w, p) }))

	return recoverer(mux)
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			if v := recover(); v != nil {
				log.Printf("panic serving %s %s: %v\n%s", r.Method, r.URL.Path, v, debug.Stack())
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
			if d := time.Since(start); d > 5*time.Second {
				log.Printf("slow request: %s %s took %s", r.Method, r.URL.Path, d)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
