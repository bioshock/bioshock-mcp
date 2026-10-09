package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchJSONCaches(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`{"n":1}`))
	}))
	defer srv.Close()

	var out struct{ N int }
	for range 3 {
		if err := getJSON(srv.URL, nil, &out); err != nil || out.N != 1 {
			t.Fatal(err, out)
		}
	}
	if err := fetchJSON("POST", srv.URL, []byte(`{"q":1}`), nil, &out); err != nil {
		t.Fatal(err)
	}
	if hits != 2 { // one GET (then cached) + one POST with a different body
		t.Fatalf("want 2 upstream hits, got %d", hits)
	}

	// expire the entries: next call goes upstream again
	cache.Lock()
	for k, c := range cache.m {
		c.at = time.Now().Add(-cacheTTL)
		cache.m[k] = c
	}
	cache.Unlock()
	getJSON(srv.URL, nil, &out)
	if hits != 3 {
		t.Fatalf("want refetch after TTL, got %d hits", hits)
	}
}
