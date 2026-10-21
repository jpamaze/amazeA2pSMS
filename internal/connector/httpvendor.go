package connector

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/jpamaze/amazeA2pSMS/internal/domain"
	"github.com/jpamaze/amazeA2pSMS/internal/smpp/gsm7"
)

// HTTPProfile describes one HTTP vendor (an http_vendor_profiles row).
// Placeholders in request values: {{id}} {{to}} {{to_plus}} {{to_national}}
// {{sender}} {{text}} {{pe_id}} {{template_id}} {{tm_chain}} {{unicode}}
// {{segments}} {{secret}}.
type HTTPProfile struct {
	BaseURL     string
	Auth        HTTPAuth
	Request     HTTPRequest
	Response    HTTPResponse
	DLR         HTTPDLR
	Secret      string // decrypted secret_enc, used by Auth and {{secret}}
	DLRToken    string
	TPS         int
	Concurrency int
}

// HTTPAuth says how the secret is presented.
type HTTPAuth struct {
	Type     string `json:"type"`     // none | bearer | basic | header | query
	Name     string `json:"name"`     // header or query parameter name (header/query)
	Username string `json:"username"` // basic: the secret is the password
}

// HTTPRequest is the submit request template.
type HTTPRequest struct {
	Method   string            `json:"method"`   // POST (default) or GET
	Path     string            `json:"path"`     // appended to the base URL
	Encoding string            `json:"encoding"` // json (default) | form | query
	Params   map[string]string `json:"params"`   // field -> template
	Headers  map[string]string `json:"headers"`
}

// HTTPResponse says how to read the submit response.
type HTTPResponse struct {
	Format        string   `json:"format"`         // json (default) | text
	SuccessPath   string   `json:"success_path"`   // json: field that signals success
	SuccessValues []string `json:"success_values"` // accepted values (case-insensitive)
	IDPath        string   `json:"id_path"`        // json: vendor message ID
	ErrorPath     string   `json:"error_path"`     // json: vendor error code
	IDRegex       string   `json:"id_regex"`       // text: first capture group is the ID
}

// HTTPDLR says how to read the vendor's delivery callback.
type HTTPDLR struct {
	IDParam            string   `json:"id_param"`
	StatusParam        string   `json:"status_param"`
	ErrorParam         string   `json:"error_param"`
	DeliveredValues    []string `json:"delivered_values"`
	ExpiredValues      []string `json:"expired_values"`
	IntermediateValues []string `json:"intermediate_values"` // e.g. SENT, ACCEPTED: acknowledged and ignored
}

// ParseHTTPProfile decodes the JSON columns of a profile.
func ParseHTTPProfile(baseURL string, auth, req, resp, dlr []byte) (HTTPProfile, error) {
	p := HTTPProfile{BaseURL: strings.TrimRight(baseURL, "/")}
	for name, pair := range map[string]struct {
		b []byte
		v any
	}{"auth": {auth, &p.Auth}, "request_template": {req, &p.Request}, "response_mapping": {resp, &p.Response}, "dlr_mapping": {dlr, &p.DLR}} {
		if len(pair.b) == 0 {
			continue
		}
		if err := json.Unmarshal(pair.b, pair.v); err != nil {
			return p, fmt.Errorf("http profile %s: %w", name, err)
		}
	}
	if p.Request.Method == "" {
		p.Request.Method = http.MethodPost
	}
	if p.Request.Encoding == "" {
		p.Request.Encoding = "json"
	}
	if p.Response.Format == "" {
		p.Response.Format = "json"
	}
	if p.Response.Format == "json" && p.Response.IDPath == "" {
		return p, errors.New("http profile: response_mapping.id_path is required for json responses")
	}
	if p.Response.Format == "text" {
		if _, err := regexp.Compile(p.Response.IDRegex); err != nil || p.Response.IDRegex == "" {
			return p, fmt.Errorf("http profile: response_mapping.id_regex is invalid: %v", err)
		}
	}
	if p.DLR.IDParam == "" || p.DLR.StatusParam == "" {
		return p, errors.New("http profile: dlr_mapping.id_param and status_param are required")
	}
	return p, nil
}

