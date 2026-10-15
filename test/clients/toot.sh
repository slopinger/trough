#!/usr/bin/env bash
# Client API check with the toot CLI (https://toot.bezdomni.net) against a
# disposable dev instance. Needs toot on PATH, e.g.:
#   nix shell nixpkgs#toot --command bash test/clients/toot.sh
set -euo pipefail
repo=$(cd "$(dirname "$0")/../.." && pwd)
tmp=$(mktemp -d)
pid=
cleanup() { [ -n "$pid" ] && kill "$pid" 2>/dev/null; rm -rf "$tmp"; }
trap cleanup EXIT
port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
host=127.0.0.1:$port
flags=(-insecure -host "$host" -data "$tmp/data")
(cd "$repo" && go build -o "$tmp/trough" ./cmd/trough)
for u in alice bob; do echo "${u}password12" | "$tmp/trough" user create "${flags[@]}" -password-stdin $u >/dev/null 2>&1; done
"$tmp/trough" serve "${flags[@]}" -listen "$host" >"$tmp/log" 2>&1 &
pid=$!
for _ in $(seq 50); do curl -sf "http://$host/nodeinfo/2.0" >/dev/null && break; sleep 0.1; done
export XDG_CONFIG_HOME=$tmp/config
python3 -c "
import zlib, struct
c = lambda t, d: struct.pack('>I', len(d)) + t + d + struct.pack('>I', zlib.crc32(t + d))
raw = b''.join(b'\0' + b'\xff\0\0' * 16 for _ in range(16))
open('$tmp/red.png', 'wb').write(b'\x89PNG\r\n\x1a\n' + c(b'IHDR', struct.pack('>IIBBBBB', 16, 16, 8, 2, 0, 0, 0)) + c(b'IDAT', zlib.compress(raw)) + c(b'IEND', b''))"

T() {
	if ! out=$(toot "$@" 2>&1) || grep -q '^Error' <<<"$out"; then
		echo "FAIL: toot $*"; echo "$out"; echo "--- server log"; tail -20 "$tmp/log"; exit 1
	fi
	echo "ok: toot $*"
}
want() { grep -q -- "$1" <<<"$out" || { echo "FAIL: expected '$1' in: $out"; exit 1; }; }

T login_cli -i "http://$host" -e alice -p alicepassword12
T whoami; want "@alice"
T instance; want "$host"
T post "hello from toot #cli" --visibility public
id=${out##*/}
T post "a reply" --reply-to "$id"
T thread "$id"; want "a reply"
T favourite "$id"
T reblog "$id"
T bookmark "$id"
T bookmarks; want "hello from toot"
T pin "$id"
T follow bob
T following alice; want "bob"
T whois bob; want "@bob"
T search bob; want "@bob"
T post "poll time" --poll-option yes --poll-option no --poll-expires-in 1h
T post --using "alice@$host" "content warning" --spoiler-text "cw"
T timeline --public; want "hello from toot"
T timeline --tag cli; want "hello from toot"
T tags follow cli
T tags followed; want "#cli"
T lists create friends
T lists add friends bob
T lists accounts friends; want "bob"
T timeline --list friends
T upload "$tmp/red.png" --description "an image"; want "type 'image'"
T post "with media" --media "$tmp/red.png" --description "alt text"
T status "$id"; want "hello from toot"
T unpin "$id"
T unreblog "$id"
T unfavourite "$id"
T delete "$id"
T unfollow bob
T mute bob
T unmute bob
T block bob
T unblock bob
T notifications
echo "all toot checks passed"
