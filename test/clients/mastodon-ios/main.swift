// Decodes trough's answers with the official Mastodon iOS app's entities
// (MastodonSDK) and its own JSONDecoder, on the paths the app requests.
// Built and run by test/clients/run-mastodon-ios.sh.
import Foundation
#if canImport(FoundationNetworking)
import FoundationNetworking
#endif
import MastodonEntities

let env = ProcessInfo.processInfo.environment
let base = "http://" + env["MASTODON_IOS_SERVER"]!
let aliceToken = env["MASTODON_IOS_TOKEN_ALICE"]!, bobToken = env["MASTODON_IOS_TOKEN_BOB"]!
var failures = 0

func check(_ what: String, _ ok: Bool, _ detail: @autoclosure () -> String = "") {
  print(ok ? "  ok: \(what)" : "  FAIL: \(what) \(detail())")
  if !ok { failures += 1 }
}

func call(_ method: String, _ path: String, _ token: String, json: [String: Any]? = nil) async -> Data? {
  var req = URLRequest(url: URL(string: base + path)!)
  req.httpMethod = method
  req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
  if let json {
    req.setValue("application/json", forHTTPHeaderField: "Content-Type")
    req.httpBody = try! JSONSerialization.data(withJSONObject: json)
  }
  guard let (data, resp) = try? await URLSession.shared.data(for: req), let http = resp as? HTTPURLResponse else {
    return nil
  }
  if http.statusCode >= 300 {
    print("  FAIL: \(method) \(path) -> \(http.statusCode) \(String(decoding: data.prefix(200), as: UTF8.self))")
    failures += 1
    return nil
  }
  return data
}

func get<T: Decodable>(_ type: T.Type, _ method: String, _ path: String, _ token: String, json: [String: Any]? = nil) async -> T? {
  guard let data = await call(method, path, token, json: json) else { return nil }
  do {
    return try Mastodon.API.decoder.decode(T.self, from: data)
  } catch {
    print("  FAIL: decoding \(T.self) from \(method) \(path): \(error)")
    failures += 1
    return nil
  }
}

typealias E = Mastodon.Entity

print("1. instance and account")
let inst = await get(E.V2.Instance.self, "GET", "/api/v2/instance", aliceToken)
check("v2 instance", inst != nil)
let v1 = await get(E.Instance.self, "GET", "/api/v1/instance", aliceToken)
check("v1 instance", v1 != nil)
check("instance rules", v1?.rules?.first?.title == "Be kind" && v1?.rules?.first?.hint == "No harassment.", "\(String(describing: v1?.rules))")
let rules = await get([E.Instance.Rule].self, "GET", "/api/v1/instance/rules", aliceToken)
check("rules", rules?.count == 1)
guard let me = await get(E.Account.self, "GET", "/api/v1/accounts/verify_credentials", aliceToken),
  let bob = await get(E.Account.self, "GET", "/api/v1/accounts/verify_credentials", bobToken)
else { print("cannot continue"); exit(1) }
check("account", me.username == "alice")
let rel = await get(E.Relationship.self, "POST", "/api/v1/accounts/\(bob.id)/follow", aliceToken)
check("follow", rel?.following == true)
let _ = await get([E.Relationship].self, "GET", "/api/v1/accounts/relationships?id[]=\(bob.id)", aliceToken)
let _ = await get([E.FamiliarFollowers].self, "GET", "/api/v1/accounts/familiar_followers?id[]=\(bob.id)", aliceToken)
let _ = await get([E.V2.SuggestionAccount].self, "GET", "/api/v2/suggestions", aliceToken)
let _ = await get([E.Tag].self, "GET", "/api/v1/followed_tags", aliceToken)
let _ = await get([E.FeaturedTag].self, "GET", "/api/v1/accounts/\(bob.id)/featured_tags", aliceToken)
let _ = await get([E.Emoji].self, "GET", "/api/v1/custom_emojis", aliceToken)

print("2. posting, quotes, polls")
guard let post = await get(E.Status.self, "POST", "/api/v1/statuses", bobToken, json: ["status": "hello from bob #ios", "visibility": "public"]) else {
  print("cannot continue"); exit(1)
}
check("quote approval present", post.quoteApproval != nil)
let _ = await call("POST", "/api/v1/featured_tags", bobToken, json: ["name": "ios"])
let featured = await get([E.FeaturedTag].self, "GET", "/api/v1/accounts/\(bob.id)/featured_tags", aliceToken)
check("featured tag", featured?.first?.name == "ios" && featured?.first?.statusesCount == "1" && featured?.first?.lastStatusAt != nil,
  "\(String(describing: featured))")
let quote = await get(E.Status.self, "POST", "/api/v1/statuses", aliceToken,
  json: ["status": "so true", "visibility": "public", "quoted_status_id": post.id, "quote_approval_policy": "followers"])
check("quote accepted", quote?.quote?.state == .accepted, "\(String(describing: quote?.quote?.state))")
check("quoted status", quote?.quote?.quotedStatus?.id == post.id)
let poll = await get(E.Status.self, "POST", "/api/v1/statuses", bobToken,
  json: ["status": "tea or coffee", "visibility": "public", "poll": ["options": ["tea", "coffee"], "expires_in": 3600]])
