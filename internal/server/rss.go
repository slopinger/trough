package server

import (
	"encoding/xml"
	"net/http"
	"strings"
	"time"
)

// RSS feeds of a local account's public posts (/@name.rss) and of a
// hashtag (/tags/name.rss), as Mastodon has them. Replies and boosts are
// left out of profile feeds.

type rssFeed struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Media   string     `xml:"xmlns:media,attr"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Image       *rssImage `xml:"image,omitempty"`
	LastBuild   string    `xml:"lastBuildDate,omitempty"`
	Generator   string    `xml:"generator"`
	Items       []rssItem `xml:"item"`
}

type rssImage struct {
	URL   string `xml:"url"`
	Title string `xml:"title"`
	Link  string `xml:"link"`
}

type rssItem struct {
	GUID        string     `xml:"guid"`
	Link        string     `xml:"link"`
	PubDate     string     `xml:"pubDate"`
	Description string     `xml:"description"`
	Categories  []string   `xml:"category"`
	Media       []rssMedia `xml:"media:content"`
}

type rssMedia struct {
	URL    string `xml:"url,attr"`
	Type   string `xml:"type,attr"`
	Medium string `xml:"medium,attr"`
	Desc   string `xml:"media:description,omitempty"`
}

func (app *App) rssItems(sts []*Status) []rssItem {
	var items []rssItem
	for _, s := range sts {
		desc := s.Content
		if s.Spoiler != "" {
			desc = "<p><strong>" + xmlText(s.Spoiler) + "</strong></p>" + desc
		}
		it := rssItem{GUID: s.URI, Link: s.URL, PubDate: s.CreatedAt.UTC().Format(time.RFC1123Z), Description: desc}
		if it.Link == "" {
			it.Link = s.URI
		}
		it.Categories = s.Tags
		for _, at := range s.Attachments {
			it.Media = append(it.Media, rssMedia{URL: app.absMedia(at.URL), Type: at.MediaType, Medium: at.Kind(), Desc: at.Description})
		}
		items = append(items, it)
	}
	return items
}

func xmlText(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func writeRSS(w http.ResponseWriter, ch rssChannel) {
	ch.Generator = SoftwareName + " " + SoftwareVersion
	if len(ch.Items) > 0 {
		ch.LastBuild = ch.Items[0].PubDate
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	enc.Encode(rssFeed{Version: "2.0", Media: "http://search.yahoo.com/mrss/", Channel: ch})
}

// handleProfileRSS serves /@name.rss for local accounts.
func (app *App) handleProfileRSS(w http.ResponseWriter, r *http.Request, acc *Account) {
	sts, _ := app.queryStatuses(nil, `s.account_id=? AND s.visibility='public' AND s.boost_of_id='' AND s.in_reply_to_uri=''`,
		[]any{acc.ID}, "", 20, false)
	ch := rssChannel{Title: acc.Name(), Link: app.cfg.BaseURL() + acc.Path(), Description: "Public posts from @" + acc.Acct(),
		Items: app.rssItems(sts)}
	if acc.Avatar != "" {
		ch.Image = &rssImage{URL: app.absMedia(acc.Avatar), Title: ch.Title, Link: ch.Link}
	}
	writeRSS(w, ch)
}

// handleTagRSS serves /tags/name.rss.
func (app *App) handleTagRSS(w http.ResponseWriter, r *http.Request, tag string) {
	sts, _ := app.tagTimeline(nil, tag, "")
	writeRSS(w, rssChannel{Title: "#" + tag, Link: app.cfg.BaseURL() + "/tags/" + tag,
		Description: "Public posts tagged #" + tag + " on " + app.cfg.Host, Items: app.rssItems(sts)})
}
