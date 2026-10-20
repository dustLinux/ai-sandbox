package pool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

func logf(format string, args ...any) { log.Printf(format, args...) }

// notify records an event and, when configured, pushes it to Telegram.
func (p *Pool) notify(kind, ownerID string, chatID int64, message string) {
	ev := &Event{
		ID:      0,
		Kind:    kind,
		OwnerID: ownerID,
		ChatID:  chatID,
		Message: message,
		Created: time.Now().UTC(),
	}
	p.store.Update(func(d *data) {
		d.Seq++
		ev.ID = d.Seq
		d.Events = append(d.Events, ev)
		if len(d.Events) > 500 {
			d.Events = d.Events[len(d.Events)-500:]
		}
	})
	target := chatID
	if target == 0 {
		target = p.cfg.TelegramChat
	}
	if p.cfg.TelegramToken == "" || target == 0 {
		return
	}
	go p.sendTelegram(target, message)
}

func (p *Pool) sendTelegram(chatID int64, message string) {
	body, _ := json.Marshal(map[string]any{
		"chat_id":    chatID,
		"text":       message,
		"parse_mode": "HTML",
	})
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Post("https://api.telegram.org/bot"+p.cfg.TelegramToken+"/sendMessage",
		"application/json", bytes.NewReader(body))
	if err != nil {
		logf("telegram: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logf("telegram: status %d", resp.StatusCode)
	}
}

// Events returns the events after a given id.
func (p *Pool) Events(after int64) []Event {
	var out []Event
	p.store.View(func(d *data) {
		for _, e := range d.Events {
			if e.ID > after {
				out = append(out, *e)
			}
		}
	})
	return out
}

// Status is the public snapshot of the pool.
func (p *Pool) Status() map[string]any {
	out := map[string]any{}
	p.store.View(func(d *data) {
		accounts := make([]map[string]any, 0, len(d.Accounts))
		for _, a := range d.Accounts {
			accounts = append(accounts, map[string]any{
				"id": a.ID, "label": a.Label, "verified": a.Verified,
				"credit_usd": a.CreditUSD, "servers": a.Servers, "disabled": a.Disabled,
			})
		}
		servers := make([]map[string]any, 0, len(d.Servers))
		for _, s := range d.Servers {
			servers = append(servers, map[string]any{
				"id": s.ID, "vibe_id": s.VibeID, "url": s.URL, "state": s.State,
				"phase": s.Phase, "owner_id": s.OwnerID, "lease_id": s.LeaseID,
				"last_seen": s.LastSeen, "error": s.Error,
			})
		}
		queue := make([]map[string]any, 0, len(d.Queue))
		for _, q := range d.Queue {
			queue = append(queue, map[string]any{
				"id": q.ID, "owner_id": q.OwnerID, "state": q.State, "created": q.Created,
			})
		}
		leases := make([]map[string]any, 0, len(d.Leases))
		for _, l := range d.Leases {
			leases = append(leases, map[string]any{
				"id": l.ID, "server_id": l.ServerID, "owner_id": l.OwnerID,
				"state": l.State, "created": l.Created, "ended": l.Ended,
			})
		}
		out["accounts"] = accounts
		out["servers"] = servers
		out["queue"] = queue
		out["leases"] = leases
	})
	return out
}

func (p *Pool) Lease(leaseID string) (map[string]any, bool) {
	var out map[string]any
	p.store.View(func(d *data) {
		for _, l := range d.Leases {
			if l.ID != leaseID {
				continue
			}
			out = map[string]any{
				"id": l.ID, "server_id": l.ServerID, "owner_id": l.OwnerID,
				"state": l.State, "created": l.Created, "ended": l.Ended,
			}
			for _, s := range d.Servers {
				if s.ID == l.ServerID {
					out["url"] = s.URL
					out["phase"] = s.Phase
					out["server_state"] = s.State
				}
			}
		}
	})
	return out, out != nil
}

func fmtID(s string) string { return fmt.Sprintf("%q", s) }
