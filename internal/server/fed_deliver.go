package server

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"git.freifick.net/slop/trough/internal/ap"
	"git.freifick.net/slop/trough/internal/httpsig"
)

// Outgoing activities are queued in the deliveries table and POSTed by a
// background loop with exponential backoff.

var retryDelays = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour, 24 * time.Hour}

func (app *App) enqueue(signer *Account, inboxes []string, activity obj) {
	app.enqueueAt(signer, inboxes, activity, time.Now())
}

// enqueueAt queues a delivery that is sent no earlier than at.
func (app *App) enqueueAt(signer *Account, inboxes []string, activity obj, at time.Time) {
	body, err := json.Marshal(activity)
	if err != nil {
		log.Printf("deliver: marshal: %v", err)
		return
	}
	seen := map[string]bool{}
	now, due := time.Now().Unix(), at.Unix()
	n := 0
	for _, inbox := range inboxes {
		if inbox == "" || seen[inbox] || strings.HasPrefix(inbox, app.cfg.BaseURL()+"/") {
			continue
		}
		seen[inbox] = true
		if app.domainBlocked(ap.Host(inbox)) {
			continue
		}
		if _, err := app.db.Exec(`INSERT INTO deliveries (account_id,inbox,payload,next_try,created_at) VALUES (?,?,?,?,?)`,
			signer.ID, inbox, string(body), due, now); err != nil {
			log.Printf("deliver: queue: %v", err)
		}
		n++
	}
	if n > 0 {
		time.AfterFunc(time.Until(at), func() {
			select {
			case app.deliverKick <- struct{}{}:
			default:
			}
		})
	}
}

type delivery struct {
	ID        int64
	AccountID string
	Inbox     string
	Payload   string
	Attempts  int
}

func (app *App) deliveryLoop(ctx context.Context) {
	keys := map[string]*rsa.PrivateKey{}
	var keysMu sync.Mutex
	keyFor := func(accountID string) (*Account, *rsa.PrivateKey, error) {
		acc, err := app.AccountByID(accountID)
		if err != nil {
			return nil, nil, err
		}
		keysMu.Lock()
		defer keysMu.Unlock()
		if k, ok := keys[accountID]; ok {
			return acc, k, nil
		}
		k, err := httpsig.ParsePrivateKey(acc.PrivateKey)
		if err != nil {
			return nil, nil, err
		}
		keys[accountID] = k
		return acc, k, nil
	}
	for {
		rows, err := app.db.Query(`SELECT id,account_id,inbox,payload,attempts FROM deliveries WHERE next_try <= ? ORDER BY next_try LIMIT 64`, time.Now().Unix())
		var batch []delivery
		if err == nil {
			for rows.Next() {
				var d delivery
				if rows.Scan(&d.ID, &d.AccountID, &d.Inbox, &d.Payload, &d.Attempts) == nil {
					batch = append(batch, d)
				}
			}
			rows.Close()
		}
		if len(batch) > 0 {
			sem := make(chan struct{}, 8)
			var wg sync.WaitGroup
			for _, d := range batch {
				// claim it so a slow batch doesn't get picked up again
				app.db.Exec(`UPDATE deliveries SET next_try=? WHERE id=?`, time.Now().Add(10*time.Minute).Unix(), d.ID)
				wg.Add(1)
				sem <- struct{}{}
				go func(d delivery) {
					defer wg.Done()
					defer func() { <-sem }()
					acc, key, err := keyFor(d.AccountID)
					if err != nil {
						app.db.Exec(`DELETE FROM deliveries WHERE id=?`, d.ID)
						return
					}
					app.finishDelivery(d, app.post(ctx, acc, key, d.Inbox, []byte(d.Payload)))
				}(d)
			}
			wg.Wait()
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-app.deliverKick:
		case <-time.After(15 * time.Second):
		}
	}
}

type permanentError struct{ error }

func (app *App) post(ctx context.Context, signer *Account, key *rsa.PrivateKey, inbox string, body []byte) error {
	u, err := app.checkRemoteURL(inbox)
	if err != nil {
		return permanentError{err}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	style := app.sigStyle(u.Host)
	code, status, msg, err := app.postOnce(ctx, u.String(), body, signer.PublicKeyID, key, style)
	if style == sigStyleRFC9421 && (code == http.StatusUnauthorized || code == http.StatusForbidden) {
		// double-knock: they don't take RFC 9421 (from us) after all
		app.rejectedRFC9421(u.Host)
		code, status, msg, err = app.postOnce(ctx, u.String(), body, signer.PublicKeyID, key, sigStyleCavage)
	}
	if err != nil {
		return err
	}
	if code/100 == 2 {
		return nil
	}
	err = fmt.Errorf("%s: %s", status, strings.TrimSpace(msg))
	if code/100 == 4 && code != http.StatusTooManyRequests && code != http.StatusRequestTimeout {
		return permanentError{err}
	}
	return err
}

func (app *App) postOnce(ctx context.Context, inbox string, body []byte, keyID string, key *rsa.PrivateKey, style string) (int, string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, inbox, bytes.NewReader(body))
	if err != nil {
		return 0, "", "", permanentError{err}
	}
	req.Header.Set("Content-Type", ap.MimeType)
	req.Header.Set("Accept", ap.MimeType)
	req.Header.Set("User-Agent", app.userAgent())
	sign := httpsig.SignCavage
	if style == sigStyleRFC9421 {
		sign = httpsig.SignRFC9421
	}
	if err := sign(req, body, keyID, key); err != nil {
		return 0, "", "", permanentError{err}
	}
	resp, err := app.client.Do(req)
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	return resp.StatusCode, resp.Status, string(msg), nil
}

func (app *App) finishDelivery(d delivery, err error) {
	if err == nil {
		app.db.Exec(`DELETE FROM deliveries WHERE id=?`, d.ID)
		return
	}
	_, permanent := err.(permanentError)
	if permanent || d.Attempts >= len(retryDelays) {
		log.Printf("deliver to %s failed permanently: %v", d.Inbox, err)
		app.db.Exec(`DELETE FROM deliveries WHERE id=?`, d.ID)
		return
	}
	next := time.Now().Add(retryDelays[d.Attempts])
	app.db.Exec(`UPDATE deliveries SET attempts=attempts+1, next_try=?, last_error=? WHERE id=?`, next.Unix(), truncateRunes(err.Error(), 300), d.ID)
}
