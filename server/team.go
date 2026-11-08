package main

// Teams: a main account (the owner) adds people by email in Account settings.
// Each person on the team works inside the owner's workspace and sees the
// owner's clusters and servers. A workspace is just an owner's tenant, so the
// control plane does not change: we only pick which tenant a request reads.
//
// Roles: "member" can view everything and use AI, latency checks and
// rightsizing advice; "admin" can also add, rename and delete clusters and
// servers and act on incidents and fixes. Only the owner manages the team.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	roleOwner  = "owner"
	roleAdmin  = "admin"
	roleMember = "member"
	maxTeam    = 50
)

// teamMember is one person on an owner's team. The person may not have
// signed up yet; they join the moment they do.
type teamMember struct {
	Owner string    `bson:"owner" json:"-"`
	Email string    `bson:"email" json:"email"`
	Role  string    `bson:"role" json:"role"`
	Added time.Time `bson:"added" json:"added"`
}

// workspace is the tenant a request works in, and the caller's role there.
type workspace struct {
	Owner  string // owner's email
	Name   string // "Shivansh's workspace"
	Role   string
	Tenant string
}

func (w workspace) canManage() bool { return w.Role == roleOwner || w.Role == roleAdmin }

func ensureTeamIndexes(ctx context.Context, c *mongo.Collection) error {
	_, err := c.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "owner", Value: 1}, {Key: "email", Value: 1}}, Options: options.Index().SetUnique(true).SetName("uk_owner_email")},
		{Keys: bson.D{{Key: "email", Value: 1}}, Options: options.Index().SetName("by_email")},
	})
	return err
}

