package store

import (
	"context"
	"strings"
)

// Webhook is an outbound notification endpoint. URL and Secret are
// ciphertext (see WebhookAAD).
type Webhook struct {
	ID             int64
	Name           string
	URL            []byte
	URLHost        string
	Format         string // json | slack | wecom | feishu | dingtalk
	Events         []string
	Repos          []string
	Secret         []byte
	Enabled        bool
	LastStatus     string
	LastDeliveryAt int64
	CreatedAt      int64
}

// WebhookAAD binds a webhook's ciphertexts to it.
func (w *Webhook) WebhookAAD(field string) string { return "webhook:" + w.Name + ":" + field }

const webhookCols = `id, name, url, url_host, format, events, repos, secret, enabled, last_status, last_delivery_at, created_at`

func scanWebhook(sc interface{ Scan(...any) error }) (*Webhook, error) {
	var w Webhook
	var events, repos string
	var enabled int
	if err := sc.Scan(&w.ID, &w.Name, &w.URL, &w.URLHost, &w.Format, &events, &repos, &w.Secret, &enabled, &w.LastStatus, &w.LastDeliveryAt, &w.CreatedAt); err != nil {
		return nil, wrapErr(err)
	}
	w.Events, w.Repos, w.Enabled = splitList(events), splitList(repos), enabled != 0
	return &w, nil
}

func (s *Store) CreateWebhook(ctx context.Context, w *Webhook) error {
	w.CreatedAt = Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO webhooks (name, url, url_host, format, events, repos, secret, enabled, created_at)
VALUES (?,?,?,?,?,?,?,?,?)`, w.Name, w.URL, w.URLHost, w.Format, strings.Join(w.Events, ","), strings.Join(w.Repos, ","), w.Secret, b2i(w.Enabled), w.CreatedAt)
	if err != nil {
		return wrapErr(err)
	}
	w.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) GetWebhook(ctx context.Context, id int64) (*Webhook, error) {
	return scanWebhook(s.db.QueryRowContext(ctx, `SELECT `+webhookCols+` FROM webhooks WHERE id=?`, id))
}

func (s *Store) ListWebhooks(ctx context.Context) ([]*Webhook, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+webhookCols+` FROM webhooks ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Webhook
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) DeleteWebhook(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM webhooks WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetWebhookEnabled(ctx context.Context, id int64, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE webhooks SET enabled=? WHERE id=?`, b2i(enabled), id)
	return err
}

func (s *Store) RecordWebhookDelivery(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE webhooks SET last_status=?, last_delivery_at=? WHERE id=?`, status, Now(), id)
	return err
}