check("poll", poll?.poll?.options.count == 2)
if let pid = poll?.poll?.id {
  let voted = await get(E.Poll.self, "POST", "/api/v1/polls/\(pid)/votes", aliceToken, json: ["choices": [0]])
  check("vote", voted?.voted == true)
}
let reply = await get(E.Status.self, "POST", "/api/v1/statuses", aliceToken, json: ["status": "@bob hi", "in_reply_to_id": post.id])
check("reply", reply?.inReplyToID == post.id)
if let reply {
  let edited = await get(E.Status.self, "PUT", "/api/v1/statuses/\(reply.id)", aliceToken, json: ["status": "@bob hi there"])
  check("edit", edited?.editedAt != nil)
  let hist = await get([E.StatusEdit].self, "GET", "/api/v1/statuses/\(reply.id)/history", aliceToken)
  check("history", (hist?.count ?? 0) >= 2)
  let _ = await get(E.StatusSource.self, "GET", "/api/v1/statuses/\(reply.id)/source", aliceToken)
}
let ctx = await get(E.Context.self, "GET", "/api/v1/statuses/\(post.id)/context", aliceToken)
check("context", ctx?.descendants.contains { $0.id == reply?.id } == true)
let fav = await get(E.Status.self, "POST", "/api/v1/statuses/\(post.id)/favourite", aliceToken)
check("favourite", fav?.favourited == true)
let _ = await get(E.Status.self, "POST", "/api/v1/statuses/\(post.id)/reblog", aliceToken)
let _ = await get(E.Status.self, "POST", "/api/v1/statuses/\(post.id)/bookmark", aliceToken)
let _ = await get([E.Account].self, "GET", "/api/v1/statuses/\(post.id)/favourited_by", aliceToken)
let _ = await get([E.Account].self, "GET", "/api/v1/statuses/\(post.id)/reblogged_by", aliceToken)
let quotes = await get([E.Status].self, "GET", "/api/v1/statuses/\(post.id)/quotes", bobToken)
check("quotes list", quotes?.first?.id == quote?.id)
let dm = await get(E.Status.self, "POST", "/api/v1/statuses", aliceToken, json: ["status": "@bob psst", "visibility": "direct"])
check("direct", dm?.visibility == .direct)

print("3. timelines and notifications")
let home = await get([E.Status].self, "GET", "/api/v1/timelines/home", aliceToken)
check("home", home?.isEmpty == false)
let _ = await get([E.Status].self, "GET", "/api/v1/timelines/public?local=true", aliceToken)
let _ = await get([E.Status].self, "GET", "/api/v1/timelines/tag/ios", aliceToken)
let _ = await get([E.Status].self, "GET", "/api/v1/bookmarks", aliceToken)
let _ = await get([E.Status].self, "GET", "/api/v1/favourites", aliceToken)
let _ = await get([E.Status].self, "GET", "/api/v1/accounts/\(bob.id)/statuses?exclude_replies=true", aliceToken)
let grouped = await get(E.GroupedNotificationsResults.self, "GET",
  "/api/v2/notifications?grouped_types[]=favourite&grouped_types[]=reblog&grouped_types[]=follow&expand_accounts=partial_avatars", bobToken)
check("grouped notifications", (grouped?.notificationGroups.count ?? 0) >= 4, "\(grouped?.notificationGroups.count ?? -1)")
let notifs = await get([E.Notification].self, "GET", "/api/v1/notifications", bobToken)
check("notifications", (notifs?.count ?? 0) >= 4)
let _ = await get(E.NotificationPolicy.self, "GET", "/api/v2/notifications/policy", bobToken)
let _ = await get([E.NotificationRequest].self, "GET", "/api/v1/notifications/requests", bobToken)
let _ = await get([String: E.Marker].self, "GET", "/api/v1/markers?timeline[]=home&timeline[]=notifications", bobToken)

print("4. search, lists, filters, conversations, trends")
let found = await get(E.SearchResult.self, "GET", "/api/v2/search?q=bob&resolve=true", aliceToken)
check("search", found?.accounts.contains { $0.id == bob.id } == true)
let list = await get(E.List.self, "POST", "/api/v1/lists", aliceToken, json: ["title": "friends"])
if let list {
  let _ = await call("POST", "/api/v1/lists/\(list.id)/accounts", aliceToken, json: ["account_ids": [bob.id]])
  let members = await get([E.Account].self, "GET", "/api/v1/lists/\(list.id)/accounts", aliceToken)
  check("list members", members?.first?.id == bob.id)
}
let _ = await get([E.List].self, "GET", "/api/v1/lists", aliceToken)
let _ = await get([E.FilterV2].self, "GET", "/api/v2/filters", aliceToken)
let convs = await get([E.Conversation].self, "GET", "/api/v1/conversations", bobToken)
check("conversation", convs?.first?.lastStatus?.id == dm?.id)
let _ = await get([E.Tag].self, "GET", "/api/v1/trends/tags", aliceToken)
let _ = await get([E.Status].self, "GET", "/api/v1/trends/statuses", aliceToken)
let _ = await get([E.Card].self, "GET", "/api/v1/trends/links", aliceToken)

print(failures == 0 ? "all Mastodon iOS checks passed" : "\(failures) Mastodon iOS checks FAILED")
exit(failures == 0 ? 0 : 1)