func (s *store) teamOf(ctx context.Context, owner string) ([]teamMember, error) {
	cur, err := s.team.Find(ctx, bson.M{"owner": strings.ToLower(owner)}, options.Find().SetSort(bson.D{{Key: "added", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var out []teamMember
	err = cur.All(ctx, &out)
	return out, err
}

// teamsOf lists the teams an email belongs to.
func (s *store) teamsOf(ctx context.Context, email string) ([]teamMember, error) {
	cur, err := s.team.Find(ctx, bson.M{"email": strings.ToLower(email)}, options.Find().SetSort(bson.D{{Key: "added", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var out []teamMember
	err = cur.All(ctx, &out)
	return out, err
}

func (s *store) membership(ctx context.Context, owner, email string) (*teamMember, error) {
	var m teamMember
	err := s.team.FindOne(ctx, bson.M{"owner": strings.ToLower(owner), "email": strings.ToLower(email)}).Decode(&m)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

var errTeamExists = errors.New("already on the team")

func (s *store) addMember(ctx context.Context, m teamMember) error {
	_, err := s.team.InsertOne(ctx, m)
	if mongo.IsDuplicateKeyError(err) {
		return errTeamExists
	}
	return err
}

func (s *store) setMemberRole(ctx context.Context, owner, email, role string) (bool, error) {
	r, err := s.team.UpdateOne(ctx, bson.M{"owner": strings.ToLower(owner), "email": strings.ToLower(email)}, bson.M{"$set": bson.M{"role": role}})
	if err != nil {
		return false, err
	}
	return r.MatchedCount > 0, nil
}

func (s *store) removeMember(ctx context.Context, owner, email string) (bool, error) {
	r, err := s.team.DeleteOne(ctx, bson.M{"owner": strings.ToLower(owner), "email": strings.ToLower(email)})
	if err != nil {
		return false, err
	}
	return r.DeletedCount > 0, nil
}

// setWorkspace stores which workspace a user opens: "" is their own.
func (s *store) setWorkspace(ctx context.Context, email, owner string) error {
	_, err := s.users.UpdateOne(ctx, bson.M{"email": strings.ToLower(email)}, bson.M{"$set": bson.M{"workspace": strings.ToLower(owner)}})
	return err
}

// allTeams lists every team membership (for the admin page).
func (s *store) allTeams(ctx context.Context) ([]teamMember, error) {
	cur, err := s.team.Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	var out []teamMember
	err = cur.All(ctx, &out)
	return out, err
}

func workspaceName(ownerName, ownerEmail string) string {
	n := strings.TrimSpace(ownerName)
	if n == "" {
		n, _, _ = strings.Cut(ownerEmail, "@")
	}
	if strings.HasSuffix(strings.ToLower(n), "s") {
		return n + "' workspace"
	}
	return n + "'s workspace"
}

func ownWorkspace(u *User) workspace {
	return workspace{Owner: strings.ToLower(u.Email), Name: workspaceName(u.Name, u.Email), Role: roleOwner, Tenant: tenantFor(u.Email)}
}

// workspaceFor resolves the workspace a user is working in. The stored choice
// counts only while the owner still has them on the team, so removing someone
// takes effect on their very next request.
func (a *app) workspaceFor(ctx context.Context, u *User) workspace {
	if u.Workspace == "" || u.Workspace == strings.ToLower(u.Email) {
		return ownWorkspace(u)
	}
	m, err := a.store.membership(ctx, u.Workspace, u.Email)
	if err != nil || m == nil {
		return ownWorkspace(u)
	}
	ws := workspace{Owner: m.Owner, Role: m.Role, Tenant: tenantFor(m.Owner)}
	ws.Name = workspaceName("", m.Owner)
	if o, err := a.store.find(ctx, m.Owner); err == nil && o != nil {
		ws.Name = workspaceName(o.Name, o.Email)
	}
	return ws
}

// invitedByApproved reports whether an approved owner has this email on a team.
// Such people skip the admin approval gate: the owner vouches for them.
func (a *app) invitedByApproved(ctx context.Context, email string) (owner string, ok bool) {
	teams, err := a.store.teamsOf(ctx, email)
	if err != nil {
		return "", false
	}
	for _, t := range teams {
		if o, err := a.store.find(ctx, t.Owner); err == nil && o != nil && o.Approved {
			return t.Owner, true
		}
	}
	return "", false
}

// joinInvitedTeam runs at sign-up and login: an invited person is approved and,
// if they have not picked a workspace yet, opens the team's.
func (a *app) joinInvitedTeam(ctx context.Context, u *User) {
	owner, ok := a.invitedByApproved(ctx, u.Email)
	if !ok {
		return
	}
	if !u.Approved {
		if a.store.setApproved(ctx, u.Email, true) == nil {
			u.Approved = true
		}
	}
	if u.Workspace == "" {
		if a.store.setWorkspace(ctx, u.Email, owner) == nil {
			u.Workspace = owner
		}
	}
}

// --- API -------------------------------------------------------------------

type teamView struct {
	Email  string    `json:"email"`
	Name   string    `json:"name,omitempty"`
	Role   string    `json:"role"`
	Joined bool      `json:"joined"`
	Added  time.Time `json:"added"`
}

type workspaceView struct {
	Owner  string `json:"owner"` // "" for your own
	Name   string `json:"name"`
	Role   string `json:"role"`
	Active bool   `json:"active"`
}

// handleTeam: GET lists your team and the workspaces you can open; POST adds a
// person (and emails them); PATCH changes a role; DELETE removes a person.
func (a *app) handleTeam(w http.ResponseWriter, r *http.Request) {
	u := a.sessionUser(r)
	if u == nil || !u.Approved {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	owner := strings.ToLower(u.Email)

	switch r.Method {
	case http.MethodGet:
		members, err := a.store.teamOf(ctx, owner)
		if err != nil {
			writeJSON(w, 503, map[string]any{"error": "could not load team"})
			return
		}
		team := make([]teamView, 0, len(members))
		for _, m := range members {
			v := teamView{Email: m.Email, Role: m.Role, Added: m.Added}
			if p, err := a.store.find(ctx, m.Email); err == nil && p != nil {
				v.Name, v.Joined = p.Name, true
			}
			team = append(team, v)
		}
		cur := a.workspaceFor(ctx, u)
		spaces := []workspaceView{{Owner: "", Name: "My workspace", Role: roleOwner, Active: cur.Owner == owner}}
		if teams, err := a.store.teamsOf(ctx, owner); err == nil {
			for _, t := range teams {
				name := workspaceName("", t.Owner)
				if o, err := a.store.find(ctx, t.Owner); err == nil && o != nil {
					name = workspaceName(o.Name, o.Email)
				}
				spaces = append(spaces, workspaceView{Owner: t.Owner, Name: name, Role: t.Role, Active: cur.Owner == t.Owner})
			}
		}
		writeJSON(w, 200, map[string]any{"team": team, "workspaces": spaces})

	case http.MethodPost, http.MethodPatch:
		var in struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
		email := strings.ToLower(strings.TrimSpace(in.Email))
		role := in.Role
		if role != roleAdmin {
			role = roleMember
		}
		if r.Method == http.MethodPatch {
			ok, err := a.store.setMemberRole(ctx, owner, email, role)
			if err != nil {
				writeJSON(w, 503, map[string]any{"error": "could not update"})
				return
			}
			if !ok {
				writeJSON(w, 404, map[string]any{"error": "not on your team"})
				return
			}
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
		if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email || !strings.Contains(email, ".") {
			writeJSON(w, 400, map[string]any{"error": "Enter a valid email address."})
			return
		}
		if email == owner {
			writeJSON(w, 400, map[string]any{"error": "That's you. You already own this workspace."})
			return
		}
		if cur, err := a.store.teamOf(ctx, owner); err == nil && len(cur) >= maxTeam {
			writeJSON(w, 400, map[string]any{"error": fmt.Sprintf("A team can have up to %d people.", maxTeam)})
			return
		}
		err := a.store.addMember(ctx, teamMember{Owner: owner, Email: email, Role: role, Added: time.Now()})
		if err == errTeamExists {
			writeJSON(w, 409, map[string]any{"error": "Already on your team."})
			return
		}
		if err != nil {
			writeJSON(w, 503, map[string]any{"error": "could not add"})
			return
		}
		// An existing account joins right away: approved, and opening this
		// workspace unless they already picked one.
		existing, _ := a.store.find(ctx, email)
		if existing != nil {
			a.joinInvitedTeam(ctx, existing)
		}
		go a.sendTeamInvite(email, u.Name, u.Email, role, existing != nil)
		writeJSON(w, 200, map[string]any{"ok": true, "joined": existing != nil})

	case http.MethodDelete:
		email := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("email")))
		ok, err := a.store.removeMember(ctx, owner, email)
		if err != nil {
			writeJSON(w, 503, map[string]any{"error": "could not remove"})
			return
		}
		if !ok {
			writeJSON(w, 404, map[string]any{"error": "not on your team"})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})

	default:
		writeJSON(w, 405, map[string]any{"error": "method not allowed"})
	}
}

// handleWorkspace switches the workspace you open ({"owner": ""} is your own)
// or leaves a team ({"owner": "...", "leave": true}).
func (a *app) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	u := a.sessionUser(r)
	if u == nil || !u.Approved {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return
	}
	var in struct {
		Owner string `json:"owner"`
		Leave bool   `json:"leave"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
	owner := strings.ToLower(strings.TrimSpace(in.Owner))
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if owner != "" && owner != strings.ToLower(u.Email) {
		m, err := a.store.membership(ctx, owner, u.Email)
		if err != nil || m == nil {
			writeJSON(w, 404, map[string]any{"error": "You're not on that team."})
			return
		}
		if in.Leave {
			if _, err := a.store.removeMember(ctx, owner, u.Email); err != nil {
				writeJSON(w, 503, map[string]any{"error": "could not leave"})
				return
			}
			owner = ""
		}
	} else {
		owner = ""
	}
	if err := a.store.setWorkspace(ctx, u.Email, owner); err != nil {
		writeJSON(w, 503, map[string]any{"error": "could not switch"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// sendTeamInvite tells someone they were added to a workspace. People without
// an account get a sign-up link with their email filled in.
func (a *app) sendTeamInvite(to, ownerName, ownerEmail, role string, hasAccount bool) {
	base := strings.TrimRight(envOr("PUBLIC_BASE_URL", "https://www.clusterheart.com"), "/")
	who := strings.TrimSpace(ownerName)
	if who == "" {
		who = ownerEmail
	}
	link, cta := base+"/signup?email="+url.QueryEscape(to), "Create your account"
	if hasAccount {
		link, cta = base+"/app", "Open the dashboard"
	}
	can := "see every cluster and server, read their health and history, and ask the AI assistant"
	if role == roleAdmin {
		can = "see and manage every cluster and server: add, rename and remove them, and act on incidents and fixes"
	}
	body := fmt.Sprintf(`<p>Hi,</p>
<p><b>%s</b> (%s) added you to <b>%s</b> on Clusterheart. You can %s.</p>
<p style="margin:26px 0"><a href="%s" style="background:#000;color:#fff;text-decoration:none;font-weight:600;padding:12px 22px;display:inline-block">%s</a></p>
<p style="color:#525252;font-size:13px">Or paste this link into your browser:<br>%s</p>
<p style="color:#525252;font-size:13px">%s</p>`,
		htmlEscape(who), htmlEscape(ownerEmail), htmlEscape(workspaceName(ownerName, ownerEmail)), can, link, cta, link,
		map[bool]string{true: "Sign in with this email and you'll land in the shared workspace.", false: "Sign up with this email address and you'll land in the shared workspace, no approval wait."}[hasAccount])
	if err := sendMail(to, who+" added you to Clusterheart", body); err != nil {
		log.Printf("team: invite to %s failed: %v", to, err)
		return
	}
	log.Printf("team: invite sent to %s (from %s)", to, ownerEmail)
}
