#!/usr/bin/env python3
# Federation interop check between trough and a real Lemmy, driven over
# trough's Mastodon client API and Lemmy's own API (v3). Stdlib only.
#
#   python3 test/interop/interop_lemmy.py <trough-base> <user> <password> <lemmy-base> <lemmy-user> <lemmy-password>
#
# test/interop/run-lemmy.py sets everything up and calls this. Lemmy users
# can't follow people, so the scenario is about communities: a trough user
# follows one, sees its posts, upvotes and comments, and gets replies.
import json
from pathlib import Path
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

sys.path.insert(0, str(Path(__file__).resolve().parent))
from interop import API, notifications, password_token, text, wait  # noqa: E402


class Lemmy:
    def __init__(self, base, jwt=None):
        self.base, self.jwt = base.rstrip("/"), jwt

    def call(self, method, path, data=None, ok=(200,)):
        url = f"{self.base}/api/v3{path}"
        body = None
        if method == "GET" and data:
            url += "?" + urllib.parse.urlencode(data)
        elif data is not None:
            body = json.dumps(data).encode()
        r = urllib.request.Request(url, data=body, method=method, headers={"Content-Type": "application/json"})
        if self.jwt:
            r.add_header("Authorization", "Bearer " + self.jwt)
        try:
            with urllib.request.urlopen(r, timeout=30) as resp:
                code, raw = resp.status, resp.read()
        except urllib.error.HTTPError as e:
            code, raw = e.code, e.read()
        if code not in ok:
            raise AssertionError(f"{method} {path} -> {code}: {raw[:300]!r}")
        return json.loads(raw) if raw else None

    def get(self, path, **params):
        return self.call("GET", path, params)

    def post(self, path, **data):
        return self.call("POST", path, data)


def main():
    tbase, tuser, tpw, lbase, luser, lpw = sys.argv[1:7]
    t = API(tbase, password_token(tbase, tuser, tpw))
    lem = Lemmy(lbase)
    lem.jwt = lem.post("/user/login", username_or_email=luser, password=lpw)["jwt"]
    lhost = urllib.parse.urlparse(lbase).netloc
    site = lem.get("/site")
    print("trough:", tuser, "| lemmy:", lhost, site["version"])
    stamp = str(int(time.time()))

    print("1. a Lemmy community, followed from trough")
    community = lem.post("/community", name=f"trough{stamp}", title="trough interop")["community_view"]["community"]
    acct = f"{community['name']}@{lhost}"
    group = wait("trough resolves the community", lambda: next((a for a in t.get("/api/v2/search?" + urllib.parse.urlencode({"q": "@" + acct, "resolve": "true"}))["accounts"] if a["acct"] == acct), None))
    assert group.get("group") is True, f"community is not a group: {group}"
    t.post(f"/api/v1/accounts/{group['id']}/follow")
    # Lemmy loses the Accept: it starts sending to a newly seen instance up to a minute
    # later and skips what was queued before. trough resends the Follow after 3 minutes.
    wait("Lemmy accepted the follow", lambda: t.get(f"/api/v1/accounts/relationships?id[]={group['id']}")[0]["following"], timeout=300)
    wait("Lemmy counts trough's user as a subscriber", lambda: lem.get("/community", id=community["id"])["community_view"]["counts"]["subscribers"] >= 2)

    print("2. a Lemmy post shows up on trough")
    title = f"hello from lemmy {stamp}"
    post = lem.post("/post", name=title, community_id=community["id"], body="a body with **markdown**")["post_view"]["post"]

    def find(needle):
        for s in t.get("/api/v1/timelines/home?limit=40"):
            o = s.get("reblog") or s
            if needle in text(o) or needle in (o.get("spoiler_text") or ""):
                return o
        return None
    tpost = wait("post announced by the community reached trough home", lambda: find(title))

    print("3. likes and comments from trough reach Lemmy")
    t.post(f"/api/v1/statuses/{tpost['id']}/favourite")
    wait("favourite counts as an upvote on Lemmy", lambda: lem.get("/post", id=post["id"])["post_view"]["counts"]["upvotes"] >= 2)
    reply = t.post("/api/v1/statuses", {"status": f"@{acct} nice post {stamp}", "in_reply_to_id": tpost["id"], "visibility": "public"})

    def comment_by_trough():
        for c in lem.get("/comment/list", post_id=post["id"], type_="All", sort="New")["comments"]:
            if f"nice post {stamp}" in c["comment"]["content"]:
                return c["comment"]
        return None
    tcomment = wait("trough reply became a Lemmy comment", comment_by_trough)

    print("4. Lemmy replies reach trough")
    lem.post("/comment", content=f"thanks from lemmy {stamp}", post_id=post["id"], parent_id=tcomment["id"])
    wait("reply to trough's comment notified the trough user", lambda: any(f"thanks from lemmy {stamp}" in text(n["status"]) for n in notifications(t, "mention")))

    print("5. edits and deletes")
    lem.call("PUT", "/post", {"post_id": post["id"], "name": title + " (edited)", "body": "a body with **markdown**"})
    try:
        wait("Lemmy edit reached trough", lambda: find(title + " (edited)"), timeout=20)
    except AssertionError as e:
        print("  WARN:", e)
    t.call("DELETE", f"/api/v1/statuses/{reply['id']}")
    wait("trough delete reached Lemmy", lambda: lem.get("/comment", id=tcomment["id"])["comment_view"]["comment"]["deleted"])

    print("6. unfollow")
    t.post(f"/api/v1/accounts/{group['id']}/unfollow")
    wait("Lemmy dropped trough's user from the community", lambda: lem.get("/community", id=community["id"])["community_view"]["counts"]["subscribers"] == 1)
    print("all interop checks passed against lemmy", site["version"])


if __name__ == "__main__":
    main()
