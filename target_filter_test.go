package favorites_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	f "github.com/StuHelper/stuhelper-favorites"
	"github.com/StuHelper/stuhelper-favorites/httpapi"
)

func TestFavoritesTargetFilterOwnershipValidationAndPagination(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	for _, seed := range []struct{ owner, kind, target string }{
		{"alice", "course", "a"}, {"alice", "course", "b"}, {"alice", "course", "excluded"},
		{"alice", "teacher", "a"}, {"bob", "course", "a"}, {"bob", "course", "private"},
	} {
		if _, err := x.s.Add(ctx, seed.owner, seed.kind+seed.target, f.Add{Kind: seed.kind, TargetID: seed.target}); err != nil {
			t.Fatal(err)
		}
	}
	x.source.state.Store(1)
	calls := x.source.calls.Load()
	h := httpapi.Handler{Service: x.s, Authenticate: func(r *http.Request) (string, error) { return r.Header.Get("Test-Session"), nil }}
	request := func(owner, query string, status int) f.Items {
		t.Helper()
		r := httptest.NewRequest("GET", "/api/v1/favorites/items?"+query, nil)
		r.Header.Set("Test-Session", owner)
		r.Header.Set("X-Owner", "bob")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s: status %d: %s", query, w.Code, w.Body.String())
		}
		var result f.Items
		if status == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	result := request("alice", "kind=course&target_ids=a,b,a,private,missing&limit=100&owner=bob", 200)
	if len(result.Items) != 2 || result.Next != "" {
		t.Fatal(result)
	}
	for _, item := range result.Items {
		if item.Kind != "course" || (item.TargetID != "a" && item.TargetID != "b") {
			t.Fatal(item)
		}
	}
	if x.source.calls.Load() != calls {
		t.Fatal("own receipt read queried target admission")
	}
	if got := request("alice", "kind=course&target_ids=private,missing", 200); len(got.Items) != 0 {
		t.Fatal(got)
	}
	if got := request("alice", "kind=teacher&target_ids=a,b", 200); len(got.Items) != 1 || got.Items[0].Kind != "teacher" {
		t.Fatal(got)
	}
	if got := request("alice", "kind=course&target_ids=a,b&q=Title%20b", 200); len(got.Items) != 1 || got.Items[0].TargetID != "b" {
		t.Fatal(got)
	}
	for _, sort := range []string{"newest", "name"} {
		base := "kind=course&target_ids=b,a&limit=1&sort=" + sort
		first := request("alice", base, 200)
		if len(first.Items) != 1 || first.Next == "" {
			t.Fatal(first)
		}
		second := request("alice", "kind=course&target_ids=a,b,a&limit=1&sort="+sort+"&after="+url.QueryEscape(first.Next), 200)
		if len(second.Items) != 1 || second.Next != "" || first.Items[0].ID == second.Items[0].ID {
			t.Fatal(second)
		}
		request("alice", "kind=course&target_ids=a,excluded&limit=1&sort="+sort+"&after="+url.QueryEscape(first.Next), 400)
		request("bob", base+"&after="+url.QueryEscape(first.Next), 400)
	}
	for _, query := range []string{
		"target_ids=a", "kind=unknown&target_ids=a", "kind=course&target_ids=", "kind=course&target_ids=a,",
		"kind=course&target_ids=,a", "kind=course&target_ids=a,,b", "kind=course&target_ids=%20a",
		"kind=course&target_ids=a%00", "kind=course&target_ids=a&target_ids=b", "kind=course&target_ids=" + strings.Repeat("x", 201),
		"kind=course&target_ids=" + strings.Repeat("a,", 100) + "b",
	} {
		request("alice", query, 400)
	}
	ids := []string{"a"}
	for i := 1; i < 100; i++ {
		ids = append(ids, fmt.Sprintf("absent-%d", i))
	}
	if got := request("alice", "kind=course&target_ids="+strings.Join(ids, ","), 200); len(got.Items) != 1 || got.Items[0].TargetID != "a" {
		t.Fatal(got)
	}
	request("", "kind=course&target_ids=a", 401)
	var count int
	if err := x.db.QueryRow(`SELECT count(*) FROM favorites.items`).Scan(&count); err != nil || count != 6 {
		t.Fatal(count, err)
	}
}
