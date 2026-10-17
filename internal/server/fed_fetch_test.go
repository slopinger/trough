package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/slopinger/trough/internal/ap"
	"github.com/slopinger/trough/internal/httpsig"
)

func TestFetchJSONResignsRedirect(t *testing.T) {
	app := testApp(t)
	app.cfg.Insecure = true
	app.client = app.newRemoteClient()
	actor := app.instanceActor()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signature, err := httpsig.ParseCavage(r)
		if err == nil {
			var signed string
			signed, err = httpsig.CheckCavage(r, signature, nil)
			if err == nil {
				err = httpsig.VerifyRSA(actor.PublicKey, signed, signature.Signature, signature.Algorithm)
			}
		}
		if err != nil {
			t.Logf("signature rejected at %s: %v", r.URL.RequestURI(), err)
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/target?version=2", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/activity+json")
		fmt.Fprint(w, `{"type":"Person","id":"https://remote.test/actor"}`)
	}))
	defer server.Close()
	result, final, err := app.fetchJSON(context.Background(), server.URL+"/start", ap.MimeType)
	if err != nil {
		t.Fatal(err)
	}
	if ap.Str(result, "type") != "Person" || final != server.URL+"/target?version=2" {
		t.Fatalf("unexpected result: %v, %q", result, final)
	}
}

func TestRemoteClientsRefuseBlockedRedirects(t *testing.T) {
	app := testApp(t)
	app.cfg.Insecure = true
	app.client = app.newRemoteClient()
	app.mediaClient = app.newRemoteClient()
	if _, err := app.db.Exec(`INSERT INTO domain_blocks (domain,reason,created_at) VALUES ('localhost','test',0)`); err != nil {
		t.Fatal(err)
	}
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		fmt.Fprint(w, `{}`)
	}))
	defer target.Close()
	blocked := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, blocked, http.StatusFound) }))
	defer origin.Close()
	for name, client := range map[string]*http.Client{"federation": app.client, "media": app.mediaClient} {
		t.Run(name, func(t *testing.T) {
			resp, err := client.Get(origin.URL)
			if resp != nil {
				resp.Body.Close()
			}
			if !errors.Is(err, errBlocked) {
				t.Fatalf("expected domain block, got %v", err)
			}
		})
	}
	if reached.Load() != 0 {
		t.Fatal("contacted blocked redirect target")
	}
}

func TestFetchJSONRedirectLimit(t *testing.T) {
	app := testApp(t)
	app.cfg.Insecure = true
	app.client = app.newRemoteClient()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, "/again", http.StatusFound)
	}))
	defer server.Close()
	_, _, err := app.fetchJSON(context.Background(), server.URL, ap.MimeType)
	if err == nil || !strings.Contains(err.Error(), "too many redirects") {
		t.Fatalf("redirect loop was not rejected: %v", err)
	}
	if requests.Load() != 5 {
		t.Fatalf("got %d requests, want 5", requests.Load())
	}
}

func TestFetchJSONResignsCrossHostRedirect(t *testing.T) {
	app := testApp(t)
	app.cfg.Insecure = true
	app.client = app.newRemoteClient()
	actor := app.instanceActor()
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signature, err := httpsig.ParseCavage(r)
		if err == nil {
			var signed string
			signed, err = httpsig.CheckCavage(r, signature, nil)
			if err == nil {
				err = httpsig.VerifyRSA(actor.PublicKey, signed, signature.Signature, signature.Algorithm)
			}
		}
		if err != nil {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"type":"Person"}`)
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/actor?ref=origin", http.StatusFound)
	}))
	defer origin.Close()
	_, final, err := app.fetchJSON(context.Background(), origin.URL+"/start", ap.MimeType)
	if err != nil {
		t.Fatal(err)
	}
	if final != destination.URL+"/actor?ref=origin" {
		t.Fatalf("unexpected final URL: %s", final)
	}
}
