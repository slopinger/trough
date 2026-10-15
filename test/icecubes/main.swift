// Drives a trough instance with Ice Cubes' own NetworkClient and Models
// (the iOS app's networking layer), the way its screens use them.
// Built and run by test/run-icecubes.sh.
import Foundation
import Models
import NetworkClient

let env = ProcessInfo.processInfo.environment
let server = env["ICE_SERVER"]!
func client(_ token: String) -> MastodonClient {
  MastodonClient(server: server, oauthToken: OauthToken(accessToken: token, tokenType: "Bearer", scope: "read write follow push", createdAt: 0))
}
let alice = client(env["ICE_TOKEN_ALICE"]!)
let bob = client(env["ICE_TOKEN_BOB"]!)
let alice2 = MastodonClient(server: server, version: .v2, oauthToken: OauthToken(accessToken: env["ICE_TOKEN_ALICE"]!, tokenType: "Bearer", scope: "read write follow push", createdAt: 0))
let bob2 = MastodonClient(server: server, version: .v2, oauthToken: OauthToken(accessToken: env["ICE_TOKEN_BOB"]!, tokenType: "Bearer", scope: "read write follow push", createdAt: 0))

var failures = 0
// On a decoding failure, decode again by hand to see which field it was.
func diagnose<T: Decodable>(_ type: T.Type, _ path: String, _ token: String) async {
  var comps = URLComponents(string: "http://\(server)\(path)")!
  comps.host = comps.host
  var req = URLRequest(url: comps.url!)
  req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
  guard let (data, _) = try? await URLSession.shared.data(for: req) else { return }
  let d = JSONDecoder()
  d.keyDecodingStrategy = .convertFromSnakeCase
  do { _ = try d.decode(T.self, from: data) } catch { print("    decoding \(path): \(error)") }
}
func check(_ what: String, _ ok: Bool, _ detail: @autoclosure () -> String = "") {
  print(ok ? "  ok: \(what)" : "  FAIL: \(what) \(detail())")
  if !ok { failures += 1 }
}
func step<T>(_ what: String, _ body: () async throws -> T) async -> T? {
  do { return try await body() } catch {
    print("  FAIL: \(what): \(error)")
    failures += 1
    return nil
  }
}

print("1. instance, as CurrentInstance loads it")
if let inst: Instance = await step("instance v2", { try await alice.get(endpoint: Instances.instance, forceVersion: .v2) }) {
  let v = inst.apiVersions?.mastodon ?? 0
  check("api_versions.mastodon >= 7 (quotes enabled)", v >= 7, "\(v)")
  check("version string", inst.version.hasPrefix("4.5"), inst.version)
}
// The app reads only v2 for the current instance; its Instance model needs
// v2's "domain", which Mastodon's v1 doesn't have either.
let emojis: [Emoji]? = await step("custom emojis", { try await alice.get(endpoint: CustomEmojis.customEmojis) })
check("custom emojis decode", emojis != nil)

print("2. accounts")
guard let me: Account = await step("verify_credentials", { try await alice.get(endpoint: Accounts.verifyCredentials) }),
  let bobAcc: Account = await step("bob", { try await bob.get(endpoint: Accounts.verifyCredentials) })
else { exit(1) }
check("own account", me.username == "alice")
let prefs: ServerPreferences? = await step("preferences", { try await alice.get(endpoint: Accounts.preferences) })
check("preferences decode", prefs != nil)
let rel: Relationship? = await step("follow", { try await alice.post(endpoint: Accounts.follow(id: bobAcc.id, notify: false, reblogs: true)) })
check("following bob", rel?.following == true)
let lookedUp: Account? = await step("lookup", { try await alice.get(endpoint: Accounts.lookup(name: "bob")) })
check("lookup", lookedUp?.id == bobAcc.id)
let rels: [Relationship]? = await step("relationships", { try await alice.get(endpoint: Accounts.relationships(ids: [bobAcc.id])) })
check("relationships", rels?.first?.following == true)
let _: [Account]? = await step("followers", { try await alice.get(endpoint: Accounts.followers(id: bobAcc.id, maxId: nil)) })
let _: [Account]? = await step("familiar followers", { try await alice.get(endpoint: Accounts.familiarFollowers(withAccount: bobAcc.id)) })
let _: [Tag]? = await step("followed tags", { try await alice.get(endpoint: Accounts.followedTags) })
let _: [Account]? = await step("suggestions", { try await alice.get(endpoint: Accounts.suggestions) })

