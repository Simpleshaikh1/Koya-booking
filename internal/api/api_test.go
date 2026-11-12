package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"koya-booking/internal/api"
	"koya-booking/internal/booking"
	"koya-booking/internal/store"
)

const key = "test-key"

// Every test runs against the in-memory store, and also against Postgres when
// TEST_DATABASE_URL is set
func stores(t *testing.T) map[string]func() booking.Store {
	m := map[string]func() booking.Store{
		"memory": func() booking.Store { return store.NewMemory() },
	}
	if url := os.Getenv("TEST_DATABASE_URL"); url != "" {
		m["postgres"] = func() booking.Store {
			pg, err := store.NewPostgres(context.Background(), url)
			if err != nil {
				t.Fatalf("postgres: %v", err)
			}
			t.Cleanup(pg.Close)
			return pg
		}
	}
	return m
}

type client struct {
	t   *testing.T
	srv *httptest.Server
}

func setup(t *testing.T, mk func() booking.Store) *client {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(api.NewServer(mk(), key, []byte("<html></html>"), log).Handler())
	t.Cleanup(srv.Close)
	c := &client{t: t, srv: srv}
	future := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	past := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	slots := []map[string]string{
		{"id": "s2", "provider": "Dr Bello", "start": future.Add(time.Hour).Format(time.RFC3339), "end": future.Add(90 * time.Minute).Format(time.RFC3339)},
		{"id": "s1", "provider": "Dr Ade", "start": future.Format(time.RFC3339), "end": future.Add(30 * time.Minute).Format(time.RFC3339)},
		{"id": "old", "provider": "Dr Ade", "start": past.Format(time.RFC3339), "end": past.Add(30 * time.Minute).Format(time.RFC3339)},
	}
	for i := 3; i <= 30; i++ {
		start := future.Add(time.Duration(i) * time.Hour)
		slots = append(slots, map[string]string{"id": "s" + strconv.Itoa(i), "provider": "Dr Chika",
			"start": start.Format(time.RFC3339), "end": start.Add(30 * time.Minute).Format(time.RFC3339)})
	}
	if code, _, _ := c.do("POST", "/reset", map[string]any{"slots": slots}, nil); code != 200 {
		t.Fatalf("reset: %d", code)
	}
	return c
}

func (c *client) do(method, path string, body any, headers map[string]string) (int, map[string]any, http.Header) {
	c.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.srv.URL+path, rdr)
	req.Header.Set("X-API-Key", key)
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out, res.Header
}

func newBooking(slot, name, email string) map[string]string {
	return map[string]string{"slot_id": slot, "name": name, "email": email}
}

func forEachStore(t *testing.T, fn func(t *testing.T, c *client)) {
	for name, mk := range stores(t) {
		t.Run(name, func(t *testing.T) { fn(t, setup(t, mk)) })
	}
}

func TestLevel1(t *testing.T) {
	forEachStore(t, func(t *testing.T, c *client) {
		code, body, _ := c.do("GET", "/slots", nil, nil)
		slots := body["slots"].([]any)
		if code != 200 || len(slots) != 31 {
			t.Fatalf("slots: %d %v", code, len(slots))
		}
		if first := slots[0].(map[string]any); first["id"] != "old" || first["available"] != true {
			t.Fatalf("want earliest first, got %v", first)
		}
		if second := slots[1].(map[string]any); second["id"] != "s1" {
			t.Fatalf("want s1 second, got %v", second)
		}

		_, body, _ = c.do("GET", "/slots?provider=Dr%20Ade", nil, nil)
		if n := len(body["slots"].([]any)); n != 2 {
			t.Fatalf("provider filter: %d", n)
		}

		code, b, _ := c.do("POST", "/bookings", newBooking("s1", "Ada Obi", "ada@example.com"), nil)
		if code != 201 || b["status"] != "confirmed" || b["id"] == "" || b["slot_id"] != "s1" {
			t.Fatalf("book: %d %v", code, b)
		}
		code, got, _ := c.do("GET", "/bookings/"+b["id"].(string), nil, nil)
		if code != 200 || got["id"] != b["id"] || got["created_at"] != b["created_at"] {
			t.Fatalf("get: %d %v", code, got)
		}
		if code, _, _ := c.do("GET", "/bookings/nope", nil, nil); code != 404 {
			t.Fatalf("missing booking: %d", code)
		}

		_, body, _ = c.do("GET", "/slots?provider=Dr%20Ade", nil, nil)
		for _, s := range body["slots"].([]any) {
			if m := s.(map[string]any); m["id"] == "s1" && m["available"] != false {
				t.Fatal("s1 should be unavailable")
			}
		}
	})
}

