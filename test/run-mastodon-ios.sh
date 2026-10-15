#!/usr/bin/env bash
# Checks trough's client API with the official Mastodon iOS app's own data layer.
#
# The app (github.com/mastodon/mastodon-ios) is open source. Its API layer
# needs Combine, but its entities (MastodonSDK/Entity, every type it
# decodes server answers into) and its JSONDecoder build on Linux with
# shims for a few display-only Apple APIs. test/mastodon-ios/main.swift
# then calls a disposable trough on the paths the app requests, with the
# JSON bodies it sends, and decodes the answers with them.
#
#   bash test/run-mastodon-ios.sh
#
# Needs git, go, curl, python3 and Nix (Swift 6.2 from nixpkgs). The app is
# pinned below; everything lives in a temp directory. Left out: the
# donation-campaign entity (needs the app's settings store), the onboarding
# server picker's Language and the unused Preferences entity.
set -euo pipefail
MASTODON_IOS_REV=${MASTODON_IOS_REV:-14c140cb2be0ec5a6a004bfb89f7b8d8f5436013}
repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
pid=
cleanup() { [ -n "$pid" ] && kill "$pid" 2>/dev/null; rm -rf "$tmp"; }
trap cleanup EXIT

git init -q "$tmp/app"
git -C "$tmp/app" fetch -q --depth 1 https://github.com/mastodon/mastodon-ios "$MASTODON_IOS_REV"
git -C "$tmp/app" checkout -q FETCH_HEAD
sdk=$tmp/app/MastodonSDK/Sources/MastodonSDK
mkdir -p "$tmp/src" "$tmp/out"
cp "$sdk"/Entity/*.swift "$sdk/Mastodon.swift" "$sdk/InstanceConfigLimitingPropertyContaining.swift" "$tmp/src/"
rm "$tmp/src/Mastodon+Entity+DonationCampaign.swift"
sed -i '/^import \(UIKit\|MastodonCommon\|MastodonLocalization\)$/d' "$tmp/src"/*.swift
cp "$repo/test/mastodon-ios/Shims.swift" "$tmp/src/"
# the app's JSONDecoder (custom date parsing), verbatim from Mastodon+API.swift
python3 - "$sdk/API/Mastodon+API.swift" "$tmp/src/Decoder.swift" <<'PY'
import sys
src = open(sys.argv[1]).read()
body = src[src.index("    static let fractionalSecondsPreciseISO8601Formatter"):src.index("    static func oauthEndpointURL")]
open(sys.argv[2], "w").write("import Foundation\n\nextension Mastodon.API {\n" + body.replace("static let decoder", "public static let decoder") + "}\n")
PY

echo "building the Mastodon iOS app's entities with Swift..."
cat > "$tmp/build.sh" <<SH
set -e
cd "$tmp/out"
swiftc -emit-library -emit-module -parse-as-library -module-name MastodonEntities -swift-version 5 "$tmp"/src/*.swift -o libMastodonEntities.so -emit-module-path MastodonEntities.swiftmodule
swiftc -swift-version 5 -I . -L . -lMastodonEntities -Xlinker -rpath -Xlinker "$tmp/out" "$repo/test/mastodon-ios/main.swift" -o check
SH
bash -lc "nix-shell --pure '$repo/test/swift-shell.nix' --run 'bash $tmp/build.sh'" 2>&1 | grep -E 'error:' || true
[ -x "$tmp/out/check" ] || { echo "build failed"; exit 1; }

port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
host=127.0.0.1:$port
flags=(-insecure -host "$host" -data "$tmp/data")
(cd "$repo" && go build -o "$tmp/trough" ./cmd/trough)
for u in alice bob; do echo "${u}password12" | "$tmp/trough" user create "${flags[@]}" -password-stdin $u >/dev/null 2>&1; done
"$tmp/trough" serve "${flags[@]}" -listen "$host" >"$tmp/log" 2>&1 &
pid=$!
for _ in $(seq 50); do curl -sf "http://$host/nodeinfo/2.0" >/dev/null && break; sleep 0.1; done
token() {
  python3 - "$host" "$1" <<'PY'
import json, sys, urllib.parse, urllib.request
host, user = sys.argv[1], sys.argv[2]
post = lambda p, d: json.load(urllib.request.urlopen(urllib.request.Request(f"http://{host}{p}", data=urllib.parse.urlencode(d).encode())))
app = post("/api/v1/apps", {"client_name": "Mastodon for iOS", "redirect_uris": "mastodon://joinmastodon.org/oauth", "scopes": "read write follow push"})
print(post("/oauth/token", {"grant_type": "password", "username": user, "password": user + "password12", "scope": "read write follow push",
                            "client_id": app["client_id"], "client_secret": app["client_secret"]})["access_token"])
PY
}
set +e
MASTODON_IOS_SERVER=$host MASTODON_IOS_TOKEN_ALICE=$(token alice) MASTODON_IOS_TOKEN_BOB=$(token bob) timeout 300 "$tmp/out/check"
rc=$?
[ $rc -ne 0 ] && { echo "--- trough log ---"; tail -20 "$tmp/log"; }
exit $rc
