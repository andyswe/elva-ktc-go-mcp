package ktc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseStartPage(t *testing.T) {
	p, err := parseStartPage(latin1ToUTF8(fixture(t, "start.html")))
	if err != nil {
		t.Fatal(err)
	}
	if p.Model != "RCU1111-1100" || p.Version != "4.7.13" || p.Name != "1-1001" {
		t.Errorf("product info = %+v", p)
	}
	want := []MeterRef{{"VV-01", "187588 l", 1}, {"EL-01", "26795900 Wh", 3}}
	if len(p.Meters) != len(want) {
		t.Fatalf("meters = %+v", p.Meters)
	}
	for i := range want {
		if p.Meters[i] != want[i] {
			t.Errorf("meter %d = %+v, want %+v", i, p.Meters[i], want[i])
		}
	}
}

func TestIsLoginPage(t *testing.T) {
	if !isLoginPage(fixture(t, "login.html")) {
		t.Error("login.html not detected as login page")
	}
	if isLoginPage(fixture(t, "start.html")) {
		t.Error("start.html detected as login page")
	}
}

func TestParseMeterXML(t *testing.T) {
	el, err := parseMeterXML(fixture(t, "mb3_el.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if el.Pos != 3 || el.Name != "EL-01" || el.MeterID != "87654321" || el.CommStatus != "OK" {
		t.Errorf("el = %+v", el)
	}
	if v, ok := el.Get("energy"); !ok || v.Unit != "Wh" || v.Value != 26795900 {
		t.Errorf("el energy = %+v %v", v, ok)
	}
	if v, ok := el.Get("power"); !ok || v.Unit != "W" || v.Value != 584 {
		t.Errorf("el power = %+v %v", v, ok)
	}
	if el.CurrentDay == nil || *el.CurrentDay != 2200 {
		t.Errorf("el current day = %v", el.CurrentDay)
	}
	if el.MeterTime.IsZero() {
		t.Error("el meter time not parsed")
	}

	w, err := parseMeterXML(fixture(t, "mb1_water.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := w.Get("volume"); !ok || v.Unit != "l" || v.Value != 187588 {
		t.Errorf("water volume = %+v %v", v, ok)
	}
	if v, ok := w.Get("flow"); !ok || v.Unit != "l/h" || v.Value != 3.0 {
		t.Errorf("water flow = %+v %v", v, ok)
	}

	empty, err := parseMeterXML(fixture(t, "mb5_empty.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if empty.Pos != 5 || len(empty.Values) != 0 || empty.CurrentHour != nil {
		t.Errorf("empty = %+v", empty)
	}
}

// fakeRCU emulates the RCU as observed on firmware 4.7.13: one session, the login
// page on a bad password, and a shared M-Bus cursor over MB01..MB08 where
// counter=0 resets to MB01, counter=1/2 step back and forward with wrap-around,
// and an out-of-range pos gives MB01.
type fakeRCU struct {
	*httptest.Server
	cursor int
}

const fakeSlots = 8

func newFakeRCU(t *testing.T) *fakeRCU {
	start, login := fixture(t, "start.html"), fixture(t, "login.html")
	meters := map[int][]byte{1: fixture(t, "mb1_water.xml"), 3: fixture(t, "mb3_el.xml")}
	empty := fixture(t, "mb5_empty.xml")
	f := &fakeRCU{cursor: 5}
	loggedIn := false
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.URL.Path {
		case "/index.htm":
			loggedIn = r.Form.Get("passwd") == "secret"
			if loggedIn {
				w.Write(start)
			} else {
				w.Write(login)
			}
		case "/mb.xml":
			if !loggedIn {
				w.Write(login)
				return
			}
			switch {
			case r.Form.Has("pos"):
				f.cursor, _ = strconv.Atoi(r.Form.Get("pos"))
				if f.cursor < 1 || f.cursor > fakeSlots {
					f.cursor = 1
				}
			case r.Form.Get("counter") == "1":
				f.cursor = (f.cursor+fakeSlots-2)%fakeSlots + 1
			case r.Form.Get("counter") == "2":
				f.cursor = f.cursor%fakeSlots + 1
			default:
				t.Errorf("unexpected mb.xml request %v (counter=0 resets the shared cursor)", r.Form)
				f.cursor = 1
			}
			if b, ok := meters[f.cursor]; ok {
				w.Write(b)
			} else {
				w.Write(bytes.Replace(empty, []byte("MB05"), []byte(fmt.Sprintf("MB%02d", f.cursor)), 1))
			}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	return f
}

func TestClient(t *testing.T) {
	srv := newFakeRCU(t)
	defer srv.Close()
	ctx := context.Background()

	if _, err := New(srv.URL, "wrong", time.Second).Login(ctx); !errors.Is(err, ErrAuth) {
		t.Errorf("wrong password: err = %v, want ErrAuth", err)
	}
	if _, err := New(srv.URL, "wrong", time.Second).ReadMeter(ctx, 3); !errors.Is(err, ErrAuth) {
		t.Errorf("read without login: err = %v, want ErrAuth", err)
	}

	c := New(srv.URL, "secret", time.Second)
	p, err := c.Login(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Meters) != 2 {
		t.Fatalf("meters = %+v", p.Meters)
	}
	m, err := c.ReadMeter(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "EL-01" || srv.cursor != 3 {
		t.Errorf("name = %q, cursor = %d", m.Name, srv.cursor)
	}
	if _, err := c.ReadMeter(ctx, 42); err == nil {
		t.Error("reading out-of-range MB42 succeeded")
	}
}

func TestCursorSaveRestore(t *testing.T) {
	srv := newFakeRCU(t)
	defer srv.Close()
	ctx := context.Background()
	c := New(srv.URL, "secret", time.Second)
	if _, err := c.Login(ctx); err != nil {
		t.Fatal(err)
	}
	for _, start := range []int{1, 2, 5, fakeSlots} { // include both wrap-around edges
		srv.cursor = start
		got, err := c.Cursor(ctx)
		if err != nil || got != start || srv.cursor != start {
			t.Errorf("start %d: Cursor() = %d, %v; cursor now %d", start, got, err, srv.cursor)
		}
		c.ReadMeter(ctx, 3)
		if err := c.SetCursor(ctx, got); err != nil || srv.cursor != start {
			t.Errorf("start %d: SetCursor: %v; cursor now %d", start, err, srv.cursor)
		}
	}
}

func TestClientUnreachable(t *testing.T) {
	srv := newFakeRCU(t)
	url := srv.URL
	srv.Close()
	if _, err := New(url, "secret", time.Second).Login(context.Background()); !errors.Is(err, ErrUnreachable) {
		t.Errorf("err = %v, want ErrUnreachable", err)
	}
}
