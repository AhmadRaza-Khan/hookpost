package hookpost_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahmadraza-khan/hookpost"
	"github.com/ahmadraza-khan/hookpost/signature"
)

func createEndpoint(t *testing.T, c *hookpost.Client, in hookpost.EndpointInput) hookpost.Endpoint {
	t.Helper()
	if in.OwnerID == "" {
		in.OwnerID = testOwner
	}
	if in.URL == "" {
		in.URL = "https://example.com/hooks"
	}
	ep, err := c.CreateEndpoint(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

func TestCreateEndpoint(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{})
	ep := createEndpoint(t, c, hookpost.EndpointInput{
		Description: "orders", EventTypes: []string{"order.created"},
	})
	if !strings.HasPrefix(ep.ID, "ep_") || !strings.HasPrefix(ep.Secret, "whsec_") ||
		ep.OwnerID != testOwner || ep.Description != "orders" || ep.Disabled ||
		len(ep.EventTypes) != 1 || ep.CreatedAt.IsZero() {
		t.Fatalf("endpoint %+v", ep)
	}
	all := createEndpoint(t, c, hookpost.EndpointInput{})
	if all.EventTypes == nil || len(all.EventTypes) != 0 {
		t.Fatalf("event types %#v, want empty non-nil", all.EventTypes)
	}
}

func TestCreateEndpointValidation(t *testing.T) {
	strict, err := hookpost.New(needDB(t), hookpost.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, u := range []string{
		"", "ftp://example.com/x", "/relative/path", "example.com/hooks",
		"http://example.com/hooks", "https://user:pass@example.com/", "https://:443/",
	} {
		_, err := strict.CreateEndpoint(ctx, hookpost.EndpointInput{OwnerID: "o", URL: u})
		if !errors.Is(err, hookpost.ErrInvalidURL) {
			t.Errorf("%q: got %v, want ErrInvalidURL", u, err)
		}
	}
	if _, err := strict.CreateEndpoint(ctx, hookpost.EndpointInput{URL: "https://example.com"}); !errors.Is(err, hookpost.ErrNoOwner) {
		t.Errorf("no owner: got %v", err)
	}

	lax := newDispatchClient(t, hookpost.Config{}) // AllowPrivateNetworks
	createEndpoint(t, lax, hookpost.EndpointInput{URL: "http://localhost:9000/hooks"})
}

func TestGetAndListHideSecret(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{})
	ctx := context.Background()
	a := createEndpoint(t, c, hookpost.EndpointInput{Description: "a"})
	b := createEndpoint(t, c, hookpost.EndpointInput{Description: "b"})

	got, err := c.GetEndpoint(ctx, testOwner, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Secret != "" || got.ID != a.ID || got.Description != "a" {
		t.Fatalf("get %+v", got)
	}
	list, err := c.ListEndpoints(ctx, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != a.ID || list[1].ID != b.ID {
		t.Fatalf("list %+v", list)
	}
	for _, ep := range list {
		if ep.Secret != "" {
			t.Fatal("list exposed a secret")
		}
	}
	if empty, err := c.ListEndpoints(ctx, "nobody"); err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty list %#v %v", empty, err)
	}
}

func TestUpdateEndpoint(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{})
	ctx := context.Background()
	ep := createEndpoint(t, c, hookpost.EndpointInput{Description: "old", EventTypes: []string{"a"}})

	desc := "new"
	got, err := c.UpdateEndpoint(ctx, testOwner, ep.ID, hookpost.EndpointUpdate{Description: &desc})
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "new" || got.URL != ep.URL || len(got.EventTypes) != 1 || !got.UpdatedAt.After(ep.UpdatedAt) {
		t.Fatalf("partial update %+v", got)
	}

	u, types := "https://example.org/x", []string{}
	got, err = c.UpdateEndpoint(ctx, testOwner, ep.ID, hookpost.EndpointUpdate{URL: &u, EventTypes: &types})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != u || len(got.EventTypes) != 0 || got.Description != "new" {
		t.Fatalf("update %+v", got)
	}

	bad := "ftp://x"
	if _, err := c.UpdateEndpoint(ctx, testOwner, ep.ID, hookpost.EndpointUpdate{URL: &bad}); !errors.Is(err, hookpost.ErrInvalidURL) {
		t.Fatalf("bad url: %v", err)
	}
}

func TestDisableEnableDelete(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{})
	ctx := context.Background()
	ep := createEndpoint(t, c, hookpost.EndpointInput{})

	if err := c.SetEndpointDisabled(ctx, testOwner, ep.ID, true); err != nil {
		t.Fatal(err)
	}
	got, _ := c.GetEndpoint(ctx, testOwner, ep.ID)
	if !got.Disabled || got.DisabledReason == "" {
		t.Fatalf("disabled %+v", got)
	}
	if err := c.SetEndpointDisabled(ctx, testOwner, ep.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ = c.GetEndpoint(ctx, testOwner, ep.ID)
	if got.Disabled || got.DisabledReason != "" {
		t.Fatalf("enabled %+v", got)
	}

	if err := c.DeleteEndpoint(ctx, testOwner, ep.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetEndpoint(ctx, testOwner, ep.ID); !errors.Is(err, hookpost.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := c.DeleteEndpoint(ctx, testOwner, ep.ID); !errors.Is(err, hookpost.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

// Every operation is owner-scoped: another owner sees ErrNotFound.
func TestEndpointOwnerScoping(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{})
	ctx := context.Background()
	ep := createEndpoint(t, c, hookpost.EndpointInput{})
	const other = "shop_2"
	desc := "pwned"

	_, err1 := c.GetEndpoint(ctx, other, ep.ID)
	_, err2 := c.UpdateEndpoint(ctx, other, ep.ID, hookpost.EndpointUpdate{Description: &desc})
	err3 := c.SetEndpointDisabled(ctx, other, ep.ID, true)
	_, err4 := c.RotateSecret(ctx, other, ep.ID, time.Hour)
	err5 := c.DeleteEndpoint(ctx, other, ep.ID)
	for i, err := range []error{err1, err2, err3, err4, err5} {
		if !errors.Is(err, hookpost.ErrNotFound) {
			t.Errorf("op %d: got %v, want ErrNotFound", i+1, err)
		}
	}
	if list, _ := c.ListEndpoints(ctx, other); len(list) != 0 {
		t.Errorf("other owner lists %d endpoints", len(list))
	}
	got, err := c.GetEndpoint(ctx, testOwner, ep.ID)
	if err != nil || got.Disabled || got.Description != "" {
		t.Fatalf("endpoint changed by other owner: %+v %v", got, err)
	}
}

// signedBy records requests and reports which secrets verify them.
type signedBy struct {
	mu   sync.Mutex
	reqs []struct {
		h    http.Header
		body []byte
	}
}

func (s *signedBy) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.reqs = append(s.reqs, struct {
		h    http.Header
		body []byte
	}{r.Header.Clone(), body})
	s.mu.Unlock()
}

func (s *signedBy) verifies(t *testing.T, n int, secret string) bool {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	return signature.Verify(secret, s.reqs[n].h, s.reqs[n].body) == nil
}

func (s *signedBy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

func TestRotateSecret(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{})
	ctx := context.Background()
	rec := &signedBy{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	ep := createEndpoint(t, c, hookpost.EndpointInput{URL: srv.URL})
	run(t, c)

	// During grace: both secrets verify.
	newSecret, err := c.RotateSecret(ctx, testOwner, ep.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if newSecret == ep.Secret || !strings.HasPrefix(newSecret, "whsec_") {
		t.Fatalf("new secret %q", newSecret)
	}
	send(t, c, 1)
	waitFor(t, "first request", func() bool { return rec.count() == 1 })
	if !rec.verifies(t, 0, ep.Secret) || !rec.verifies(t, 0, newSecret) {
		t.Fatal("during grace both old and new secret must verify")
	}

	// Grace over: only the newest secret verifies.
	newest, err := c.RotateSecret(ctx, testOwner, ep.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, 2)
	waitFor(t, "second request", func() bool { return rec.count() == 2 })
	if rec.verifies(t, 1, newSecret) || !rec.verifies(t, 1, newest) {
		t.Fatal("after grace only the new secret may verify")
	}
}

func TestSSRFGuardBlocksLoopback(t *testing.T) {
	newDispatchClient(t, hookpost.Config{})                // reset tables
	strict, err := hookpost.New(testPool, hookpost.Config{ // default: guard on
		PollInterval: 20 * time.Millisecond, Schedule: []time.Duration{}, Retention: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	rc := &receiver{}
	serve(t, rc) // http://127.0.0.1:... inserted directly, bypassing URL validation
	run(t, strict)

	msgID := send(t, strict, 1)
	d := waitStatus(t, msgID, "dead")
	if rc.count() != 0 {
		t.Fatal("request reached a loopback address")
	}
	if d.lastError == nil || !strings.Contains(*d.lastError, hookpost.ErrBlockedAddress.Error()) {
		t.Fatalf("last error %v, want ErrBlockedAddress", d.lastError)
	}
}
