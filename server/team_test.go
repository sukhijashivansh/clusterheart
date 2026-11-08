package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestWorkspaceName(t *testing.T) {
	cases := map[[2]string]string{
		{"Shivansh", "s@x.com"}:  "Shivansh's workspace",
		{"James", "j@x.com"}:     "James' workspace",
		{"", "ops@acme.io"}:      "ops' workspace",
		{"", "dev@acme.io"}:      "dev's workspace",
		{"  Faizan ", "f@x.com"}: "Faizan's workspace",
	}
	for in, want := range cases {
		if got := workspaceName(in[0], in[1]); got != want {
			t.Errorf("workspaceName(%q,%q) = %q, want %q", in[0], in[1], got, want)
		}
	}
	for role, want := range map[string]bool{roleOwner: true, roleAdmin: true, roleMember: false, "": false} {
		if got := (workspace{Role: role}).canManage(); got != want {
			t.Errorf("canManage(%q) = %v", role, got)
		}
	}
}

// TestTeamFlow runs the whole team lifecycle against a real MongoDB. It runs
// only when MONGO_TEST_URI is set, e.g.
//
//	MONGO_TEST_URI=mongodb://127.0.0.1:27017 go test -run TeamFlow .
func TestTeamFlow(t *testing.T) {
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("MONGO_TEST_URI not set")
	}
	t.Setenv("MONGO_URI", uri)
	t.Setenv("MONGO_DB", fmt.Sprintf("chtest_team_%d", time.Now().UnixNano()))
	t.Setenv("SMTP_USER", "") // never send real mail from tests
	ctx := context.Background()
	st, err := openStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.users.Database().Drop(ctx); _ = st.client.Disconnect(ctx) }()
	a := &app{store: st, secret: []byte("test-secret-test-secret-test-secret!"), adminEmails: map[string]bool{}}

	owner := &User{Email: "owner@acme.io", Name: "Shivansh", Approved: true, Created: time.Now()}
	pending := &User{Email: "old@acme.io", Name: "Old", Approved: false, Created: time.Now()}
	for _, u := range []*User{owner, pending} {
		if err := st.insert(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	call := func(as, method, path string, body any) (int, map[string]any) {
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		r := httptest.NewRequest(method, path, &buf)
		if as != "" {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: a.mint(as)})
		}
		w := httptest.NewRecorder()
		switch {
		case path == "/api/signup":
			a.handleSignup(w, r)
		case path == "/api/workspace":
			a.handleWorkspace(w, r)
		default:
			a.handleTeam(w, r)
		}
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	ws := func(email string) workspace {
		u, err := st.find(ctx, email)
		if err != nil || u == nil {
			t.Fatalf("find %s: %v", email, err)
		}
		return a.workspaceFor(ctx, u)
	}

	// Validation.
	if code, _ := call(owner.Email, "POST", "/api/team", map[string]string{"email": "nope"}); code != 400 {
		t.Fatalf("bad email: %d", code)
	}
	if code, _ := call(owner.Email, "POST", "/api/team", map[string]string{"email": owner.Email}); code != 400 {
		t.Fatalf("adding yourself: %d", code)
	}

	// Owner adds a new person; they sign up and land in the owner's workspace,
	// approved, as a member who cannot manage.
	if code, out := call(owner.Email, "POST", "/api/team", map[string]string{"email": "New@Acme.io", "role": "member"}); code != 200 || out["joined"] != false {
		t.Fatalf("add new: %d %v", code, out)
	}
	if code, _ := call(owner.Email, "POST", "/api/team", map[string]string{"email": "new@acme.io"}); code != 409 {
		t.Fatalf("duplicate add: %d", code)
	}
	if code, out := call("", "POST", "/api/signup", map[string]string{"name": "Newbie", "email": "new@acme.io", "password": "longenough"}); code != 200 || out["approved"] != true {
		t.Fatalf("signup: %d %v", code, out)
	}
	w := ws("new@acme.io")
	if w.Tenant != tenantFor(owner.Email) || w.Role != roleMember || w.canManage() || w.Name != "Shivansh's workspace" {
		t.Fatalf("member workspace: %+v", w)
	}

	// The role change to admin grants manage rights.
	if code, _ := call(owner.Email, "PATCH", "/api/team", map[string]string{"email": "new@acme.io", "role": "admin"}); code != 200 {
		t.Fatalf("role: %d", code)
	}
	if w := ws("new@acme.io"); !w.canManage() || w.Tenant != tenantFor(owner.Email) {
		t.Fatalf("admin workspace: %+v", w)
	}

	// An existing, still-pending account is approved and switched on add.
	if code, out := call(owner.Email, "POST", "/api/team", map[string]string{"email": "old@acme.io"}); code != 200 || out["joined"] != true {
		t.Fatalf("add existing: %d %v", code, out)
	}
	if u, _ := st.find(ctx, "old@acme.io"); !u.Approved || ws("old@acme.io").Tenant != tenantFor(owner.Email) {
		t.Fatalf("existing not joined: %+v", u)
	}

	// The owner's view lists both people; the member sees two workspaces.
	if _, out := call(owner.Email, "GET", "/api/team", nil); len(out["team"].([]any)) != 2 {
		t.Fatalf("team list: %v", out)
	}
	if _, out := call("new@acme.io", "GET", "/api/team", nil); len(out["workspaces"].([]any)) != 2 {
		t.Fatalf("workspaces: %v", out)
	}

	// Switching to your own workspace and back.
	if code, _ := call("new@acme.io", "POST", "/api/workspace", map[string]string{"owner": ""}); code != 200 || ws("new@acme.io").Tenant != tenantFor("new@acme.io") {
		t.Fatal("switch to own failed")
	}
	if code, _ := call("new@acme.io", "POST", "/api/workspace", map[string]string{"owner": "stranger@x.io"}); code != 404 {
		t.Fatalf("switch to a team you're not on: %d", code)
	}
	if code, _ := call("new@acme.io", "POST", "/api/workspace", map[string]string{"owner": owner.Email}); code != 200 || ws("new@acme.io").Tenant != tenantFor(owner.Email) {
		t.Fatal("switch back failed")
	}

	// Removing someone ends their access on the next request.
	if code, _ := call(owner.Email, "DELETE", "/api/team?email=new@acme.io", nil); code != 200 {
		t.Fatalf("remove: %d", code)
	}
	if w := ws("new@acme.io"); w.Tenant != tenantFor("new@acme.io") || w.Role != roleOwner {
		t.Fatalf("removed member still in: %+v", w)
	}

	// Leaving a team.
	if code, _ := call("old@acme.io", "POST", "/api/workspace", map[string]any{"owner": owner.Email, "leave": true}); code != 200 {
		t.Fatalf("leave: %d", code)
	}
	if m, _ := st.membership(ctx, owner.Email, "old@acme.io"); m != nil || ws("old@acme.io").Tenant != tenantFor("old@acme.io") {
		t.Fatal("leave did not take")
	}

	// A team member of a pending (unapproved) owner is not auto-approved.
	bad := &User{Email: "pend-owner@x.io", Name: "P", Approved: false, Created: time.Now()}
	_ = st.insert(ctx, bad)
	_ = st.addMember(ctx, teamMember{Owner: bad.Email, Email: "friend@x.io", Role: roleMember, Added: time.Now()})
	if _, out := call("", "POST", "/api/signup", map[string]string{"name": "F", "email": "friend@x.io", "password": "longenough"}); out["approved"] != false {
		t.Fatalf("friend of a pending owner was approved: %v", out)
	}
}
