package dlt

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
)

// HeaderRow is one header to register.
type HeaderRow struct {
	Header, Category, Status string
}

// TemplateRow is one content template to register.
type TemplateRow struct {
	TemplateID, Name, Category, Body, ConsentID, Status string
	Headers                                             []string
	URLs, APKs, Callbacks                               []string
}

// LineErrors lists every bad line of an import; nothing is imported.
type LineErrors []string

func (e LineErrors) Error() string { return strings.Join(e, "; ") }

// column aliases as operator DLT portal exports name them (compared without
// case, spaces, underscores or brackets).
var aliases = map[string][]string{
	"header":      {"header", "headers", "headername", "senderid", "sender", "headers"},
	"category":    {"category", "type", "headertype", "templatetype", "contenttype", "templatecategory"},
	"status":      {"status", "state"},
	"template_id": {"templateid", "contenttemplateid", "dlttemplateid", "dltcontenttemplateid"},
	"name":        {"name", "templatename"},
	"body":        {"body", "templatecontent", "content", "template", "message", "templatemessage"},
	"consent":     {"consenttemplateid", "consentid", "consent"},
	"urls":        {"urls", "url", "whitelistedurls"},
	"apks":        {"apks", "apk"},
	"callbacks":   {"callbacks", "callback", "callbacknumbers", "cbn"},
}

func colKey(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '_', '-', '(', ')', '.':
			return -1
		}
		return r
	}, strings.ToLower(strings.TrimSpace(strings.TrimPrefix(s, "\ufeff"))))
}

// readTable reads CSV with a header row and returns rows as field -> value.
func readTable(r io.Reader, required ...string) ([]map[string]string, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	recs, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("CSV: %v", err)
	}
	if len(recs) < 2 {
		return nil, errors.New("CSV needs a header row and at least one line")
	}
	idx := map[string]int{}
	for i, h := range recs[0] {
		k := colKey(h)
		for field, names := range aliases {
			for _, n := range names {
				if k == n {
					if _, dup := idx[field]; !dup {
						idx[field] = i
					}
				}
			}
		}
	}
	for _, f := range required {
		if _, ok := idx[f]; !ok {
			return nil, fmt.Errorf("CSV has no %s column", f)
		}
	}
	out := make([]map[string]string, 0, len(recs)-1)
	for _, rec := range recs[1:] {
		row := map[string]string{}
		for f, i := range idx {
			if i < len(rec) {
				row[f] = strings.TrimSpace(rec[i])
			}
		}
		out = append(out, row)
	}
	return out, nil
}

func normStatus(s string) (string, bool) {
	switch strings.ToLower(s) {
	case "", "active", "approved", "enabled":
		return "active", true
	case "suspended", "inactive", "deactivated", "blocked", "disabled", "rejected":
		return "suspended", true
	}
	return "", false
}

