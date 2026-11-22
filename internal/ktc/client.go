// Package ktc talks to a KTC RCU1111 "lägenhetsnod" over its built-in web UI.
//
// The RCU has no API. Its web UI works like this:
//
//   - POST /index.htm with part=3&passwd=<pw> logs in. The session is bound to the
//     client (no cookie). On success the start page is returned (product info and a
//     table of meters); on failure the login form is returned again, still HTTP 200.
//   - POST /mb.xml returns the M-Bus object at the server-side cursor as XML. The
//     cursor is moved with counter=0 (reset to MB01), counter=1 (prev), counter=2 (next,
//     both wrapping over MB01..MB08) or pos=N (jump to MBN; out of range gives MB01).
//     We read with pos=N so the shared cursor cannot confuse us.
//
// The RCU is shared with other users (the electricity company, people using the web
// UI). This client therefore only ever uses the two read endpoints above, never the
// .htm forms that save settings, and never sends counter=0. It offers Cursor and
// SetCursor so callers can put the shared cursor back where they found it.
package ktc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrAuth is returned when the RCU rejects the password.
var ErrAuth = errors.New("authentication failed: KTC rejected the password")

// ErrUnreachable wraps network-level failures talking to the RCU.
var ErrUnreachable = errors.New("KTC unreachable")

// Client is a client for one RCU.
type Client struct {
	baseURL  string
	password string
	loc      *time.Location
	http     *http.Client
}

// New returns a client for the RCU at host (host or host:port, optionally with scheme).
// loc is the time zone the RCU's clock runs in.
func New(host, password string, timeout time.Duration, loc *time.Location) *Client {
	base := host
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	return &Client{
		baseURL:  strings.TrimRight(base, "/"),
		password: password,
		loc:      loc,
		http:     &http.Client{Timeout: timeout},
	}
}

// BaseURL returns the URL of the RCU.
func (c *Client) BaseURL() string { return c.baseURL }

// Login authenticates and returns the parsed start page.
func (c *Client) Login(ctx context.Context) (*StartPage, error) {
	body, err := c.post(ctx, "/index.htm", url.Values{"part": {"3"}, "passwd": {c.password}})
	if err != nil {
		return nil, err
	}
	if isLoginPage(body) {
		return nil, ErrAuth
	}
	return parseStartPage(body)
}

// Cursor returns the position of the RCU's shared M-Bus cursor, leaving it where it was.
//
// The RCU has no read that leaves the cursor alone (counter=0 resets it to MB01, which
// is what a browser does on page load), so step next then prev. The two steps cancel
// out, including wrap-around at MB01 and MB08, and the second response is the object
// at the original position.
func (c *Client) Cursor(ctx context.Context) (int, error) {
	if _, err := c.readMB(ctx, url.Values{"counter": {"2"}}, "cursor"); err != nil {
		return 0, err
	}
	r, err := c.readMB(ctx, url.Values{"counter": {"1"}}, "cursor")
	if err != nil {
		return 0, err
	}
	return r.Pos, nil
}

// SetCursor moves the RCU's shared M-Bus cursor to MB<pos>.
func (c *Client) SetCursor(ctx context.Context, pos int) error {
	_, err := c.ReadMeter(ctx, pos)
	return err
}

// ReadMeter reads M-Bus object MB<pos>. This moves the shared cursor to pos.
// Login must have been called recently.
func (c *Client) ReadMeter(ctx context.Context, pos int) (*MeterReading, error) {
	r, err := c.readMB(ctx, url.Values{"pos": {strconv.Itoa(pos)}}, fmt.Sprintf("MB%d", pos))
	if err != nil {
		return nil, err
	}
	if r.Pos != pos {
		return nil, fmt.Errorf("reading MB%d: RCU returned MB%02d instead", pos, r.Pos)
	}
	return r, nil
}

func (c *Client) readMB(ctx context.Context, form url.Values, what string) (*MeterReading, error) {
	body, err := c.post(ctx, "/mb.xml", form)
	if err != nil {
		return nil, err
	}
	if isLoginPage(body) {
		return nil, fmt.Errorf("reading %s: session not authenticated: %w", what, ErrAuth)
	}
	r, err := parseMeterXML(body, c.loc)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", what, err)
	}
	return r, nil
}

func (c *Client) post(ctx context.Context, path string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: POST %s%s: %v", ErrUnreachable, c.baseURL, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: reading response from %s%s: %v", ErrUnreachable, c.baseURL, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("POST %s%s: unexpected HTTP status %s", c.baseURL, path, resp.Status)
	}
	return latin1ToUTF8(body), nil
}

func isLoginPage(body []byte) bool {
	return bytes.Contains(body, []byte(`name="passwd"`))
}

// latin1ToUTF8 converts ISO-8859-1 (which the RCU always uses) to UTF-8.
func latin1ToUTF8(b []byte) []byte {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		sb.WriteRune(rune(c))
	}
	return []byte(sb.String())
}