func TestLevel2(t *testing.T) {
	forEachStore(t, func(t *testing.T, c *client) {
		if code, b, _ := c.do("GET", "/slots", nil, map[string]string{"X-API-Key": ""}); code != 401 || b["error"] == nil {
			t.Fatalf("no key: %d %v", code, b)
		}
		if code, _, _ := c.do("GET", "/slots", nil, map[string]string{"X-API-Key": "wrong"}); code != 401 {
			t.Fatalf("wrong key: %d", code)
		}
		cases := []struct {
			body map[string]string
			want int
		}{
			{newBooking("", "A", "a@b.co"), 400},
			{newBooking("s1", "   ", "a@b.co"), 400},
			{newBooking("s1", "A", ""), 400},
			{newBooking("s1", "A", "not-an-email"), 400},
			{newBooking("s1", "A", "a@b"), 400},
			{map[string]string{"name": "A", "email": "a@b.co"}, 400},
			{newBooking("zzz", "A", "a@b.co"), 404},
			{newBooking("old", "A", "a@b.co"), 400},
		}
		for i, tc := range cases {
			if code, b, _ := c.do("POST", "/bookings", tc.body, nil); code != tc.want || b["error"] == nil {
				t.Errorf("case %d: got %d %v, want %d", i, code, b, tc.want)
			}
		}
		code, b, _ := c.do("POST", "/bookings", newBooking("s1", "  Ada Obi ", "  ADA@Example.COM "), nil)
		if code != 201 || b["name"] != "Ada Obi" || b["email"] != "ada@example.com" {
			t.Fatalf("trim/lowercase: %d %v", code, b)
		}
	})
}

func TestLevel3(t *testing.T) {
	forEachStore(t, func(t *testing.T, c *client) {
		// 50 people hit the same slot at the same instant: exactly one wins.
		const n = 50
		codes := make([]int, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				codes[i], _, _ = c.do("POST", "/bookings", newBooking("s1", "P", "p"+strconv.Itoa(i)+"@x.com"), nil)
			}()
		}
		close(start)
		wg.Wait()
		count := map[int]int{}
		for _, code := range codes {
			count[code]++
		}
		if count[201] != 1 || count[409] != n-1 {
			t.Fatalf("race: %v", count)
		}

		_, b, _ := c.do("POST", "/bookings", newBooking("s2", "Ada", "ada@x.com"), nil)
		id := b["id"].(string)
		code, b, _ := c.do("DELETE", "/bookings/"+id, nil, nil)
		if code != 200 || b["status"] != "cancelled" {
			t.Fatalf("cancel: %d %v", code, b)
		}
		if code, _, _ := c.do("DELETE", "/bookings/"+id, nil, nil); code != 409 {
			t.Fatalf("cancel twice: %d", code)
		}
		if code, _, _ := c.do("DELETE", "/bookings/nope", nil, nil); code != 404 {
			t.Fatalf("cancel missing: %d", code)
		}
		if code, _, _ := c.do("POST", "/bookings", newBooking("s2", "Bo", "bo@x.com"), nil); code != 201 {
			t.Fatalf("rebook freed slot: %d", code)
		}
	})
}

