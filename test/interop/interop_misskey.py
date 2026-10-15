#!/usr/bin/env python3
# Federation interop check between trough and a real Misskey (or a fork that
# keeps its API, like Sharkey), driven over trough's Mastodon client API and
# Misskey's own API. Stdlib only.
#
#   python3 test/interop/interop_misskey.py <trough-base> <user> <password> <misskey-base> <misskey-token>
#
# test/interop/run-misskey.py sets everything up and calls this. Misskey has no note
# editing and ignores remote edits, so that check only warns (Sharkey applies them).
import json
from pathlib import Path
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

sys.path.insert(0, str(Path(__file__).resolve().parent))
from interop import API, notifications, password_token, resolve_account, resolve_status, text, wait  # noqa: E402


class Misskey:
    def __init__(self, base, token):
        self.base, self.token = base.rstrip("/"), token

    def __call__(self, endpoint, ok=(200, 204), **data):
        r = urllib.request.Request(f"{self.base}/api/{endpoint}", data=json.dumps({"i": self.token, **data}).encode(),
                                   headers={"Content-Type": "application/json"}, method="POST")
        try:
            with urllib.request.urlopen(r, timeout=30) as resp:
                code, raw = resp.status, resp.read()
        except urllib.error.HTTPError as e:
            code, raw = e.code, e.read()
        if code not in ok:
            raise AssertionError(f"{endpoint} -> {code}: {raw[:300]!r}")
        return json.loads(raw) if raw else None