func list(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == ',' || r == '|' || r == '\n' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ParseHeaders reads header,category[,status] lines.
func ParseHeaders(r io.Reader) ([]HeaderRow, error) {
	rows, err := readTable(r, "header", "category")
	if err != nil {
		return nil, err
	}
	var bad LineErrors
	out := make([]HeaderRow, 0, len(rows))
	for i, row := range rows {
		n := i + 2
		h := HeaderRow{Header: strings.ToUpper(row["header"])}
		cat := row["category"]
		if strings.EqualFold(cat, "others") || strings.EqualFold(cat, "other") {
			cat = ServiceImplicit
		}
		var ok bool
		if h.Category, ok = NormCategory(cat); !ok {
			bad = append(bad, fmt.Sprintf("line %d: unknown category %q", n, row["category"]))
			continue
		}
		if !HeaderTypeOK(h.Header, h.Category) {
			bad = append(bad, fmt.Sprintf("line %d: header %q must be 6 letters (6 digits if promotional)", n, row["header"]))
			continue
		}
		if h.Status, ok = normStatus(row["status"]); !ok {
			bad = append(bad, fmt.Sprintf("line %d: unknown status %q", n, row["status"]))
			continue
		}
		out = append(out, h)
	}
	if len(bad) > 0 {
		return nil, bad
	}
	return out, nil
}

// ParseTemplates reads content templates; headers, urls, apks and
// callbacks are lists separated by ';' (or ',' or '|').
func ParseTemplates(r io.Reader) ([]TemplateRow, error) {
	rows, err := readTable(r, "template_id", "category", "header", "body")
	if err != nil {
		return nil, err
	}
	var bad LineErrors
	out := make([]TemplateRow, 0, len(rows))
	for i, row := range rows {
		n := i + 2
		t := TemplateRow{TemplateID: row["template_id"], Name: row["name"], Body: row["body"], ConsentID: row["consent"],
			Headers: list(strings.ToUpper(row["header"])), URLs: list(row["urls"]), APKs: list(row["apks"]), Callbacks: list(row["callbacks"])}
		var ok bool
		switch {
		case t.TemplateID == "":
			bad = append(bad, fmt.Sprintf("line %d: template ID is empty", n))
		case len(t.Headers) == 0:
			bad = append(bad, fmt.Sprintf("line %d: no header", n))
		default:
			if t.Category, ok = NormCategory(row["category"]); !ok {
				bad = append(bad, fmt.Sprintf("line %d: unknown category %q", n, row["category"]))
			} else if _, err := Compile(t.Body); err != nil {
				bad = append(bad, fmt.Sprintf("line %d: %v", n, err))
			} else if t.Category == ServiceExplicit && t.ConsentID == "" {
				bad = append(bad, fmt.Sprintf("line %d: service explicit template needs a consent template ID", n))
			} else if t.Status, ok = normStatus(row["status"]); !ok {
				bad = append(bad, fmt.Sprintf("line %d: unknown status %q", n, row["status"]))
			} else {
				if t.Name == "" {
					t.Name = t.TemplateID
				}
				out = append(out, t)
			}
		}
	}
	if len(bad) > 0 {
		return nil, bad
	}
	return out, nil
}

// StoreHeaders upserts headers under an entity.
func StoreHeaders(ctx context.Context, tx pgx.Tx, entityID string, rows []HeaderRow) error {
	for _, h := range rows {
		if _, err := tx.Exec(ctx, `INSERT INTO dlt_headers (entity_id, header, category, status) VALUES ($1, $2, $3, $4)
			ON CONFLICT (entity_id, header) DO UPDATE SET category = EXCLUDED.category, status = EXCLUDED.status`,
			entityID, h.Header, h.Category, h.Status); err != nil {
			return fmt.Errorf("header %s: %w", h.Header, err)
		}
	}
	return nil
}

// StoreTemplates upserts templates under an entity and replaces each
// template's headers and whitelist. Every header must already be
// registered under the entity.
func StoreTemplates(ctx context.Context, tx pgx.Tx, entityID string, rows []TemplateRow) error {
	hdr := map[string]string{}
	hr, err := tx.Query(ctx, `SELECT id, header, category FROM dlt_headers WHERE entity_id = $1`, entityID)
	if err != nil {
		return err
	}
	cats := map[string]string{}
	for hr.Next() {
		var id, h, cat string
		if err := hr.Scan(&id, &h, &cat); err != nil {
			return err
		}
		hdr[strings.ToUpper(h)], cats[strings.ToUpper(h)] = id, cat
	}
	if err := hr.Err(); err != nil {
		return err
	}
	var bad LineErrors
	for _, t := range rows {
		for _, h := range t.Headers {
			if hdr[h] == "" {
				bad = append(bad, fmt.Sprintf("template %s: header %s is not registered under this PE ID", t.TemplateID, h))
			} else if (cats[h] == Promotional) != (t.Category == Promotional) {
				bad = append(bad, fmt.Sprintf("template %s: %s template cannot use %s header %s", t.TemplateID, t.Category, cats[h], h))
			}
		}
	}
	if len(bad) > 0 {
		return bad
	}
	for _, t := range rows {
		var consent *string
		if t.ConsentID != "" {
			var cid string
			if err := tx.QueryRow(ctx, `INSERT INTO dlt_consent_templates (entity_id, consent_template_id, body) VALUES ($1, $2, '')
				ON CONFLICT (consent_template_id) DO UPDATE SET consent_template_id = EXCLUDED.consent_template_id
				RETURNING id`, entityID, t.ConsentID).Scan(&cid); err != nil {
				return fmt.Errorf("template %s consent: %w", t.TemplateID, err)
			}
			consent = &cid
		}
		var id, owner string
		if err := tx.QueryRow(ctx, `INSERT INTO dlt_templates (entity_id, template_id, name, body, category, consent_template_id, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (template_id) DO UPDATE SET name = EXCLUDED.name, body = EXCLUDED.body, category = EXCLUDED.category,
			  consent_template_id = EXCLUDED.consent_template_id, status = EXCLUDED.status
			RETURNING id, entity_id`, entityID, t.TemplateID, t.Name, t.Body, t.Category, consent, t.Status).Scan(&id, &owner); err != nil {
			return fmt.Errorf("template %s: %w", t.TemplateID, err)
		}
		if owner != entityID {
			return LineErrors{fmt.Sprintf("template %s is registered under another PE ID", t.TemplateID)}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM dlt_template_headers WHERE template_id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM dlt_template_whitelist WHERE template_id = $1`, id); err != nil {
			return err
		}
		for _, h := range t.Headers {
			if _, err := tx.Exec(ctx, `INSERT INTO dlt_template_headers (template_id, header_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, hdr[h]); err != nil {
				return err
			}
		}
		for kind, vals := range map[string][]string{"url": t.URLs, "apk": t.APKs, "callback": t.Callbacks} {
			for _, v := range vals {
				if _, err := tx.Exec(ctx, `INSERT INTO dlt_template_whitelist (template_id, kind, value) VALUES ($1, $2, $3)
					ON CONFLICT DO NOTHING`, id, kind, v); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
