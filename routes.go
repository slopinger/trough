package main

import (
	"log"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

func (app *App) Handler() http.Handler {
	mux := http.NewServeMux()

	// discovery
	mux.Handle("GET /.well-known/webfinger", cors(http.HandlerFunc(app.handleWebfinger)))
	mux.Handle("GET /.well-known/host-meta", cors(http.HandlerFunc(app.handleHostMeta)))
	mux.Handle("GET /.well-known/nodeinfo", cors(http.HandlerFunc(app.handleNodeInfoIndex)))
	mux.Handle("GET /nodeinfo/{version}", cors(http.HandlerFunc(app.handleNodeInfo)))
	app.apiRoutes(mux)

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
	mux.HandleFunc("GET /s/{id}/history", pub(app.handleHistory))
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
	mux.HandleFunc("GET /lists", auth(app.handleLists))
	mux.HandleFunc("POST /lists", auth(app.handleCreateList))
	mux.HandleFunc("GET /lists/{id}", auth(app.handleList))
	mux.HandleFunc("POST /lists/{id}/{action}", auth(app.handleListAction))
	mux.HandleFunc("POST /tags/{tag}/{action}", auth(app.handleTagFollow))
	mux.HandleFunc("POST /admin/relays", admin(app.handleAdminRelay))
	mux.HandleFunc("POST /admin/emoji", admin(app.handleAdminEmoji))
	mux.HandleFunc("GET /emojis/{code}", app.handleEmojiObject)
	mux.HandleFunc("GET /report", auth(app.handleReportForm))
	mux.HandleFunc("POST /report", auth(app.handleReport))
	mux.HandleFunc("POST /admin/reports/{id}/resolve", admin(app.handleResolveReport))
	mux.HandleFunc("GET /admin", admin(app.handleAdmin))
	mux.HandleFunc("POST /admin/users", admin(app.handleAdminCreateUser))
	mux.HandleFunc("POST /admin/users/{id}/password", admin(app.handleAdminResetPassword))
	mux.HandleFunc("POST /admin/settings", admin(app.handleAdminSettings))
	mux.HandleFunc("POST /admin/domain_blocks", admin(app.handleAdminDomainBlock))
	mux.HandleFunc("GET /oauth/authorize", auth(app.handleAuthorizeForm))
	mux.HandleFunc("POST /oauth/authorize", auth(app.handleAuthorize))
	mux.HandleFunc("POST /settings/apps/{id}/revoke", auth(app.handleRevokeApp))
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
		// Rails (Mastodon) accepts trailing slashes and some clients send them
		if p := r.URL.Path; len(p) > 1 && strings.HasSuffix(p, "/") && (strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/oauth/")) {
			r.URL.Path = strings.TrimRight(p, "/")
			r.URL.RawPath = ""
		}
		next.ServeHTTP(w, r)
	})
}