def main():
    tbase, tuser, tpw, mbase, mtoken = sys.argv[1:6]
    t, m = API(tbase, password_token(tbase, tuser, tpw)), Misskey(mbase, mtoken)
    with urllib.request.urlopen(f"{mbase}/nodeinfo/2.1", timeout=30) as r:
        software = json.load(r)["software"]  # misskey, sharkey, ...
    name = f"{software['name']} {software['version']}"
    bob, mia = t.get("/api/v1/accounts/verify_credentials"), m("i")
    thost, mhost = urllib.parse.urlparse(tbase).netloc, urllib.parse.urlparse(mbase).netloc
    bob_acct, mia_acct = f"{bob['username']}@{thost}", f"{mia['username']}@{mhost}"
    note_uri = lambda n: n.get("uri") or f"{mbase}/notes/{n['id']}"
    mnotifs = lambda typ: [n for n in m("i/notifications", limit=50) if n["type"] == typ]
    in_home = lambda uri: next((s for s in t.get("/api/v1/timelines/home") if s["uri"] == uri), None)
    stamp = str(int(time.time()))
    print("trough:", bob_acct, "| remote:", mia_acct, name)

    print("1. trough user follows the Misskey user")
    mia_on_t = resolve_account(t, mia_acct)
    t.post(f"/api/v1/accounts/{mia_on_t['id']}/follow")
    wait("follow accepted", lambda: t.get(f"/api/v1/accounts/relationships?id[]={mia_on_t['id']}")[0]["following"])
    wait("Misskey lists the follower", lambda: any(f["follower"]["username"] == bob["username"] for f in m("users/followers", userId=mia["id"])))

    print("2. Misskey note reaches trough's home timeline")
    mpost = m("notes/create", text=f"hello from misskey {stamp} #interop **bold**")["createdNote"]
    tpost = wait("note delivered", lambda: in_home(note_uri(mpost)))
    assert tpost["tags"] and tpost["tags"][0]["name"] == "interop", tpost["tags"]
    assert "hello from misskey" in text(tpost), tpost["content"]

    print("3. Misskey user follows back; mentions both ways")
    bob_on_m = m("users/show", username=bob["username"], host=thost)
    m("following/create", userId=bob_on_m["id"])
    wait("follow accepted", lambda: any(a["acct"] == mia_acct for a in t.get(f"/api/v1/accounts/{bob['id']}/followers")))
    tmention = t.post("/api/v1/statuses", {"status": f"@{mia_acct} hi from trough {stamp}", "visibility": "public"})
    wait("mention notification on Misskey", lambda: any(note_uri(n["note"]) == tmention["uri"] for n in mnotifs("mention")))
    mm = wait("trough post in Misskey home", lambda: next((n for n in m("notes/timeline", limit=30) if n.get("uri") == tmention["uri"]), None))
    assert mia["id"] in (mm.get("mentions") or []), mm.get("mentions")
    mmention = m("notes/create", text=f"@{bob_acct} hi from misskey {stamp}")["createdNote"]
    wait("mention notification on trough", lambda: any(n["status"]["uri"] == note_uri(mmention) for n in notifications(t, "mention")))

    print("4. reactions, renotes, quotes and replies from Misskey")
    m("notes/reactions/create", noteId=mm["id"], reaction="👍")
    m("notes/create", renoteId=mm["id"])
    mquote = m("notes/create", renoteId=mm["id"], text=f"quoting trough {stamp}")["createdNote"]
    mreply = m("notes/create", replyId=mm["id"], text=f"@{bob_acct} a reply from misskey")["createdNote"]
    wait("emoji reaction on trough", lambda: any(r["name"] == "👍" and r["count"] == 1
                                                 for r in t.get(f"/api/v1/statuses/{tmention['id']}")["pleroma"]["emoji_reactions"]))
    wait("reblog notification", lambda: any(n["status"]["id"] == tmention["id"] for n in notifications(t, "reblog")))
    tq = wait("quote arrived", lambda: resolve_status(t, note_uri(mquote)))
    assert (tq.get("quote") or {}).get("quoted_status", {}).get("id") == tmention["id"], tq.get("quote")
    wait("reply in thread", lambda: any(s["uri"] == note_uri(mreply) for s in t.get(f"/api/v1/statuses/{tmention['id']}/context")["descendants"]))
    m("notes/reactions/delete", noteId=mm["id"])
    wait("reaction removed on trough", lambda: not t.get(f"/api/v1/statuses/{tmention['id']}")["pleroma"]["emoji_reactions"])

    print("5. trough likes, reacts, boosts, quotes and replies to the Misskey note")
    t.post(f"/api/v1/statuses/{tpost['id']}/favourite")
    wait("like shows as a reaction", lambda: any(n["note"]["id"] == mpost["id"] for n in mnotifs("reaction")))
    t.post(f"/api/v1/statuses/{tpost['id']}/unfavourite")
    wait("unlike federated", lambda: not m("notes/show", noteId=mpost["id"])["reactions"])
    t.call("PUT", f"/api/v1/pleroma/statuses/{tpost['id']}/reactions/{urllib.parse.quote('🎉')}")
    wait("emoji reaction on Misskey", lambda: m("notes/show", noteId=mpost["id"])["reactions"].get("🎉") == 1)
    t.post(f"/api/v1/statuses/{tpost['id']}/reblog")
    wait("renote notification", lambda: any(n["note"]["renoteId"] == mpost["id"] for n in mnotifs("renote")))
    tquote = t.post("/api/v1/statuses", {"status": f"quoting misskey {stamp}", "quoted_status_id": tpost["id"], "visibility": "public"})
    wait("quote notification", lambda: any(n["note"].get("uri") == tquote["uri"] and n["note"]["renoteId"] == mpost["id"] for n in mnotifs("quote")))
    treply = t.post("/api/v1/statuses", {"status": f"@{mia_acct} nice", "in_reply_to_id": tpost["id"], "visibility": "public"})
    wait("reply in thread", lambda: any(n.get("uri") == treply["uri"] for n in m("notes/children", noteId=mpost["id"])))

    print("6. trough edits")
    # Misskey (unlike its forks) ignores Update(Note) ("Unknown type: Note"), so only report it
    t.call("PUT", f"/api/v1/statuses/{tmention['id']}", {"status": f"@{mia_acct} edited on trough {stamp}"})
    try:
        wait("trough edit visible on Misskey", lambda: "edited on trough" in (m("notes/show", noteId=mm["id"])["text"] or ""), timeout=10)
    except AssertionError as e:
        print("  WARN (Misskey does not apply remote edits):", e)

    print("7. polls")
    tpoll = t.post("/api/v1/statuses", {"status": f"trough poll {stamp}", "poll[options][]": ["yes", "no"], "poll[expires_in]": "3600", "visibility": "public"})
    mp = wait("poll on Misskey", lambda: m("ap/show", uri=tpoll["uri"])["object"])
    m("notes/polls/vote", noteId=mp["id"], choice=0)
    wait("vote counted on trough", lambda: t.get(f"/api/v1/polls/{tpoll['poll']['id']}")["options"][0]["votes_count"] == 1)
    mpoll = m("notes/create", text=f"misskey poll {stamp}", poll={"choices": ["red", "blue"], "expiredAfter": 3600000})["createdNote"]
    tp = wait("poll on trough", lambda: in_home(note_uri(mpoll)))
    t.post(f"/api/v1/polls/{tp['poll']['id']}/votes", {"choices[]": ["1"]})
    wait("vote counted on Misskey", lambda: m("notes/show", noteId=mpoll["id"])["poll"]["choices"][1]["votes"] == 1)

    print("8. followers-only and direct posts both ways")
    tpriv = t.post("/api/v1/statuses", {"status": f"followers only {stamp}", "visibility": "private"})
    mpriv = wait("followers-only post on Misskey", lambda: next((n for n in m("notes/timeline", limit=30)
                                                                 if n.get("uri") == tpriv["uri"] and n["visibility"] == "followers"), None))
    tdm = t.post("/api/v1/statuses", {"status": f"@{mia_acct} psst {stamp}", "visibility": "direct"})
    wait("direct message on Misskey", lambda: any(note_uri(n["note"]) == tdm["uri"] and n["note"]["visibility"] == "specified" for n in mnotifs("mention")))
    mfo = m("notes/create", text=f"misskey followers only {stamp}", visibility="followers")["createdNote"]
    wait("followers-only note on trough", lambda: (in_home(note_uri(mfo)) or {}).get("visibility") == "private")
    mdm = m("notes/create", text=f"@{bob_acct} psst from misskey {stamp}", visibility="specified", visibleUserIds=[bob_on_m["id"]])["createdNote"]
    wait("direct note on trough", lambda: any(n["status"]["uri"] == note_uri(mdm) and n["status"]["visibility"] == "direct" for n in notifications(t, "mention")))

    print("9. deletes in both directions")
    m_gone = lambda nid: lambda: m("notes/show", ok=(200, 400, 404), noteId=nid).get("error") is not None
    t_gone = lambda sid: lambda: t.call("GET", f"/api/v1/statuses/{sid}", ok=(200, 404)).get("error") is not None
    t.call("DELETE", f"/api/v1/statuses/{tpriv['id']}")
    wait("trough delete reached Misskey", m_gone(mpriv["id"]))
    t.call("DELETE", f"/api/v1/statuses/{tmention['id']}")
    wait("trough delete of a post with replies reached Misskey", m_gone(mm["id"]))
    m("notes/delete", noteId=mpost["id"])
    wait("Misskey delete reached trough", t_gone(tpost["id"]))

    print("10. profile updates both ways")
    t.call("PATCH", "/api/v1/accounts/update_credentials", {"note": f"bio {stamp}"})
    wait("trough profile on Misskey", lambda: f"bio {stamp}" in (m("users/show", userId=bob_on_m["id"])["description"] or ""))
    m("i/update", description=f"misskey bio {stamp}")
    wait("Misskey profile on trough", lambda: f"misskey bio {stamp}" in t.get(f"/api/v1/accounts/{mia_on_t['id']}")["note"])

    print("11. unfollow both ways")
    t.post(f"/api/v1/accounts/{mia_on_t['id']}/unfollow")
    m("following/delete", userId=bob_on_m["id"])
    wait("Misskey dropped the follower", lambda: not any(f["follower"]["username"] == bob["username"] for f in m("users/followers", userId=mia["id"])))
    wait("trough dropped the follower", lambda: not any(a["acct"] == mia_acct for a in t.get(f"/api/v1/accounts/{bob['id']}/followers")))
    print("all interop checks passed against", name)


if __name__ == "__main__":
    main()