func TestLevel4Idempotency(t *testing.T) {
	forEachStore(t, func(t *testing.T, c *client) {
		h := map[string]string{"Idempotency-Key": "k1"}
		body := newBooking("s1", "Ada", "ada@x.com")
		code1, b1, _ := c.do("POST", "/bookings", body, h)
		code2, b2, _ := c.do("POST", "/bookings", body, h)
		if code1 != 201 || code2 != 201 || b1["id"] != b2["id"] {
			t.Fatalf("replay: %d %d %v %v", code1, code2, b1["id"], b2["id"])
		}
		if code, _, _ := c.do("POST", "/bookings", newBooking("s2", "Ada", "ada@x.com"), h); code != 422 {
			t.Fatalf("different body: %d", code)
		}

		// Concurrent retries with one key: one booking, same id everywhere.
		const n = 10
		ids := make([]any, n)
		codes := make([]int, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				var b map[string]any
				codes[i], b, _ = c.do("POST", "/bookings", newBooking("s3", "Bo", "bo@x.com"), map[string]string{"Idempotency-Key": "k2"})
				ids[i] = b["id"]
			}()
		}
		close(start)
		wg.Wait()
		for i := range n {
			if codes[i] != 201 || ids[i] != ids[0] {
				t.Fatalf("concurrent replay %d: %d %v vs %v", i, codes[i], ids[i], ids[0])
			}
		}
		_, list, _ := c.do("GET", "/bookings?email=bo@x.com", nil, nil)
		if n := len(list["bookings"].([]any)); n != 1 {
			t.Fatalf("want 1 booking, got %d", n)
		}
	})
}

func TestLevel4Pagination(t *testing.T) {
	forEachStore(t, func(t *testing.T, c *client) {
		var want []string
		for i := 3; i <= 14; i++ { // 12 bookings, but only 5 per email per minute...
			email := "pat@x.com"
			if i%2 == 0 {
				email = "PAT@x.com" // ...so mix case to prove matching ignores case
			}
			code, b, _ := c.do("POST", "/bookings", newBooking("s"+strconv.Itoa(i), "Pat", email), nil)
			if code == 429 {
				break
			}
			want = append(want, b["id"].(string))
		}
		if len(want) != 5 {
			t.Fatalf("expected 5 bookings before rate limit, got %d", len(want))
		}
		other, _, _ := c.do("POST", "/bookings", newBooking("s20", "Other", "other@x.com"), nil)
		if other != 201 {
			t.Fatalf("other email: %d", other)
		}

		var got []string
		cursor := ""
		pages := 0
		for {
			path := "/bookings?email=Pat@X.com&limit=2"
			if cursor != "" {
				path += "&cursor=" + cursor
			}
			code, page, _ := c.do("GET", path, nil, nil)
			if code != 200 {
				t.Fatalf("page: %d %v", code, page)
			}
			pages++
			for _, b := range page["bookings"].([]any) {
				got = append(got, b.(map[string]any)["id"].(string))
			}
			if page["next_cursor"] == nil {
				break
			}
			cursor = page["next_cursor"].(string)
		}
		if pages != 3 || len(got) != 5 {
			t.Fatalf("pages=%d got=%d", pages, len(got))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("order: %v vs %v", got, want)
			}
		}
		for _, bad := range []string{"0", "51", "-1", "abc", ""} {
			if code, _, _ := c.do("GET", "/bookings?email=pat@x.com&limit="+bad, nil, nil); code != 400 {
				t.Errorf("limit=%q: %d", bad, code)
			}
		}
		if code, _, _ := c.do("GET", "/bookings?email=pat@x.com&cursor=garbage", nil, nil); code != 400 {
			t.Errorf("bad cursor: %d", code)
		}
	})
}

func TestLevel5RateLimit(t *testing.T) {
	forEachStore(t, func(t *testing.T, c *client) {
		// Failed attempts count: slot "zzz" doesn't exist -> 404 each time.
		for i := range 5 {
			if code, _, _ := c.do("POST", "/bookings", newBooking("zzz", "Spam", "spam@x.com"), nil); code != 404 {
				t.Fatalf("attempt %d: %d", i+1, code)
			}
		}
		code, b, h := c.do("POST", "/bookings", newBooking("s1", "Spam", "SPAM@x.com"), nil)
		if code != 429 || b["error"] == nil {
			t.Fatalf("6th: %d %v", code, b)
		}
		ra, err := strconv.Atoi(h.Get("Retry-After"))
		if err != nil || ra < 1 || ra > 60 {
			t.Fatalf("Retry-After=%q", h.Get("Retry-After"))
		}
		if code, _, _ := c.do("POST", "/bookings", newBooking("s1", "Ok", "ok@x.com"), nil); code != 201 {
			t.Fatalf("other email blocked: %d", code)
		}
	})
}
