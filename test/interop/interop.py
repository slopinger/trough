#!/usr/bin/env python3
# Federation interop check between trough and another real server (GoToSocial,
# Mastodon, ...), driven over both servers' Mastodon client APIs. Stdlib only.
#
#   python3 test/interop/interop.py <trough-base> <user> <password> <remote-base> <remote-email> <remote-password>
#   python3 test/interop/interop.py <trough-base> <user> <password> <remote-base> --token <remote-token>
#   python3 test/interop/interop.py <trough-base> <user> <password> <remote-base> --password <remote-user> <remote-password>
#
# The first form signs in through GoToSocial's web sign-in, the last uses the
# OAuth password grant. test/interop/run-{gotosocial,mastodon,akkoma}.py set everything
# up and call this.
import json, re, sys, time, urllib.error, urllib.parse, urllib.request, http.cookiejar

OOB = "urn:ietf:wg:oauth:2.0:oob"
SCOPES = "read write follow"


class API:
    def __init__(self, base, token=None):
        self.base, self.token = base.rstrip("/"), token

    def call(self, method, path, data=None, ok=(200,)):
        body = urllib.parse.urlencode(data, doseq=True).encode() if data is not None else None
        r = urllib.request.Request(self.base + path, data=body, method=method)
        if self.token:
            r.add_header("Authorization", "Bearer " + self.token)
        try:
            with urllib.request.urlopen(r, timeout=30) as resp:
                code, raw = resp.status, resp.read()
        except urllib.error.HTTPError as e:
            code, raw = e.code, e.read()
        if code not in ok:
            raise AssertionError(f"{method} {self.base}{path} -> {code}: {raw[:300]!r}")
        return json.loads(raw) if raw else None

    get = lambda self, p, **kw: self.call("GET", p, **kw)
    post = lambda self, p, d=None, **kw: self.call("POST", p, d if d is not None else {}, **kw)


def password_token(base, user, pw):
    a = API(base)
    app = a.post("/api/v1/apps", {"client_name": "trough-interop", "redirect_uris": OOB, "scopes": SCOPES})
    t = a.post("/oauth/token", {"grant_type": "password", "username": user, "password": pw, "scope": SCOPES,
                                "client_id": app["client_id"], "client_secret": app["client_secret"]})
    return t["access_token"]


def gts_token(base, email, pw):
    # GoToSocial has no password grant: go through its sign-in and consent pages
    op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
    def req(method, path, data=None):
        r = urllib.request.Request(base + path, data=urllib.parse.urlencode(data).encode() if data is not None else None, method=method)
        with op.open(r, timeout=30) as resp:
            return resp.geturl(), resp.read().decode()
    app = API(base).post("/api/v1/apps", {"client_name": "trough-interop", "redirect_uris": OOB, "scopes": SCOPES})
    req("GET", "/oauth/authorize?" + urllib.parse.urlencode(
        {"client_id": app["client_id"], "redirect_uri": OOB, "response_type": "code", "scope": SCOPES}))
    url, _ = req("POST", "/auth/sign_in", {"username": email, "password": pw})
    if "/oauth/authorize" in url:
        url, _ = req("POST", "/oauth/authorize", {})
    code = re.search(r"code=([A-Za-z0-9_-]+)", url).group(1)
    t = API(base).post("/oauth/token", {"grant_type": "authorization_code", "code": code, "redirect_uri": OOB,
                                        "client_id": app["client_id"], "client_secret": app["client_secret"]})
    return t["access_token"]


def wait(what, fn, timeout=30):
    end = time.time() + timeout
    while True:
        try:
            v = fn()
        except AssertionError:
            v = None
        if v:
            print("  ok:", what)
            return v
        if time.time() > end:
            raise AssertionError("timed out: " + what)
        time.sleep(0.5)


def resolve_account(api, acct):
    r = api.get("/api/v2/search?" + urllib.parse.urlencode({"q": "@" + acct, "resolve": "true", "type": "accounts"}))
    assert r["accounts"], f"could not resolve {acct}"
    return r["accounts"][0]