print("3. posting, polls, quotes")
guard let bobPost: Status = await step("bob posts", { try await bob.post(endpoint: Statuses.postStatus(json: StatusData(status: "hello from bob #ios", visibility: .pub))) })
else { exit(1) }
check("quote_approval says alice may quote", bobPost.quoteApproval?.automatic == ["public"], "\(String(describing: bobPost.quoteApproval))")
if let seen: Status = await step("bob's post as alice", { try await alice.get(endpoint: Statuses.status(id: bobPost.id)) }) {
  check("current_user automatic", seen.quoteApproval?.currentUser == .automatic, "\(String(describing: seen.quoteApproval))")
}
let quote: Status? = await step("alice quotes", {
  try await alice.post(endpoint: Statuses.postStatus(json: StatusData(status: "so true", visibility: .pub, quotedStatusId: bobPost.id)))
})
check("quote accepted", quote?.quote?.state == .accepted, "\(String(describing: quote?.quote?.state))")
check("quoted status embedded", quote?.quote?.quotedStatus?.id == bobPost.id)
let quotes: [Status]? = await step("quotes of bob's post", { try await bob.get(endpoint: Statuses.quotesBy(id: bobPost.id, maxId: nil)) })
check("quotes list", quotes?.map(\.id) == [quote?.id].compactMap { $0 })
if let again: Status = await step("bob's post again", { try await bob.get(endpoint: Statuses.status(id: bobPost.id)) }) {
  check("quotes_count", again.quotesCount == 1, "\(String(describing: again.quotesCount))")
}
let poll: Status? = await step("poll", {
  try await bob.post(endpoint: Statuses.postStatus(json: StatusData(status: "tea or coffee", visibility: .pub, poll: .init(options: ["tea", "coffee"], multiple: false, expires_in: 3600))))
})
check("poll decoded", poll?.poll?.options.count == 2)
if let pid = poll?.poll?.id {
  let voted: Poll? = await step("vote", { try await alice.post(endpoint: Polls.vote(id: pid, votes: [1])) })
  check("vote counted", voted?.options[1].votesCount == 1 && voted?.voted == true)
}
let reply: Status? = await step("reply", {
  try await alice.post(endpoint: Statuses.postStatus(json: StatusData(status: "@bob hi", visibility: .pub, inReplyToId: bobPost.id)))
})
check("reply", reply?.inReplyToId == bobPost.id)
let edited: Status? = await step("edit", {
  try await alice.put(endpoint: Statuses.editStatus(id: reply!.id, json: StatusData(status: "@bob hi there", visibility: .pub)))
})
check("edited", edited?.editedAt != nil)
let hist: [StatusHistory]? = await step("history", { try await alice.get(endpoint: Statuses.history(id: reply!.id)) })
check("history", (hist?.count ?? 0) >= 2)
let ctx: StatusContext? = await step("context", { try await alice.get(endpoint: Statuses.context(id: bobPost.id)) })
check("context descendants", ctx?.descendants.contains { $0.id == reply?.id } == true)
let fav: Status? = await step("favourite", { try await alice.post(endpoint: Statuses.favorite(id: bobPost.id)) })
check("favourited", fav?.favourited == true)
let boost: Status? = await step("reblog", { try await alice.post(endpoint: Statuses.reblog(id: bobPost.id)) })
check("reblogged", boost?.reblog?.id == bobPost.id || boost?.reblogged == true)
let _: Status? = await step("bookmark", { try await alice.post(endpoint: Statuses.bookmark(id: bobPost.id)) })
let favBy: [Account]? = await step("favourited by", { try await bob.get(endpoint: Statuses.favoritedBy(id: bobPost.id, maxId: nil)) })
check("favourited by", favBy?.first?.id == me.id)
let _: Status? = await step("pin", { try await bob.post(endpoint: Statuses.pin(id: bobPost.id)) })

print("4. timelines")
let home: [Status]? = await step("home", { try await alice.get(endpoint: Timelines.home(sinceId: nil, maxId: nil, minId: nil, limit: 40)) })
check("home has bob's post", home?.contains { $0.id == bobPost.id || $0.reblog?.id == bobPost.id } == true)
let local: [Status]? = await step("local", { try await alice.get(endpoint: Timelines.pub(sinceId: nil, maxId: nil, minId: nil, local: true, limit: 40)) })
check("local", (local?.count ?? 0) > 0)
let tag: [Status]? = await step("hashtag", { try await alice.get(endpoint: Timelines.hashtag(tag: "ios", additional: nil, maxId: nil, minId: nil)) })
check("hashtag timeline", tag?.first?.id == bobPost.id)
let acctSts: [Status]? = await step("account statuses (pinned)", {
  try await alice.get(endpoint: Accounts.statuses(id: bobAcc.id, sinceId: nil, tag: nil, onlyMedia: false, excludeReplies: true, excludeReblogs: false, pinned: true))
})
check("pinned", acctSts?.first?.id == bobPost.id)
let _: [Status]? = await step("bookmarks", { try await alice.get(endpoint: Accounts.bookmarks(sinceId: nil)) })
let _: [Status]? = await step("favourites", { try await alice.get(endpoint: Accounts.favorites(sinceId: nil)) })
let _: [Tag]? = await step("trending tags", { try await alice.get(endpoint: Trends.tags) })
let _: [Status]? = await step("trending posts", { try await alice.get(endpoint: Trends.statuses(offset: nil)) })
let _: [Card]? = await step("trending links", { try await alice.get(endpoint: Trends.links(offset: nil)) })
let link: [Status]? = await step("link timeline", { try await alice.get(endpoint: Timelines.link(url: URL(string: "https://example.org/")!, sinceId: nil, maxId: nil, minId: nil)) })
check("link timeline decodes", link != nil)

print("5. notifications (grouped, as Ice Cubes 4.3+ loads them)")
let grouped: GroupedNotificationsResults? = await step("grouped notifications", {
  try await bob2.get(endpoint: Notifications.notificationsV2(sinceId: nil, maxId: nil, types: nil, excludeTypes: nil, accountId: nil,
    groupedTypes: ["favourite", "reblog", "follow"], expandAccounts: "partial_avatars"))
})
if grouped == nil { await diagnose(GroupedNotificationsResults.self, "/api/v2/notifications?grouped_types[]=favourite&grouped_types[]=reblog&grouped_types[]=follow&expand_accounts=partial_avatars", env["ICE_TOKEN_BOB"]!) }
let types = Set(grouped?.notificationGroups.map(\.type) ?? [])
check("grouped types", ["follow", "favourite", "reblog", "mention", "quote"].allSatisfy { types.contains($0) }, "\(types)")
if let groups = grouped?.notificationGroups, groups.count > 1 {
  let firstPage = groups.prefix(2)
  let next: GroupedNotificationsResults? = await step("next page by most_recent_notification_id", {
    try await bob2.get(endpoint: Notifications.notificationsV2(sinceId: nil, maxId: String(firstPage.last!.mostRecentNotificationId), types: nil,
      excludeTypes: nil, accountId: nil, groupedTypes: ["favourite", "reblog", "follow"], expandAccounts: "partial_avatars"))
  })
  let keys = Set(next?.notificationGroups.map(\.groupKey) ?? [])
  check("paging continues without repeats", next != nil && keys.isDisjoint(with: firstPage.map(\.groupKey)) && keys.count == groups.count - 2,
    "\(keys.count) of \(groups.count - 2)")
  let newer: GroupedNotificationsResults? = await step("since_id", {
    try await bob2.get(endpoint: Notifications.notificationsV2(sinceId: String(groups[1].mostRecentNotificationId), maxId: nil, types: nil,
      excludeTypes: nil, accountId: nil, groupedTypes: ["favourite", "reblog", "follow"], expandAccounts: "partial_avatars"))
  })
  check("since_id returns only newer", newer?.notificationGroups.map(\.groupKey) == [groups[0].groupKey], "\(String(describing: newer?.notificationGroups.map(\.groupKey)))")
}
let v1notifs: [Models.Notification]? = await step("v1 notifications", { try await bob.get(endpoint: Notifications.notifications(minId: nil, maxId: nil, types: nil, limit: 30)) })
check("v1 notifications", (v1notifs?.count ?? 0) >= 4, "\(v1notifs?.count ?? -1)")
let policy: Models.NotificationsPolicy? = await step("notification policy", { try await bob2.get(endpoint: Notifications.policy) })
check("policy decodes", policy != nil)
let _: [NotificationsRequest]? = await step("notification requests", { try await bob.get(endpoint: Notifications.requests) })
let markers: [String: Marker]? = await step("markers", { try await bob.get(endpoint: Markers.markers) })
check("markers decode", markers != nil)

