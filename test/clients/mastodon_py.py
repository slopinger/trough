#!/usr/bin/env python3
# Mastodon client API interop check using the Mastodon.py library.
# Usage: start a dev instance with users alice (password alicepassword1) and bob, then
#   python3 test/clients/mastodon_py.py http://127.0.0.1:8813 some.png
# Mastodon.py from Nix: nix build --impure --expr 'let p = (builtins.getFlake "nixpkgs").legacyPackages.x86_64-linux; in p.python3.withPackages (ps: [ps.mastodon-py])'
import sys, threading, time, io
from mastodon import Mastodon, StreamListener

base = sys.argv[1]
cid, secret = Mastodon.create_app("Mastodon.py test", api_base_url=base, scopes=["read", "write", "follow"])
m = Mastodon(client_id=cid, client_secret=secret, api_base_url=base, version_check_mode="none")
m.log_in("alice", "alicepassword1", scopes=["read", "write", "follow"], allow_http=True)
print("instance:", m.instance().title, "| version:", m.retrieve_mastodon_version())
me = m.account_verify_credentials()
print("me:", me.acct, me.id)

got = []
class L(StreamListener):
    def on_update(self, status): got.append(("update", status.content))
    def on_notification(self, n): got.append(("notification", n.type))
handle = m.stream_user(L(), run_async=True, reconnect_async=False)
time.sleep(0.5)

st = m.status_post("hello from Mastodon.py #python", visibility="public",
                   poll=m.make_poll(["yes", "no"], expires_in=3600))
print("posted:", st.id, "poll options:", [o.title for o in st.poll.options])
reply = m.status_post("a reply", in_reply_to_id=st.id)
ctx = m.status_context(st.id)
assert len(ctx.descendants) == 1, ctx
media = m.media_post(io.BytesIO(open(sys.argv[2], "rb").read()), mime_type="image/png", description="a screenshot")
pic = m.status_post("with a picture", media_ids=[media])
assert pic.media_attachments[0].description == "a screenshot"
home = m.timeline_home(limit=2)
print("home page 1:", [s.id for s in home])
older = m.fetch_next(home)
print("home page 2:", [s.id for s in older] if older else None)
assert home and older, "pagination via Link header failed"
m.status_favourite(st.id); m.status_bookmark(st.id)
print("bookmarks:", len(m.bookmarks()))
m.status_update(reply.id, "an edited reply")
assert "edited" in m.status(reply.id).content
m.status_delete(reply.id)
res = m.search_v2("python")
print("search statuses:", len(res.statuses))
bob = m.account_lookup("bob")
rel = m.account_follow(bob.id)
print("follow bob:", rel.following)
print("relationships:", m.account_relationships([bob.id])[0].following)
m.account_update_credentials(display_name="Alice via API", note="set by Mastodon.py")
assert m.account_verify_credentials().display_name == "Alice via API"
time.sleep(1.5)
handle.close()
print("streamed:", got[:4])
assert any(e[0] == "update" for e in got), "no streamed updates"
print("notifications:", [n.type for n in m.notifications()])
print("ALL OK")
