package server

import (
	"strings"
	"time"
	"unicode"
)

// statusQuery is a post search with Mastodon-style operators:
//
//	words "exact phrase" -excluded
//	from:me from:user from:user@host   has:media|image|video|audio|poll|link|embed
//	is:reply is:sensitive   language:de   before:/after:/during:YYYY-MM-DD   in:library|all
//
// Operators can be negated with a leading "-". Unknown operators are searched as text.
type statusQuery struct {
	Terms, Exclude []string
	Ops            []searchOp
	Library        bool
}

type searchOp struct {
	Key, Value string
	Neg        bool
}

var searchOpKeys = map[string]bool{"from": true, "has": true, "is": true, "language": true, "before": true, "after": true, "during": true, "in": true}

func parseStatusQuery(q string) statusQuery {
	var sq statusQuery
	for _, tok := range splitSearchTokens(q) {
		neg := false
		word := tok.text
		if word == "-" {
			continue
		}
		if !tok.quoted && strings.HasPrefix(word, "-") && len(word) > 1 {
			neg, word = true, word[1:]
		} else if tok.neg {
			neg = true
		}
		if !tok.quoted {
			if k, v, ok := strings.Cut(word, ":"); ok && v != "" && searchOpKeys[strings.ToLower(k)] {
				k, v = strings.ToLower(k), strings.Trim(v, `"`)
				if k == "in" {
					sq.Library = strings.EqualFold(v, "library")
					continue
				}
				sq.Ops = append(sq.Ops, searchOp{Key: k, Value: v, Neg: neg})
				continue
			}
		}
		if neg {
			sq.Exclude = append(sq.Exclude, word)
		} else {
			sq.Terms = append(sq.Terms, word)
		}
	}
	return sq
}

// HasOperators reports whether the query uses anything beyond plain words.
func (sq statusQuery) HasOperators() bool {
	return len(sq.Ops) > 0 || sq.Library || len(sq.Exclude) > 0
}

// Text is the positive words joined, used for account and hashtag search alongside posts.
func (sq statusQuery) Text() string { return strings.Join(sq.Terms, " ") }

type searchToken struct {
	text        string
	quoted, neg bool
}

func splitSearchTokens(q string) []searchToken {
	var out []searchToken
	rs := []rune(q)
	for i := 0; i < len(rs); {
		if unicode.IsSpace(rs[i]) {
			i++
			continue
		}
		neg := false
		if rs[i] == '-' && i+1 < len(rs) && rs[i+1] == '"' {
			neg, i = true, i+1
		}
		if rs[i] == '"' {
			end := i + 1
			for end < len(rs) && rs[end] != '"' {
				end++
			}
			if phrase := strings.TrimSpace(string(rs[i+1 : end])); phrase != "" {
				out = append(out, searchToken{text: phrase, quoted: true, neg: neg})
			}
			i = end + 1
			continue
		}
		start := i
		inQuote := false
		for i < len(rs) && (inQuote || !unicode.IsSpace(rs[i])) {
			if rs[i] == '"' {
				inQuote = !inQuote
			}
			i++
		}
		out = append(out, searchToken{text: string(rs[start:i])})
	}
	return out
}

func likePattern(s string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(s) + "%"
}