print("6. search, lists, filters, conversations")
let results: SearchResults? = await step("search", { try await alice2.get(endpoint: Search.search(query: "bob", type: nil, offset: nil, following: nil)) })
check("search finds bob", results?.accounts.contains { $0.id == bobAcc.id } == true)
let list: Models.List? = await step("create list", { try await alice.post(endpoint: Lists.createList(title: "friends", repliesPolicy: .list, exclusive: false)) })
if let list {
  let added = await step("add to list", { try await alice.post(endpoint: Lists.updateAccounts(listId: list.id, accounts: [bobAcc.id])) })
  check("add to list answers 200", added??.statusCode == 200)
  let members: [Account]? = await step("list accounts", { try await alice.get(endpoint: Lists.accounts(listId: list.id)) })
  check("list member", members?.first?.id == bobAcc.id)
}
let filter: ServerFilter? = await step("create filter", {
  try await alice2.post(endpoint: ServerFilters.createFilter(json: ServerFilterData(title: "coffee", context: [.home, .notifications], filterAction: .warn, expiresIn: nil)))
})
check("filter created", filter?.title == "coffee")
if let filter {
  let kw: ServerFilter.Keyword? = await step("add keyword", { try await alice2.post(endpoint: ServerFilters.addKeyword(filter: filter.id, keyword: "coffee", wholeWord: true)) })
  check("keyword", kw?.keyword == "coffee")
  let filters: [ServerFilter]? = await step("filters", { try await alice2.get(endpoint: ServerFilters.filters) })
  check("filters list", filters?.first?.keywords.first?.keyword == "coffee")
}
let dm: Status? = await step("direct message", {
  try await alice.post(endpoint: Statuses.postStatus(json: StatusData(status: "@bob psst", visibility: .direct)))
})
check("dm", dm?.visibility == .direct)
let convs: [Conversation]? = await step("conversations", { try await bob.get(endpoint: Conversations.conversations(maxId: nil)) })
check("conversation", convs?.first?.lastStatus?.id == dm?.id && convs?.first?.unread == true)
if let c = convs?.first {
  let read: Conversation? = await step("mark read", { try await bob.post(endpoint: Conversations.read(id: c.id)) })
  check("read", read?.unread == false)
}

print("7. media")
var png = Data([0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A])
png.append(Data(base64Encoded: env["ICE_PNG_REST"]!)!)
let media: MediaAttachment? = await step("upload", {
  try await alice2.mediaUpload(endpoint: Media.medias, version: .v2, method: "POST", mimeType: "image/png", filename: "file", data: png)
})
check("uploaded", media?.supportedType == .image, "\(String(describing: media))")
if let media {
  let withPic: Status? = await step("post with media", {
    try await alice.post(endpoint: Statuses.postStatus(json: StatusData(status: "pic", visibility: .pub, mediaIds: [media.id],
      mediaAttributes: [.init(id: media.id, description: "a red line", thumbnail: nil, focus: nil)])))
  })
  check("media attached", withPic?.mediaAttachments.first?.id == media.id)
}

print(failures == 0 ? "all Ice Cubes checks passed" : "\(failures) Ice Cubes checks FAILED")
exit(failures == 0 ? 0 : 1)
