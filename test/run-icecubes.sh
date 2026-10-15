#!/usr/bin/env bash
# Runs the iOS app Ice Cubes' own networking code against a disposable trough.
#
# iOS apps can't run here, but Ice Cubes is open source: its Models and
# NetworkClient packages (the Codable entities and MastodonClient every
# screen goes through) build on Linux with a few shims for Apple-only APIs
# (SwiftUI/OSLog types, Markdown AttributedString, a lock). test/icecubes/
# main.swift then walks through what its screens do (instance, accounts,
# posting, polls, quotes, timelines, grouped notifications with paging,
# search, lists, filters, conversations, media) and checks the decoded
# values, since the app decodes some fields leniently.
#
#   bash test/run-icecubes.sh
#
# Needs git, go, curl, python3 and Nix (Swift 6.2 from nixpkgs). Ice Cubes
# and SwiftSoup are pinned below; everything lives in a temp directory.
# Changes to the app's code: only imports, the shims, http instead of https
# and splitting host:port (a loopback server has a port).
set -euo pipefail
ICECUBES_REV=${ICECUBES_REV:-98098b9c4b5928f89f338203193c2bf40ab7f38c}
SWIFTSOUP_REV=18b80329749eca5ea29fc50211dca5c7eff5bfec # 2.13.9, as Ice Cubes resolves it
repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
pid=
cleanup() { [ -n "$pid" ] && kill "$pid" 2>/dev/null; rm -rf "$tmp"; }
trap cleanup EXIT

fetch() { # url rev dir
  git init -q "$3" && git -C "$3" fetch -q --depth 1 "$1" "$2" && git -C "$3" checkout -q FETCH_HEAD
}
fetch https://github.com/Dimillian/IceCubesApp "$ICECUBES_REV" "$tmp/ice"
fetch https://github.com/scinfu/SwiftSoup "$SWIFTSOUP_REV" "$tmp/soup"

src=$tmp/src
mkdir -p "$src/models" "$src/network" "$tmp/out"
cp -r "$tmp/ice/Packages/Models/Sources/Models/." "$src/models/"
rm -rf "$src/models/SwiftData" "$src/models/AppAccount.swift"
cp -r "$tmp/ice/Packages/NetworkClient/Sources/NetworkClient/." "$src/network/"
rm -f "$src/network/"{OpenAIClient,DeepLClient,InstanceSocialClient}.swift
cp "$repo/test/icecubes/ModelShims.swift" "$src/models/"
cp "$repo/test/icecubes/NetworkShims.swift" "$src/network/"
find "$src" -name '*.swift' -exec sed -i -e 's/^import SwiftUI$/import Foundation/' -e '/^import \(Combine\|OSLog\|os\)$/d' {} +
python3 - "$src/network/MastodonClient.swift" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
for old, new in [
    ('scheme: String = "https"', 'scheme: String = ProcessInfo.processInfo.environment["ICE_SCHEME"] ?? "https"'),
    ("    components.host = forceServer ?? server\n",
     "    components.host = forceServer ?? server\n"
     "    if let h = components.host, let i = h.lastIndex(of: \":\"), let port = Int(h[h.index(after: i)...]) {\n"
     "      components.host = String(h[..<i])\n"
     "      components.port = port\n"
     "    }\n"),
]:
    assert old in s, old
    s = s.replace(old, new, 1)
open(p, "w").write(s)
PY

echo "building Ice Cubes' Models and NetworkClient with Swift..."
cat > "$tmp/build.sh" <<EOF
set -e
cd "$tmp/out"
swiftc -O -emit-library -emit-module -parse-as-library -module-name SwiftSoup -swift-version 5 \$(find "$tmp/soup/Sources" -name '*.swift') -o libSwiftSoup.so -emit-module-path SwiftSoup.swiftmodule
swiftc -emit-library -emit-module -parse-as-library -module-name Models -swift-version 5 -I . -L . -lSwiftSoup \$(find "$src/models" -name '*.swift') -o libModels.so -emit-module-path Models.swiftmodule
swiftc -emit-library -emit-module -parse-as-library -module-name NetworkClient -swift-version 5 -I . -L . -lSwiftSoup -lModels \$(find "$src/network" -name '*.swift') -o libNetworkClient.so -emit-module-path NetworkClient.swiftmodule
swiftc -swift-version 5 -I . -L . -lSwiftSoup -lModels -lNetworkClient -Xlinker -rpath -Xlinker "$tmp/out" "$repo/test/icecubes/main.swift" -o icecubes
EOF
bash -lc "nix-shell --pure '$repo/test/icecubes/shell.nix' --run 'bash $tmp/build.sh'" 2>&1 | grep -E 'error:' || true
[ -x "$tmp/out/icecubes" ] || { echo "build failed"; exit 1; }

port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
host=127.0.0.1:$port
flags=(-insecure -host "$host" -data "$tmp/data")
(cd "$repo" && go build -o "$tmp/trough" .)
for u in alice bob; do echo "${u}password12" | "$tmp/trough" user create "${flags[@]}" -password-stdin $u >/dev/null 2>&1; done
"$tmp/trough" serve "${flags[@]}" -listen "$host" >"$tmp/log" 2>&1 &
pid=$!
for _ in $(seq 50); do curl -sf "http://$host/nodeinfo/2.0" >/dev/null && break; sleep 0.1; done

token() { # an app registered like Ice Cubes does, then a password grant
  python3 - "$host" "$1" <<'PY'
import json, sys, urllib.parse, urllib.request
host, user = sys.argv[1], sys.argv[2]
post = lambda p, d: json.load(urllib.request.urlopen(urllib.request.Request(f"http://{host}{p}", data=urllib.parse.urlencode(d).encode())))
app = post("/api/v1/apps", {"client_name": "Ice Cubes", "redirect_uris": "icecubesapp://", "scopes": "read write follow push"})
print(post("/oauth/token", {"grant_type": "password", "username": user, "password": user + "password12", "scope": "read write follow push",
                            "client_id": app["client_id"], "client_secret": app["client_secret"]})["access_token"])
PY
}
png=$(python3 -c '
import base64, struct, zlib
chunk = lambda t, d: struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d))
rows = b"".join(b"\0" + b"\xff\0\0" * 8 for _ in range(6))
print(base64.b64encode(chunk(b"IHDR", struct.pack(">IIBBBBB", 8, 6, 8, 2, 0, 0, 0)) + chunk(b"IDAT", zlib.compress(rows)) + chunk(b"IEND", b"")).decode())')
set +e
ICE_SCHEME=http ICE_SERVER=$host ICE_TOKEN_ALICE=$(token alice) ICE_TOKEN_BOB=$(token bob) ICE_PNG_REST=$png timeout 300 "$tmp/out/icecubes"
rc=$?
[ $rc -ne 0 ] && { echo "--- trough log ---"; tail -20 "$tmp/log"; }
exit $rc
