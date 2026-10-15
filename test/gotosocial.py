#!/usr/bin/env python3
# Federation interop check against a real GoToSocial instance, driven over both
# servers' Mastodon client APIs. Stdlib only.
#
# Setup (see the README, "Testing against GoToSocial"):
#   - a GoToSocial on https://localhost:8831 with a confirmed user gal
#   - a trough dev instance on https://127.0.0.1:8832 (-insecure) with user bob
#   python3 test/gotosocial.py https://127.0.0.1:8832 bob bobpassword12 \
#       https://localhost:8831 gal@example.org 'GalPassword123!!'
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


def trough_token(base, user, pw):
    a = API(base)
    app = a.post("/api/v1/apps", {"client_name": "gts-interop", "redirect_uris": OOB, "scopes": SCOPES})
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
    app = API(base).post("/api/v1/apps", {"client_name": "gts-interop", "redirect_uris": OOB, "scopes": SCOPES})
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
    tbase, tuser, tpw, gbase, gemail, gpw = sys.argv[1:7]
    t = API(tbase, trough_token(tbase, tuser, tpw))
    g = API(gbase, gts_token(gbase, gemail, gpw))
    bob, gal = t.get("/api/v1/accounts/verify_credentials"), g.get("/api/v1/accounts/verify_credentials")
    bob_acct = f"{bob['username']}@{urllib.parse.urlparse(tbase).netloc}"
    gal_acct = f"{gal['username']}@{urllib.parse.urlparse(gbase).netloc}"
    stamp = str(int(time.time()))
    print("trough:", bob_acct, "| gotosocial:", gal_acct, g.get("/api/v1/instance")["version"])

    print("1. trough user follows the GoToSocial user")
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
    wait("GoToSocial lists the follower", lambda: any(a["acct"] == bob_acct for a in g.get(f"/api/v1/accounts/{gal['id']}/followers")))

    print("2. GoToSocial post reaches trough's home timeline")
    gpost = g.post("/api/v1/statuses", {"status": f"hello from gotosocial {stamp} #interop", "visibility": "public"})
    tpost = wait("post delivered", lambda: next((s for s in t.get("/api/v1/timelines/home") if s["uri"] == gpost["uri"]), None))
    assert tpost["tags"] and tpost["tags"][0]["name"] == "interop", tpost["tags"]

    print("3. GoToSocial user follows back; trough mention reaches GoToSocial")
    bob_on_g = resolve_account(g, bob_acct)
    if g.get(f"/api/v1/accounts/relationships?id[]={bob_on_g['id']}")[0]["following"]:
        g.post(f"/api/v1/accounts/{bob_on_g['id']}/unfollow")
        wait("old follow removed", lambda: not any(a["acct"] == gal_acct for a in t.get(f"/api/v1/accounts/{bob['id']}/followers")))
    g.post(f"/api/v1/accounts/{bob_on_g['id']}/follow")
    wait("follow accepted", lambda: g.get(f"/api/v1/accounts/relationships?id[]={bob_on_g['id']}")[0]["following"])
    tmention = t.post("/api/v1/statuses", {"status": f"@{gal_acct} hi from trough {stamp}", "visibility": "public"})
    wait("mention notification", lambda: any(n["status"]["uri"] == tmention["uri"] for n in notifications(g, "mention")))
    gm = wait("post in GoToSocial home", lambda: next((s for s in g.get("/api/v1/timelines/home") if s["uri"] == tmention["uri"]), None))
    assert any(m["acct"] == gal["username"] for m in gm["mentions"]), gm["mentions"]

    print("4. likes, boosts and replies from GoToSocial")
    g.post(f"/api/v1/statuses/{gm['id']}/favourite")
    g.post(f"/api/v1/statuses/{gm['id']}/reblog")
    greply = g.post("/api/v1/statuses", {"status": f"@{bob_acct} a reply from gotosocial", "in_reply_to_id": gm["id"], "visibility": "public"})
    wait("favourite notification", lambda: any(n["status"]["id"] == tmention["id"] for n in notifications(t, "favourite")))
    wait("reblog notification", lambda: any(n["status"]["id"] == tmention["id"] for n in notifications(t, "reblog")))
    wait("reply in thread", lambda: any(s["uri"] == greply["uri"] for s in t.get(f"/api/v1/statuses/{tmention['id']}/context")["descendants"]))
    g.post(f"/api/v1/statuses/{gm['id']}/unfavourite")
    wait("unfavourite federated", lambda: t.get(f"/api/v1/statuses/{tmention['id']}")["favourites_count"] == 0)

    print("5. trough likes, boosts and replies to the GoToSocial post")
    t.post(f"/api/v1/statuses/{tpost['id']}/favourite")
    t.post(f"/api/v1/statuses/{tpost['id']}/reblog")
    treply = t.post("/api/v1/statuses", {"status": f"@{gal_acct} nice", "in_reply_to_id": tpost["id"], "visibility": "public"})
    wait("favourite notification", lambda: any(n["status"]["uri"] == gpost["uri"] for n in notifications(g, "favourite")))
    wait("reblog notification", lambda: any(n["status"]["uri"] == gpost["uri"] for n in notifications(g, "reblog")))
    wait("reply in thread", lambda: any(s["uri"] == treply["uri"] for s in g.get(f"/api/v1/statuses/{gpost['id']}/context")["descendants"]))

    print("6. edits in both directions")
    t.call("PUT", f"/api/v1/statuses/{tmention['id']}", {"status": f"@{gal_acct} edited on trough {stamp}"})
    wait("trough edit visible on GoToSocial", lambda: "edited on trough" in text(g.get(f"/api/v1/statuses/{gm['id']}")))
    g.call("PUT", f"/api/v1/statuses/{gpost['id']}", {"status": f"edited on gotosocial {stamp}"})
    wait("GoToSocial edit visible on trough", lambda: "edited on gotosocial" in text(t.get(f"/api/v1/statuses/{tpost['id']}")))
    assert len(t.get(f"/api/v1/statuses/{tpost['id']}/history")) == 2

    print("7. polls")
    tpoll = t.post("/api/v1/statuses", {"status": f"trough poll {stamp}", "poll[options][]": ["yes", "no"], "poll[expires_in]": "3600", "visibility": "public"})
    gp = wait("poll on GoToSocial", lambda: resolve_status(g, tpoll["uri"]))
    g.post(f"/api/v1/polls/{gp['poll']['id']}/votes", {"choices[]": ["0"]})
    wait("vote counted on trough", lambda: t.get(f"/api/v1/polls/{tpoll['poll']['id']}")["options"][0]["votes_count"] == 1)
    gpoll = g.post("/api/v1/statuses", {"status": f"gotosocial poll {stamp}", "poll[options][]": ["red", "blue"], "poll[expires_in]": "3600", "visibility": "public"})
    tp = wait("poll on trough", lambda: next((s for s in t.get("/api/v1/timelines/home") if s["uri"] == gpoll["uri"]), None))
    t.post(f"/api/v1/polls/{tp['poll']['id']}/votes", {"choices[]": ["1"]})
    wait("vote counted on GoToSocial", lambda: g.get(f"/api/v1/polls/{gpoll['poll']['id']}")["options"][1]["votes_count"] == 1)

    print("8. followers-only and direct posts")
    tpriv = t.post("/api/v1/statuses", {"status": f"followers only {stamp}", "visibility": "private"})
    wait("followers-only post delivered", lambda: any(s["uri"] == tpriv["uri"] and s["visibility"] == "private" for s in g.get("/api/v1/timelines/home")))
    tdm = t.post("/api/v1/statuses", {"status": f"@{gal_acct} psst {stamp}", "visibility": "direct"})
    wait("direct message delivered", lambda: any(n["status"]["uri"] == tdm["uri"] and n["status"]["visibility"] == "direct" for n in notifications(g, "mention")))

    print("9. deletes in both directions")
    gone = lambda api, sid: lambda: api.call("GET", f"/api/v1/statuses/{sid}", ok=(200, 404)).get("error") is not None
    gpriv = next(s for s in g.get("/api/v1/timelines/home") if s["uri"] == tpriv["uri"])
    t.call("DELETE", f"/api/v1/statuses/{tpriv['id']}")
    wait("trough delete reached GoToSocial", gone(g, gpriv["id"]))
    # A post that has replies: GoToSocial 0.22.1 keeps a stub to preserve the
    # thread, and its stubbing fails ("NOT NULL constraint failed:
    # statuses.thread_id") for remote posts here, so only report this one.
    t.call("DELETE", f"/api/v1/statuses/{tmention['id']}")
    try:
        wait("trough delete of a post with replies reached GoToSocial", gone(g, gm["id"]), timeout=10)
    except AssertionError as e:
        print("  WARN (GoToSocial side):", e)
    g.call("DELETE", f"/api/v1/statuses/{greply['id']}")
    g.call("DELETE", f"/api/v1/statuses/{gpost['id']}")
    wait("GoToSocial delete reached trough", gone(t, tpost["id"]))

    print("10. profile updates")
    t.call("PATCH", "/api/v1/accounts/update_credentials", {"note": f"bio {stamp}"})
    wait("profile update on GoToSocial", lambda: f"bio {stamp}" in g.get(f"/api/v1/accounts/{bob_on_g['id']}")["note"])

    print("11. unfollow both ways")
    t.post(f"/api/v1/accounts/{gal_on_t['id']}/unfollow")
    g.post(f"/api/v1/accounts/{bob_on_g['id']}/unfollow")
    wait("GoToSocial dropped the follower", lambda: not any(a["acct"] == bob_acct for a in g.get(f"/api/v1/accounts/{gal['id']}/followers")))
    wait("trough dropped the follower", lambda: not any(a["acct"] == gal_acct for a in t.get(f"/api/v1/accounts/{bob['id']}/followers")))
    print("all GoToSocial interop checks passed")


if __name__ == "__main__":
    main()
