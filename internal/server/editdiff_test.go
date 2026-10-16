package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestWordDiff(t *testing.T) {
	render := func(ps []DiffPart) string {
		var b strings.Builder
		for _, p := range ps {
			switch p.Op {
			case 1:
				b.WriteString("[+" + p.Text + "]")
			case -1:
				b.WriteString("[-" + p.Text + "]")
			default:
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	for _, c := range []struct{ a, b, want string }{
		{"the quick fox", "the quick fox", "the quick fox"},
		{"frist post", "first post", "[-frist][+first] post"},
		{"hello world", "hello brave new world", "hello [+brave new ]world"},
		{"a b c d", "a d", "a [-b c ]d"},
		{"", "new", "[+new]"},
		{"old", "", "[-old]"},
	} {
		got := render(wordDiff(c.a, c.b))
		if got != c.want {
			t.Errorf("wordDiff(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
		if a, b := diffWords(wordDiff(c.a, c.b)); c.a == c.b && (a != 0 || b != 0) {
			t.Error("identical texts have changes")
		}
	}
	long := strings.Repeat("word ", 4000)
	if parts := wordDiff(long+"x", "y"+long); len(parts) == 0 {
		t.Error("no diff for long texts")
	}
}

func TestEditHistoryPage(t *testing.T) {
	la, _, bob, _ := featureFixture(t)
	ctx := context.Background()
	st, _ := la.PostStatus(ctx, bob, PostInput{Text: "frist post here", Visibility: "public"})
	full, _ := la.loadStatus(bob, st.ID)
	if _, err := la.EditStatus(ctx, bob, full, "first post here, edited", "", true); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(la.base + "/s/" + st.ID + "/history")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(b)
	for _, want := range []string{"Current version", "Original", "<del>frist</del>", "<ins>first</ins>", "+3", "−2", "marked sensitive", "Show as posted"} {
		if !strings.Contains(page, want) {
			t.Errorf("history page lacks %q", want)
		}
	}
	if strings.Index(page, "Current version") > strings.Index(page, "Original") {
		t.Error("history isn't newest first")
	}
}

func TestWordDiffGroupsChanges(t *testing.T) {
	got := wordDiff("hot take: the trough is full", "trying out the new page")
	want := []DiffPart{{"hot take:", -1}, {"trying out", 1}, {" the ", 0}, {"trough is full", -1}, {"new page", 1}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %+v", got)
		}
	}
}