// SQL turns the query into a condition on statuses s for viewer v. ok is false when the
// query has nothing to search for, so an empty or operator-free query never lists every post.
func (app *App) statusQuerySQL(v *Account, sq statusQuery) (where string, args []any, ok bool) {
	conds := []string{`s.boost_of_id=''`}
	textMatch := `(s.content LIKE ? ESCAPE '\' OR s.spoiler LIKE ? ESCAPE '\' OR EXISTS(SELECT 1 FROM attachments t WHERE t.status_id=s.id AND t.description LIKE ? ESCAPE '\'))`
	for _, t := range sq.Terms {
		p := likePattern(t)
		conds, args = append(conds, textMatch), append(args, p, p, p)
	}
	for _, t := range sq.Exclude {
		p := likePattern(t)
		conds, args = append(conds, "NOT "+textMatch), append(args, p, p, p)
	}
	filters := 0
	var fromIn, fromOut []any // from: accounts, OR'ed together
	for _, op := range sq.Ops {
		var c string
		var a []any
		switch op.Key {
		case "from":
			id := app.searchFromAccount(v, op.Value)
			if id == "" && !op.Neg {
				return "", nil, false
			}
			if id != "" && op.Neg {
				fromOut = append(fromOut, id)
			} else if id != "" {
				fromIn = append(fromIn, id)
			}
			filters++
			continue
		case "has":
			switch strings.ToLower(op.Value) {
			case "media":
				c = `EXISTS(SELECT 1 FROM attachments t WHERE t.status_id=s.id)`
			case "image", "video", "audio":
				c, a = `EXISTS(SELECT 1 FROM attachments t WHERE t.status_id=s.id AND t.media_type LIKE ?)`, []any{strings.ToLower(op.Value) + "/%"}
			case "poll":
				c = `s.poll != ''`
			case "link", "embed":
				c = `s.card_url != ''`
			default:
				continue
			}
		case "is":
			switch strings.ToLower(op.Value) {
			case "reply":
				c = `s.in_reply_to_uri != ''`
			case "sensitive":
				c = `(s.sensitive=1 OR s.spoiler != '')`
			default:
				continue
			}
		case "language":
			c, a = `(s.language=? OR s.language LIKE ?)`, []any{strings.ToLower(op.Value), strings.ToLower(op.Value) + "-%"}
		case "before", "after", "during":
			day, err := time.Parse("2006-01-02", op.Value)
			if err != nil {
				continue
			}
			switch op.Key {
			case "before":
				c, a = `s.created_at < ?`, []any{day.Unix()}
			case "after":
				c, a = `s.created_at >= ?`, []any{day.AddDate(0, 0, 1).Unix()}
			default:
				c, a = `(s.created_at >= ? AND s.created_at < ?)`, []any{day.Unix(), day.AddDate(0, 0, 1).Unix()}
			}
		}
		if op.Neg {
			c = "NOT " + c
		}
		conds, args = append(conds, c), append(args, a...)
		filters++
	}
	if len(fromIn) > 0 {
		conds, args = append(conds, `s.account_id IN (`+placeholders(len(fromIn))+`)`), append(args, fromIn...)
	}
	if len(fromOut) > 0 {
		conds, args = append(conds, `s.account_id NOT IN (`+placeholders(len(fromOut))+`)`), append(args, fromOut...)
	}
	if len(sq.Terms) == 0 && filters == 0 && !sq.Library {
		return "", nil, false
	}
	// Scope, like Mastodon 4.2: public posts of accounts that opted into search
	// (indexable) and everything you wrote, liked, boosted, bookmarked or were
	// mentioned in; in:library only the latter (visibility is still checked).
	library := `(s.account_id=? OR EXISTS(SELECT 1 FROM likes l WHERE l.status_id=s.id AND l.account_id=?)
			OR EXISTS(SELECT 1 FROM bookmarks b2 WHERE b2.status_id=s.id AND b2.account_id=?)
			OR EXISTS(SELECT 1 FROM mentions m2 WHERE m2.status_id=s.id AND m2.account_id=?)
			OR EXISTS(SELECT 1 FROM statuses rb WHERE rb.boost_of_id=s.id AND rb.account_id=?))`
	if v == nil {
		if sq.Library {
			return "", nil, false
		}
		conds = append(conds, `s.visibility IN ('public','unlisted') AND a.indexable=1 AND `+notLimitedSQL)
	} else if sq.Library {
		conds = append(conds, library)
		args = append(args, v.ID, v.ID, v.ID, v.ID, v.ID)
	} else {
		conds = append(conds, `((s.visibility IN ('public','unlisted') AND a.indexable=1 AND `+notLimitedSQL+`) OR `+library+`)`)
		args = append(args, v.ID, v.ID, v.ID, v.ID, v.ID)
	}
	return strings.Join(conds, " AND "), args, true
}

// searchFromAccount resolves the from: operator against known accounts, without network lookups.
func (app *App) searchFromAccount(v *Account, handle string) string {
	if strings.EqualFold(handle, "me") {
		return viewerID(v)
	}
	user, domain, _ := strings.Cut(strings.TrimPrefix(handle, "@"), "@")
	if strings.EqualFold(domain, app.cfg.Host) {
		domain = ""
	}
	var id string
	app.db.QueryRow(`SELECT id FROM accounts WHERE username=? COLLATE NOCASE AND domain=? AND suspended=0`, user, strings.ToLower(domain)).Scan(&id)
	return id
}

// searchStatuses runs a post search for the web UI and the API.
func (app *App) searchStatuses(v *Account, q, accountID, maxID string, limit int) []*Status {
	sq := parseStatusQuery(q)
	where, args, ok := app.statusQuerySQL(v, sq)
	if !ok {
		return nil
	}
	if accountID != "" {
		where += ` AND s.account_id=?`
		args = append(args, accountID)
	}
	sts, _ := app.queryStatuses(v, where, args, maxID, limit, false)
	return sts
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }
