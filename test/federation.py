#!/usr/bin/env python3
"""End-to-end federation test between two local trough instances.

Start two fresh dev instances (see README, "Local development") with users
alice (on 127.0.0.1:8801) and bob (on 127.0.0.1:8802), then run:

    python3 test/federation.py <alice-password> <bob-password>
"""
import http.cookiejar, os, re, sys, time, urllib.parse, urllib.request

A = os.environ.get("TROUGH_TEST_A", "http://127.0.0.1:8801")
B = os.environ.get("TROUGH_TEST_B", "http://127.0.0.1:8802")
AH = urllib.parse.urlsplit(A).netloc
BH = urllib.parse.urlsplit(B).netloc


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None


class Client:
    def __init__(self, base):
        self.base = base
        self.jar = http.cookiejar.CookieJar()
        self.op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))
        self.csrf = ""

    def req(self, method, path, data=None, headers=None):
        h = {"Origin": self.base}
        h.update(headers or {})
        body = None
        if data is not None:
            body = urllib.parse.urlencode(data).encode()
            h["Content-Type"] = "application/x-www-form-urlencoded"
        r = urllib.request.Request(self.base + path, data=body, method=method, headers=h)
        try:
            resp = self.op.open(r)
            text = resp.read().decode()
            url = resp.geturl()
        except urllib.error.HTTPError as e:
            text = e.read().decode()
            url = e.geturl()
            if e.code >= 400:
                print(f"  ! {method} {path} -> {e.code}: {text[:300]}")
        m = re.search(r'name="csrf" value="([0-9a-f]+)"', text)
        if m:
            self.csrf = m.group(1)
        return url, text

    def get(self, path):
        return self.req("GET", path)

    def post(self, path, **data):
        data["csrf"] = self.csrf
        return self.req("POST", path, data)

    def login(self, user, pw):
        url, text = self.req("POST", "/login", {"username": user, "password": pw})
        assert "Log out" in text, "login failed"
        print(f"logged in as {user} at {self.base}")


def wait(cond, what, timeout=15):
    t = time.time()
    while time.time() - t < timeout:
        if cond():
            print(f"  ok: {what}")
            return
        time.sleep(0.5)
    print(f"  FAIL: {what}")
    sys.exit(1)


alice, bob = Client(A), Client(B)
alice.login("alice", os.environ.get("TROUGH_TEST_ALICE_PASSWORD") or sys.argv[1])
bob.login("bob", os.environ.get("TROUGH_TEST_BOB_PASSWORD") or sys.argv[2])


# bob finds and follows alice
url, text = bob.get("/search?q=" + urllib.parse.quote(f"@alice@{AH}"))
assert url.endswith(f"/@alice@{AH}"), url
acc_id = re.search(r'/accounts/([0-9A-Z]+)/follow"', text).group(1)
bob.post(f"/accounts/{acc_id}/follow")
wait(lambda: "Unfollow" in bob.get(f"/@alice@{AH}")[1], "bob follows alice (Accept received)")
wait(lambda: "followed you" in alice.get("/notifications")[1], "alice got follow notification")

# alice posts, mentioning bob
alice.get("/")
alice.post("/statuses", text=f"Hello fediverse! #trough test, ping @bob@{BH} https://example.com/a/very/long/link/that/should/be/shortened", visibility="public")
wait(lambda: "Hello fediverse!" in bob.get("/")[1], "post delivered to bob's home")
wait(lambda: "mentioned you" in bob.get("/notifications")[1], "bob got mention notification")
url, text = bob.get("/")
sid = re.search(r'/statuses/([0-9A-Z]+)/like"', text).group(1)
bob.post(f"/statuses/{sid}/like")
bob.get("/")
bob.post(f"/statuses/{sid}/boost")
wait(lambda: "favourited your post" in alice.get("/notifications")[1], "alice got like")
wait(lambda: "boosted your post" in alice.get("/notifications")[1], "alice got boost")

# Undo deliveries remove the original interaction and its notification.
bob.get("/")
bob.post(f"/statuses/{sid}/unlike")
bob.get("/")
bob.post(f"/statuses/{sid}/unboost")
wait(lambda: "favourited your post" not in alice.get("/notifications")[1], "unlike removed alice's notification")
wait(lambda: "boosted your post" not in alice.get("/notifications")[1], "unboost removed alice's notification")

# bob replies
bob.get("/")
bob.post("/statuses", text=f"@alice@{AH} hi back!", visibility="public", in_reply_to=sid)
url, text = alice.get("/")
asid = re.search(r'/statuses/([0-9A-Z]+)/delete"', text).group(1)
wait(lambda: "hi back!" in alice.get(f"/s/{asid}")[1], "reply visible in alice's thread")

# followers-only post
alice.get("/")
alice.post("/statuses", text="secret for followers", visibility="private")
wait(lambda: "secret for followers" in bob.get("/")[1], "followers-only post delivered")
anon = Client(A)
assert "secret for followers" not in anon.get("/@alice")[1], "private post leaked to anonymous"
print("  ok: private post hidden from anonymous visitors")

# profile update federates
alice.get("/settings")
alice.post("/settings/profile", display_name="Alice Wonder", note="I live in #wonderland")
wait(lambda: "Alice Wonder" in bob.get(f"/@alice@{AH}")[1], "profile update federated")

# delete
alice.get("/")
alice.post(f"/statuses/{asid}/delete")
wait(lambda: "Hello fediverse!" not in bob.get("/")[1], "delete federated")

# unfollow
bob.get(f"/@alice@{AH}")
bob.post(f"/accounts/{acc_id}/unfollow")
wait(lambda: alice.get("/@alice/followers")[1].count("accrow") == 0, "unfollow federated")
wait(lambda: "followed you" not in alice.get("/notifications")[1], "unfollow removed alice's notification")

# locked account: follow requests
alice.get("/settings")
alice.post("/settings/profile", display_name="Alice Wonder", note="locked now", locked="1")
time.sleep(1)
bob.get(f"/@alice@{AH}")
bob.post(f"/accounts/{acc_id}/follow")
wait(lambda: "Requested" in bob.get(f"/@alice@{AH}")[1], "follow is pending on a locked account")
wait(lambda: "requested to follow you" in alice.get("/notifications")[1], "alice got follow request")
url, text = alice.get("/follow_requests")
bob_id = re.search(r'/accounts/([0-9A-Z]+)/accept"', text).group(1)
alice.post(f"/accounts/{bob_id}/accept")
wait(lambda: "Unfollow" in bob.get(f"/@alice@{AH}")[1], "accepted follow request federated")

# direct message
bob.get("/")
bob.post("/statuses", text=f"@alice@{AH} psst, a direct message", visibility="direct")
wait(lambda: "psst, a direct message" in alice.get("/notifications")[1], "direct message delivered")
assert "psst" not in Client(B).get("/@bob")[1], "DM visible to anonymous"
print("  ok: direct message hidden from anonymous visitors")

# block
alice.get(f"/@bob@{BH}")
alice.post(f"/accounts/{bob_id}/block")
def blocked():
    t = bob.get(f"/@alice@{AH}")[1]
    return "has blocked you" in t and "Unfollow" not in t
wait(blocked, "block federated and removed bob's follow")
bob.get("/")
bob.post("/statuses", text=f"@alice@{AH} are you there?", visibility="public")
time.sleep(3)
assert "are you there?" not in alice.get("/notifications")[1], "notification from blocked account"
print("  ok: no notifications from blocked accounts")
print("ALL OK")