def resolve_status(api, uri):
    r = api.get("/api/v2/search?" + urllib.parse.urlencode({"q": uri, "resolve": "true", "type": "statuses"}))
    return r["statuses"][0] if r["statuses"] else None


def notifications(api, typ):
    return [n for n in api.get("/api/v1/notifications?limit=40") if n["type"] == typ]


def text(s):
    return re.sub("<[^>]+>", "", s["content"])


def main():
    tbase, tuser, tpw, gbase = sys.argv[1:5]
    t = API(tbase, password_token(tbase, tuser, tpw))
    if sys.argv[5] == "--token":
        g = API(gbase, sys.argv[6])
    elif sys.argv[5] == "--password":  # servers with the password grant (Pleroma, Akkoma)
        g = API(gbase, password_token(gbase, sys.argv[6], sys.argv[7]))
    else:
        g = API(gbase, gts_token(gbase, sys.argv[5], sys.argv[6]))
    remote = g.get("/api/v1/instance")
    api_version = (g.call("GET", "/api/v2/instance", ok=(200, 404)).get("api_versions") or {}).get("mastodon", 0)
    bob, gal = t.get("/api/v1/accounts/verify_credentials"), g.get("/api/v1/accounts/verify_credentials")
    bob_acct = f"{bob['username']}@{urllib.parse.urlparse(tbase).netloc}"
    gal_acct = f"{gal['username']}@{urllib.parse.urlparse(gbase).netloc}"
    stamp = str(int(time.time()))
    print("trough:", bob_acct, "| remote:", gal_acct, remote["version"])

    print("1. trough user follows the remote user")
    gal_on_t = resolve_account(t, gal_acct)
    # start from a clean slate when re-run
    if t.get(f"/api/v1/accounts/relationships?id[]={gal_on_t['id']}")[0]["following"]:
        t.post(f"/api/v1/accounts/{gal_on_t['id']}/unfollow")
        wait("old follow removed", lambda: not any(a["acct"] == bob_acct for a in g.get(f"/api/v1/accounts/{gal['id']}/followers")))
    t.post(f"/api/v1/accounts/{gal_on_t['id']}/follow")
    if gal["locked"]:  # GoToSocial accounts approve followers by default
        req = wait("follow request arrived", lambda: next((a for a in g.get("/api/v1/follow_requests") if a["acct"] == bob_acct), None))
        g.post(f"/api/v1/follow_requests/{req['id']}/authorize")
    wait("follow accepted", lambda: t.get(f"/api/v1/accounts/relationships?id[]={gal_on_t['id']}")[0]["following"])
    wait("remote lists the follower", lambda: any(a["acct"] == bob_acct for a in g.get(f"/api/v1/accounts/{gal['id']}/followers")))

    print("2. remote post reaches trough's home timeline")
    gpost = g.post("/api/v1/statuses", {"status": f"hello from the remote server {stamp} #interop", "visibility": "public"})
    tpost = wait("post delivered", lambda: next((s for s in t.get("/api/v1/timelines/home") if s["uri"] == gpost["uri"]), None))
    assert tpost["tags"] and tpost["tags"][0]["name"] == "interop", tpost["tags"]

    print("3. remote user follows back; trough mention reaches remote")
    bob_on_g = resolve_account(g, bob_acct)
    if g.get(f"/api/v1/accounts/relationships?id[]={bob_on_g['id']}")[0]["following"]:
        g.post(f"/api/v1/accounts/{bob_on_g['id']}/unfollow")
        wait("old follow removed", lambda: not any(a["acct"] == gal_acct for a in t.get(f"/api/v1/accounts/{bob['id']}/followers")))
    g.post(f"/api/v1/accounts/{bob_on_g['id']}/follow")
    wait("follow accepted", lambda: g.get(f"/api/v1/accounts/relationships?id[]={bob_on_g['id']}")[0]["following"])
    tmention = t.post("/api/v1/statuses", {"status": f"@{gal_acct} hi from trough {stamp}", "visibility": "public"})
    wait("mention notification", lambda: any(n["status"]["uri"] == tmention["uri"] for n in notifications(g, "mention")))
    gm = wait("post in remote home", lambda: next((s for s in g.get("/api/v1/timelines/home") if s["uri"] == tmention["uri"]), None))
    assert any(m["acct"] == gal["username"] for m in gm["mentions"]), gm["mentions"]

    print("4. likes, boosts and replies from remote")
    g.post(f"/api/v1/statuses/{gm['id']}/favourite")
    g.post(f"/api/v1/statuses/{gm['id']}/reblog")
    greply = g.post("/api/v1/statuses", {"status": f"@{bob_acct} a reply from the remote server", "in_reply_to_id": gm["id"], "visibility": "public"})
    wait("favourite notification", lambda: any(n["status"]["id"] == tmention["id"] for n in notifications(t, "favourite")))
    wait("reblog notification", lambda: any(n["status"]["id"] == tmention["id"] for n in notifications(t, "reblog")))
    wait("reply in thread", lambda: any(s["uri"] == greply["uri"] for s in t.get(f"/api/v1/statuses/{tmention['id']}/context")["descendants"]))
    g.post(f"/api/v1/statuses/{gm['id']}/unfavourite")
    wait("unfavourite federated", lambda: t.get(f"/api/v1/statuses/{tmention['id']}")["favourites_count"] == 0)

    print("5. trough likes, boosts and replies to the remote post")
    t.post(f"/api/v1/statuses/{tpost['id']}/favourite")
    t.post(f"/api/v1/statuses/{tpost['id']}/reblog")
    treply = t.post("/api/v1/statuses", {"status": f"@{gal_acct} nice", "in_reply_to_id": tpost["id"], "visibility": "public"})
    wait("favourite notification", lambda: any(n["status"]["uri"] == gpost["uri"] for n in notifications(g, "favourite")))
    wait("reblog notification", lambda: any(n["status"]["uri"] == gpost["uri"] for n in notifications(g, "reblog")))
    wait("reply in thread", lambda: any(s["uri"] == treply["uri"] for s in g.get(f"/api/v1/statuses/{gpost['id']}/context")["descendants"]))

    print("6. edits in both directions")
    t.call("PUT", f"/api/v1/statuses/{tmention['id']}", {"status": f"@{gal_acct} edited on trough {stamp}"})
    wait("trough edit visible on remote", lambda: "edited on trough" in text(g.get(f"/api/v1/statuses/{gm['id']}")))
    g.call("PUT", f"/api/v1/statuses/{gpost['id']}", {"status": f"edited remotely {stamp}"})
    wait("remote edit visible on trough", lambda: "edited remotely" in text(t.get(f"/api/v1/statuses/{tpost['id']}")))
    assert len(t.get(f"/api/v1/statuses/{tpost['id']}/history")) == 2

    print("7. polls")
    tpoll = t.post("/api/v1/statuses", {"status": f"trough poll {stamp}", "poll[options][]": ["yes", "no"], "poll[expires_in]": "3600", "visibility": "public"})
    gp = wait("poll on remote", lambda: resolve_status(g, tpoll["uri"]))
    g.post(f"/api/v1/polls/{gp['poll']['id']}/votes", {"choices[]": ["0"]})
    wait("vote counted on trough", lambda: t.get(f"/api/v1/polls/{tpoll['poll']['id']}")["options"][0]["votes_count"] == 1)
    gpoll = g.post("/api/v1/statuses", {"status": f"remote poll {stamp}", "poll[options][]": ["red", "blue"], "poll[expires_in]": "3600", "visibility": "public"})
    tp = wait("poll on trough", lambda: next((s for s in t.get("/api/v1/timelines/home") if s["uri"] == gpoll["uri"]), None))
    t.post(f"/api/v1/polls/{tp['poll']['id']}/votes", {"choices[]": ["1"]})
    wait("vote counted on remote", lambda: g.get(f"/api/v1/polls/{gpoll['poll']['id']}")["options"][1]["votes_count"] == 1)

    if api_version >= 7:  # Mastodon 4.5+ quote posts (FEP-044f)
        print("7b. quotes in both directions, with approval (FEP-044f)")
        quote_of = lambda api, sid: (api.get(f"/api/v1/statuses/{sid}").get("quote") or {})
        gq = g.post("/api/v1/statuses", {"status": f"quoting trough {stamp}", "quoted_status_id": gm["id"], "visibility": "public"})
        tq = wait("remote quote on trough", lambda: resolve_status(t, gq["uri"]))
        wait("remote quote approved by trough", lambda: quote_of(g, gq["id"]).get("state") == "accepted")
        wait("remote quote shown on trough", lambda: (quote_of(t, tq["id"]).get("quoted_status") or {}).get("id") == tmention["id"])
        tq2 = t.post("/api/v1/statuses", {"status": f"quoting remote {stamp}", "quoted_status_id": tpost["id"], "visibility": "public"})
        gq2 = wait("trough quote on remote", lambda: resolve_status(g, tq2["uri"]))
        wait("trough quote approved by remote", lambda: quote_of(t, tq2["id"]).get("state") == "accepted")
        wait("trough quote shown on remote", lambda: (quote_of(g, gq2["id"]).get("quoted_status") or {}).get("uri") == gpost["uri"])
        t.post(f"/api/v1/statuses/{tmention['id']}/quotes/{tq['id']}/revoke")
        wait("revoke reached remote", lambda: quote_of(g, gq["id"]).get("state") == "revoked")

    print("8. followers-only and direct posts")
    tpriv = t.post("/api/v1/statuses", {"status": f"followers only {stamp}", "visibility": "private"})
    wait("followers-only post delivered", lambda: any(s["uri"] == tpriv["uri"] and s["visibility"] == "private" for s in g.get("/api/v1/timelines/home")))
    tdm = t.post("/api/v1/statuses", {"status": f"@{gal_acct} psst {stamp}", "visibility": "direct"})
    wait("direct message delivered", lambda: any(n["status"]["uri"] == tdm["uri"] and n["status"]["visibility"] == "direct" for n in notifications(g, "mention")))

    print("9. deletes in both directions")
    gone = lambda api, sid: lambda: api.call("GET", f"/api/v1/statuses/{sid}", ok=(200, 404)).get("error") is not None
    gpriv = next(s for s in g.get("/api/v1/timelines/home") if s["uri"] == tpriv["uri"])
    t.call("DELETE", f"/api/v1/statuses/{tpriv['id']}")
    wait("trough delete reached remote", gone(g, gpriv["id"]))
    # A post that has replies: GoToSocial 0.22.1 keeps a stub to preserve the
    # thread, and its stubbing fails ("NOT NULL constraint failed:
    # statuses.thread_id") for remote posts there, so only report this one.
    t.call("DELETE", f"/api/v1/statuses/{tmention['id']}")
    try:
        wait("trough delete of a post with replies reached remote", gone(g, gm["id"]), timeout=10)
    except AssertionError as e:
        print("  WARN (remote side):", e)
    g.call("DELETE", f"/api/v1/statuses/{greply['id']}")
    g.call("DELETE", f"/api/v1/statuses/{gpost['id']}")
    wait("remote delete reached trough", gone(t, tpost["id"]))

    print("10. profile updates")
    t.call("PATCH", "/api/v1/accounts/update_credentials", {"note": f"bio {stamp}"})
    wait("profile update on remote", lambda: f"bio {stamp}" in g.get(f"/api/v1/accounts/{bob_on_g['id']}")["note"])

    print("11. unfollow both ways")
    t.post(f"/api/v1/accounts/{gal_on_t['id']}/unfollow")
    g.post(f"/api/v1/accounts/{bob_on_g['id']}/unfollow")
    wait("remote dropped the follower", lambda: not any(a["acct"] == bob_acct for a in g.get(f"/api/v1/accounts/{gal['id']}/followers")))
    wait("trough dropped the follower", lambda: not any(a["acct"] == gal_acct for a in t.get(f"/api/v1/accounts/{bob['id']}/followers")))
    print("all interop checks passed against", remote["version"])


if __name__ == "__main__":
    main()
