package server

import (
	"regexp"
	"strconv"
	"strings"
)

// Word-level differences between versions of an edited post, for the edit
// history page. Versions are compared as plain text.

type DiffPart struct {
	Text string
	Op   int // 0 unchanged, 1 added, -1 removed
}

var diffTokenRe = regexp.MustCompile(`\s+|[^\s]+`)

// maxDiffTokens bounds the changed middle part that gets a real comparison;
// anything bigger is shown as removed and added as a whole.
const maxDiffTokens = 1500

func wordDiff(a, b string) []DiffPart {
	x, y := diffTokenRe.FindAllString(a, -1), diffTokenRe.FindAllString(b, -1)
	pre := 0
	for pre < len(x) && pre < len(y) && x[pre] == y[pre] {
		pre++
	}
	suf := 0
	for suf < len(x)-pre && suf < len(y)-pre && x[len(x)-1-suf] == y[len(y)-1-suf] {
		suf++
	}
	var out []DiffPart
	add := func(op int, toks ...string) {
		for _, t := range toks {
			if n := len(out); n > 0 && out[n-1].Op == op {
				out[n-1].Text += t
			} else {
				out = append(out, DiffPart{Text: t, Op: op})
			}
		}
	}
	add(0, x[:pre]...)
	mx, my := x[pre:len(x)-suf], y[pre:len(y)-suf]
	if len(mx) > maxDiffTokens || len(my) > maxDiffTokens {
		add(-1, mx...)
		add(1, my...)
	} else {
		// longest common subsequence, then walk it
		w := len(my) + 1
		l := make([]uint16, (len(mx)+1)*w)
		for i := len(mx) - 1; i >= 0; i-- {
			for j := len(my) - 1; j >= 0; j-- {
				if mx[i] == my[j] {
					l[i*w+j] = l[(i+1)*w+j+1] + 1
				} else if a, b := l[(i+1)*w+j], l[i*w+j+1]; a >= b {
					l[i*w+j] = a
				} else {
					l[i*w+j] = b
				}
			}
		}
		i, j := 0, 0
		for i < len(mx) && j < len(my) {
			switch {
			case mx[i] == my[j]:
				add(0, mx[i])
				i, j = i+1, j+1
			case l[(i+1)*w+j] >= l[i*w+j+1]:
				add(-1, mx[i])
				i++
			default:
				add(1, my[j])
				j++
			}
		}
		add(-1, mx[i:]...)
		add(1, my[j:]...)
	}
	add(0, x[len(x)-suf:]...)
	return groupChanges(out)
}

// groupChanges makes runs of changes separated only by spaces read as one
// removed block followed by one added block, instead of alternating words.
func groupChanges(parts []DiffPart) []DiffPart {
	var out []DiffPart
	var del, ins strings.Builder
	flush := func() {
		if del.Len() > 0 {
			out = append(out, DiffPart{Text: del.String(), Op: -1})
		}
		if ins.Len() > 0 {
			out = append(out, DiffPart{Text: ins.String(), Op: 1})
		}
		del.Reset()
		ins.Reset()
	}
	for i, p := range parts {
		switch {
		case p.Op < 0:
			del.WriteString(p.Text)
		case p.Op > 0:
			ins.WriteString(p.Text)
		case strings.TrimSpace(p.Text) == "" && del.Len()+ins.Len() > 0 && i+1 < len(parts):
			del.WriteString(p.Text) // a space between two changes belongs to both
			ins.WriteString(p.Text)
		default:
			flush()
			out = append(out, p)
		}
	}
	flush()
	return out
}

// diffWords counts the words added and removed in a diff.
func diffWords(parts []DiffPart) (added, removed int) {
	for _, p := range parts {
		n := len(strings.Fields(p.Text))
		if p.Op > 0 {
			added += n
		} else if p.Op < 0 {
			removed += n
		}
	}
	return
}

// HistoryEntry is one version on the edit history page, with what changed
// compared to the version before it.
type HistoryEntry struct {
	StatusVersion
	Label            string
	Diff, CWDiff     []DiffPart
	Added, Removed   int
	SensitiveChanged bool
	First, Current   bool
}

// historyEntries returns the versions newest first, each compared to the one before.
func historyEntries(vs []StatusVersion) []HistoryEntry {
	out := make([]HistoryEntry, 0, len(vs))
	for i := len(vs) - 1; i >= 0; i-- {
		e := HistoryEntry{StatusVersion: vs[i], First: i == 0, Current: i == len(vs)-1}
		switch {
		case i == 0:
			e.Label = "Original"
		case e.Current:
			e.Label = "Current version"
		default:
			e.Label = "Edit " + strconv.Itoa(i)
		}
		if i > 0 {
			prev := vs[i-1]
			e.Diff = wordDiff(plainText(prev.Content), plainText(vs[i].Content))
			if prev.Spoiler != vs[i].Spoiler {
				e.CWDiff = wordDiff(prev.Spoiler, vs[i].Spoiler)
			}
			e.Added, e.Removed = diffWords(append(append([]DiffPart{}, e.Diff...), e.CWDiff...))
			e.SensitiveChanged = prev.Sensitive != vs[i].Sensitive
		}
		out = append(out, e)
	}
	return out
}