// HTTPTransport sends messages to an HTTP vendor. Long messages are sent
// whole; the vendor splits them, so each message has one vendor ID.
type HTTPTransport struct {
	Profile HTTPProfile
	Client  *http.Client
	limiter *rate.Limiter
	idRe    *regexp.Regexp
}

// NewHTTPTransport returns a transport for p.
func NewHTTPTransport(p HTTPProfile) *HTTPTransport {
	if p.TPS <= 0 {
		p.TPS = 100
	}
	if p.Concurrency <= 0 {
		p.Concurrency = 50
	}
	t := &HTTPTransport{
		Profile: p,
		limiter: rate.NewLimiter(rate.Limit(p.TPS), max(1, p.TPS/20)),
		Client: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				MaxIdleConnsPerHost: p.Concurrency,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 5 * time.Second,
			},
		},
	}
	if p.Response.Format == "text" {
		t.idRe = regexp.MustCompile(p.Response.IDRegex)
	}
	return t
}

// Window implements Transport.
func (t *HTTPTransport) Window() int { return t.Profile.Concurrency }

// Send implements Transport.
func (t *HTTPTransport) Send(ctx context.Context, m *domain.Message) ([]string, error) {
	id, err := withRetry(ctx, httpTemporary, func() (string, error) {
		if err := t.limiter.Wait(ctx); err != nil {
			return "", err
		}
		return t.submit(ctx, m)
	})
	if err != nil {
		return nil, err
	}
	return []string{id}, nil
}

func httpTemporary(err error) bool {
	var se *SendError
	if errors.As(err, &se) {
		return se.Temporary
	}
	return true // transport errors: connection refused, timeouts
}

func (t *HTTPTransport) vars(m *domain.Message) map[string]string {
	unicode := "0"
	if gsm7.PlanFor(m.Text).Coding == gsm7.CodingUCS2 {
		unicode = "1"
	}
	national := m.Destination
	if strings.HasPrefix(national, "91") && len(national) == 12 {
		national = national[2:]
	}
	return map[string]string{
		"id": m.ID, "to": m.Destination, "to_plus": "+" + m.Destination, "to_national": national,
		"sender": m.Sender, "text": m.Text, "pe_id": m.DLT.PEID, "template_id": m.DLT.TemplateID,
		"tm_chain": m.DLT.TMChain, "unicode": unicode, "segments": strconv.Itoa(m.Segments),
		"secret": t.Profile.Secret,
	}
}

var placeholder = regexp.MustCompile(`\{\{\s*([a-z_]+)\s*\}\}`)

func expand(tmpl string, vars map[string]string) string {
	return placeholder.ReplaceAllStringFunc(tmpl, func(s string) string {
		return vars[placeholder.FindStringSubmatch(s)[1]]
	})
}

func (t *HTTPTransport) buildRequest(ctx context.Context, m *domain.Message) (*http.Request, error) {
	p := t.Profile
	vars := t.vars(m)
	params := map[string]string{}
	for k, v := range p.Request.Params {
		params[k] = expand(v, vars)
	}
	u, err := url.Parse(p.BaseURL + p.Request.Path)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	var body io.Reader
	contentType := ""
	switch p.Request.Encoding {
	case "query":
		for k, v := range params {
			q.Set(k, v)
		}
	case "form":
		form := url.Values{}
		for k, v := range params {
			form.Set(k, v)
		}
		body, contentType = strings.NewReader(form.Encode()), "application/x-www-form-urlencoded"
	default:
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		body, contentType = bytes.NewReader(b), "application/json"
	}
	if p.Auth.Type == "query" {
		q.Set(p.Auth.Name, p.Secret)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, p.Request.Method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json, text/plain")
	for k, v := range p.Request.Headers {
		req.Header.Set(k, expand(v, vars))
	}
	switch p.Auth.Type {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+p.Secret)
	case "basic":
		req.SetBasicAuth(p.Auth.Username, p.Secret)
	case "header":
		req.Header.Set(p.Auth.Name, p.Secret)
	}
	return req, nil
}

func (t *HTTPTransport) submit(ctx context.Context, m *domain.Message) (string, error) {
	req, err := t.buildRequest(ctx, m)
	if err != nil {
		return "", &SendError{Code: "HTTP_BAD_REQUEST_TEMPLATE", Err: err}
	}
	resp, err := t.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))

	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return "", &SendError{Code: fmt.Sprintf("HTTP_%d", resp.StatusCode), Temporary: true, Err: errors.New(snippet(body))}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return "", &SendError{Code: fmt.Sprintf("HTTP_%d", resp.StatusCode), Err: errors.New(snippet(body))}
	}
	return t.parseResponse(body)
}