// apiRoutes registers the Mastodon-compatible client API and OAuth.
func (app *App) apiRoutes(mux *http.ServeMux) {
	h := func(pattern string, f http.HandlerFunc) { mux.Handle(pattern, cors(f)) }
	pub := func(f apiHandler) http.HandlerFunc { return app.api("", false, f) }
	read := func(f apiHandler) http.HandlerFunc { return app.api("read", true, f) }
	write := func(f apiHandler) http.HandlerFunc { return app.api("write", true, f) }
	follow := func(f apiHandler) http.HandlerFunc { return app.api("follow", true, f) }

	// preflight for everything under /api and /oauth
	mux.Handle("OPTIONS /api/", cors(http.NotFoundHandler()))
	mux.Handle("OPTIONS /oauth/", cors(http.NotFoundHandler()))
	mux.Handle("OPTIONS /.well-known/", cors(http.NotFoundHandler()))
	mux.Handle("OPTIONS /nodeinfo/", cors(http.NotFoundHandler()))

	h("GET /.well-known/oauth-authorization-server", app.handleOAuthMetadata)
	h("POST /oauth/token", app.handleToken)
	h("POST /oauth/revoke", app.handleRevoke)
	h("POST /api/v1/apps", pub(app.handleCreateApp))
	h("GET /api/v1/apps/verify_credentials", app.api("", false, func(w http.ResponseWriter, r *http.Request, c *apiCtx) {
		if c.AppID == "" {
			apiError(w, 401, "The access token is invalid")
			return
		}
		app.handleVerifyApp(w, r, c)
	}))

	h("GET /api/v1/instance", pub(app.apiInstance))
	h("GET /api/v2/instance", pub(app.apiInstance2))
	h("GET /api/v1/instance/peers", pub(app.apiPeers))
	h("GET /api/v1/instance/activity", pub(emptyList))
	h("GET /api/v1/instance/rules", pub(emptyList))
	h("GET /api/v1/instance/domain_blocks", pub(emptyList))
	h("GET /api/v1/custom_emojis", pub(app.apiCustomEmojis))
	h("GET /api/v1/announcements", pub(emptyList))
	h("GET /api/v1/trends", pub(emptyList))
	h("GET /api/v1/trends/tags", pub(emptyList))
	h("GET /api/v1/trends/statuses", pub(emptyList))
	h("GET /api/v1/trends/links", pub(emptyList))
	h("GET /api/v1/directory", pub(func(w http.ResponseWriter, r *http.Request, c *apiCtx) {
		accs, _ := app.accountsQuery(`SELECT ` + accountCols + ` FROM accounts a JOIN users u ON u.account_id=a.id ORDER BY u.last_seen DESC LIMIT 40`)
		app.writeAccounts(w, accs)
	}))
	h("GET /api/v1/preferences", read(app.apiPreferences))
	h("GET /api/v1/markers", read(app.apiGetMarkers))
	h("POST /api/v1/markers", write(app.apiSetMarkers))
	h("GET /api/v1/lists", read(app.apiLists))
	h("POST /api/v1/lists", write(app.apiCreateList))
	h("GET /api/v1/lists/{id}", read(app.apiList1))
	h("PUT /api/v1/lists/{id}", write(app.apiList1))
	h("DELETE /api/v1/lists/{id}", write(app.apiList1))
	h("GET /api/v1/lists/{id}/accounts", read(app.apiListAccounts))
	h("POST /api/v1/lists/{id}/accounts", write(app.apiListAccounts))
	h("DELETE /api/v1/lists/{id}/accounts", write(app.apiListAccounts))
	h("GET /api/v1/followed_tags", read(app.apiFollowedTags))
	h("GET /api/v1/tags/{name}", pub(app.apiGetTag))
	h("POST /api/v1/tags/{name}/{action}", follow(app.apiTagAction))
	h("GET /api/v1/featured_tags", read(emptyList))
	h("GET /api/v1/featured_tags/suggestions", read(emptyList))
	h("GET /api/v1/domain_blocks", read(emptyList))
	h("GET /api/v1/endorsements", read(emptyList))
	h("GET /api/v1/filters", read(app.apiFiltersV1))
	h("GET /api/v2/filters", read(app.apiFiltersV2))
	h("GET /api/v1/conversations", read(app.apiConversations))
	h("GET /api/v1/suggestions", read(emptyList))
	h("GET /api/v2/suggestions", read(emptyList))

	h("GET /api/v1/accounts/verify_credentials", read(app.apiVerifyCredentials))
	h("PATCH /api/v1/accounts/update_credentials", write(app.apiUpdateCredentials))
	h("GET /api/v1/accounts/relationships", read(app.apiRelationships))
	h("GET /api/v1/accounts/familiar_followers", read(emptyList))
	h("GET /api/v1/accounts/lookup", pub(app.apiLookup))
	h("GET /api/v1/accounts/search", read(app.apiAccountSearch))
	h("GET /api/v1/accounts/{id}", pub(app.apiGetAccount))
	h("GET /api/v1/accounts/{id}/statuses", pub(app.apiAccountStatuses))
	h("GET /api/v1/accounts/{id}/followers", pub(app.apiFollowList(true)))
	h("GET /api/v1/accounts/{id}/following", pub(app.apiFollowList(false)))
	h("GET /api/v1/accounts/{id}/featured_tags", pub(emptyList))
	h("GET /api/v1/accounts/{id}/lists", read(app.apiAccountLists))
	h("GET /api/v1/accounts/{id}/identity_proofs", pub(emptyList))
	h("POST /api/v1/accounts/{id}/{action}", follow(app.apiAccountAction))
	h("GET /api/v1/blocks", read(app.apiBlocksOrMutes("blocks")))
	h("GET /api/v1/mutes", read(app.apiBlocksOrMutes("mutes")))
	h("GET /api/v1/follow_requests", read(app.apiFollowRequests))
	h("POST /api/v1/follow_requests/{id}/{action}", follow(app.apiFollowRequestAction))
	h("GET /api/v1/bookmarks", read(app.apiBookmarks))
	h("GET /api/v1/favourites", read(app.apiFavourites))

	h("POST /api/v1/statuses", write(app.apiPostStatus))
	h("GET /api/v1/statuses/{id}", pub(app.apiGetStatus))
	h("PUT /api/v1/statuses/{id}", write(app.apiEditStatus))
	h("DELETE /api/v1/statuses/{id}", write(app.apiDeleteStatus))
	h("GET /api/v1/statuses/{id}/context", pub(app.apiContext))
	h("GET /api/v1/statuses/{id}/source", read(app.apiStatusSource))
	h("GET /api/v1/statuses/{id}/history", pub(app.apiStatusHistory))
	h("GET /api/v1/statuses/{id}/favourited_by", pub(app.apiStatusAccounts(true)))
	h("GET /api/v1/statuses/{id}/reblogged_by", pub(app.apiStatusAccounts(false)))
	h("POST /api/v1/statuses/{id}/{action}", write(app.apiStatusAction))
	h("PUT /api/v1/pleroma/statuses/{id}/reactions/{emoji}", write(app.apiReaction))
	h("DELETE /api/v1/pleroma/statuses/{id}/reactions/{emoji}", write(app.apiReaction))
	h("GET /api/v1/polls/{id}", pub(app.apiGetPoll))
	h("POST /api/v1/polls/{id}/votes", write(app.apiVote))

	h("POST /api/v1/media", write(app.apiUploadMedia))
	h("POST /api/v2/media", write(app.apiUploadMedia))
	h("GET /api/v1/media/{id}", write(app.apiMediaByID))
	h("PUT /api/v1/media/{id}", write(app.apiMediaByID))

	h("GET /api/v1/timelines/home", read(app.apiHome))
	h("GET /api/v1/timelines/public", pub(app.apiPublic))
	h("GET /api/v1/timelines/tag/{tag}", pub(app.apiTagTimeline))
	h("GET /api/v1/timelines/list/{id}", read(app.apiListTimeline))

	h("GET /api/v1/notifications", read(app.apiNotifications))
	h("GET /api/v1/notifications/unread_count", read(app.apiUnreadCount))
	h("POST /api/v1/notifications/clear", write(app.apiClearNotifications))
	h("GET /api/v1/notifications/{id}", read(app.apiNotification1))
	h("POST /api/v1/notifications/{id}/dismiss", write(app.apiDismissNotification))

	h("GET /api/v2/search", pub(app.apiSearch))
	h("GET /api/v1/search", pub(app.apiSearch))

	push := func(f apiHandler) http.HandlerFunc { return app.api("push", true, f) }
	h("POST /api/v1/push/subscription", push(app.apiPushSubscribe))
	h("GET /api/v1/push/subscription", push(app.apiPushGet))
	h("PUT /api/v1/push/subscription", push(app.apiPushUpdate))
	h("DELETE /api/v1/push/subscription", push(app.apiPushDelete))
	h("GET /api/v1/streaming/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("OK")) })
	h("GET /api/v1/streaming", app.handleStreaming)
	h("GET /api/v1/streaming/{stream...}", app.handleStreaming)

}