func (t *HTTPTransport) parseResponse(body []byte) (string, error) {
	r := t.Profile.Response
	if r.Format == "text" {
		mm := t.idRe.FindSubmatch(body)
		if len(mm) < 2 || len(mm[1]) == 0 {
			return "", &SendError{Code: "HTTP_REFUSED", Err: errors.New(snippet(body))}
		}
		return string(mm[1]), nil
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", &SendError{Code: "HTTP_BAD_RESPONSE", Err: fmt.Errorf("not JSON: %s", snippet(body))}
	}
	if r.SuccessPath != "" {
		v := jsonPath(doc, r.SuccessPath)
		if !matchesAny(v, r.SuccessValues) {
			code := jsonPath(doc, r.ErrorPath)
			if code == "" {
				code = v
			}
			return "", &SendError{Code: "VENDOR_" + strings.ToUpper(code), Err: errors.New(snippet(body))}
		}
	}
	id := jsonPath(doc, r.IDPath)
	if id == "" {
		return "", &SendError{Code: "HTTP_NO_MESSAGE_ID", Err: errors.New(snippet(body))}
	}
	return id, nil
}

// jsonPath reads a dot path ("data.0.id") from decoded JSON as a string.
func jsonPath(doc any, path string) string {
	if path == "" {
		return ""
	}
	cur := doc
	for _, part := range strings.Split(path, ".") {
		switch v := cur.(type) {
		case map[string]any:
			cur = v[part]
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(v) {
				return ""
			}
			cur = v[i]
		default:
			return ""
		}
	}
	switch v := cur.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func matchesAny(v string, values []string) bool {
	for _, x := range values {
		if strings.EqualFold(v, x) {
			return true
		}
	}
	return false
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// DLRHandler serves the vendor's delivery callback at
// /dlr/<vendor>?token=<dlr_token>&... (GET query, form or JSON body). A
// receipt is acknowledged with 200 only after it is journaled to Kafka; any
// other answer makes the vendor retry.
func DLRHandler(p HTTPProfile, journal func(context.Context, domain.DLR) error, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if subtle.ConstantTimeCompare([]byte(token), []byte(p.DLRToken)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		fields, err := dlrFields(r)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		id, status := fields(p.DLR.IDParam), fields(p.DLR.StatusParam)
		if id == "" || status == "" {
			http.Error(w, "missing id or status", http.StatusBadRequest)
			return
		}
		if matchesAny(status, p.DLR.IntermediateValues) {
			w.WriteHeader(http.StatusOK) // not final; nothing to record
			return
		}
		stat := "UNDELIV"
		switch {
		case matchesAny(status, p.DLR.DeliveredValues):
			stat = "DELIVRD"
		case matchesAny(status, p.DLR.ExpiredValues):
			stat = "EXPIRED"
		}
		now := time.Now().UTC()
		d := domain.DLR{VendorMsgID: id, Stat: stat, ErrorCode: fields(p.DLR.ErrorParam), DoneAt: now, ReceivedAt: now}
		if err := journal(r.Context(), d); err != nil {
			log.Error("could not journal HTTP DLR; vendor should retry", "err", err)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})
}

// dlrFields returns a lookup over query parameters, a form body or a JSON
// body (dot paths allowed for JSON).
func dlrFields(r *http.Request) (func(string) string, error) {
	q := r.URL.Query()
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch {
	case r.Method == http.MethodPost && ct == "application/json":
		var doc any
		if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&doc); err != nil {
			return nil, err
		}
		return func(k string) string {
			if k == "" {
				return ""
			}
			if v := jsonPath(doc, k); v != "" {
				return v
			}
			return q.Get(k)
		}, nil
	case r.Method == http.MethodPost:
		r.Body = http.MaxBytesReader(nil, r.Body, 64*1024)
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		return func(k string) string {
			if k == "" {
				return ""
			}
			return r.Form.Get(k)
		}, nil
	}
	return func(k string) string {
		if k == "" {
			return ""
		}
		return q.Get(k)
	}, nil
}
