package conformance

import (
	"context"
	"strings"
	"testing"

	"golang.org/x/oauth2"
	"google.golang.org/api/admin/directory/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// TestGoogleAdminConformance drives Google's own generated client
// (google.golang.org/api/admin/directory/v1) against the
// google-admin-style adapter: option.WithEndpoint points the SDK at the
// booted engine while its serialization, query-param plumbing and
// googleapi error decoding all stay stock.
func TestGoogleAdminConformance(t *testing.T) {
	ctx := context.Background()
	base := Boot(t, "google-admin-style")

	svc, err := admin.NewService(ctx,
		option.WithEndpoint(base),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "conformance-token"})),
	)
	if err != nil {
		t.Fatalf("admin.NewService: %v", err)
	}

	// ===== Users.List Customer=my_customer returns the seeded directory =====
	all, err := svc.Users.List().Customer("my_customer").Do()
	if err != nil {
		t.Fatalf("users.list: %v", err)
	}
	if all.Kind != "admin#directory#users" {
		t.Errorf("users.list: kind=%q", all.Kind)
	}
	emails := map[string]bool{}
	for _, u := range all.Users {
		emails[u.PrimaryEmail] = true
	}
	if !emails["admin@mock-domain.com"] || !emails["alice@mock-domain.com"] || !emails["bob@mock-domain.com"] {
		t.Errorf("users.list: want seeded admin+alice+bob, got %v", emails)
	}
	Record(t, "google-api-go-client", "google-admin-style", "Users.List Customer=my_customer returns the seeded directory")

	// ===== Users.Insert + Users.Get round-trip by numeric id and email =====
	created, err := svc.Users.Insert(&admin.User{
		PrimaryEmail: "carol@mock-domain.com",
		Name:         &admin.UserName{GivenName: "Carol", FamilyName: "Ng", FullName: "Carol Ng"},
		OrgUnitPath:  "/Engineering",
	}).Do()
	if err != nil {
		t.Fatalf("users.insert: %v", err)
	}
	if created.Id == "" || created.Kind != "admin#directory#user" || created.Suspended {
		t.Fatalf("users.insert: got id=%q kind=%q suspended=%v", created.Id, created.Kind, created.Suspended)
	}
	byID, err := svc.Users.Get(created.Id).Do()
	if err != nil {
		t.Fatalf("users.get by id: %v", err)
	}
	if byID.PrimaryEmail != "carol@mock-domain.com" || byID.OrgUnitPath != "/Engineering" || byID.Name.GivenName != "Carol" {
		t.Errorf("users.get by id: email=%q orgUnitPath=%q name=%+v", byID.PrimaryEmail, byID.OrgUnitPath, byID.Name)
	}
	byEmail, err := svc.Users.Get("carol@mock-domain.com").Do()
	if err != nil {
		t.Fatalf("users.get by email: %v", err)
	}
	if byEmail.Id != created.Id {
		t.Errorf("users.get by email: id=%q want %q", byEmail.Id, created.Id)
	}
	Record(t, "google-api-go-client", "google-admin-style", "Users.Insert + Users.Get round-trip by numeric id and email")

	// ===== Users.List Domain narrows to a single email domain =====
	inDomain, err := svc.Users.List().Domain("mock-domain.com").Do()
	if err != nil {
		t.Fatalf("users.list domain: %v", err)
	}
	if len(inDomain.Users) != 4 {
		t.Errorf("users.list domain: len=%d want 4 (3 seeded + carol)", len(inDomain.Users))
	}
	for _, u := range inDomain.Users {
		if !strings.HasSuffix(u.PrimaryEmail, "@mock-domain.com") {
			t.Errorf("users.list domain: foreign email %q leaked", u.PrimaryEmail)
		}
	}
	none, err := svc.Users.List().Domain("nowhere.example").Do()
	if err != nil {
		t.Fatalf("users.list empty domain: %v", err)
	}
	if len(none.Users) != 0 {
		t.Errorf("users.list empty domain: len=%d want 0", len(none.Users))
	}
	Record(t, "google-api-go-client", "google-admin-style", "Users.List Domain narrows to a single email domain")

	// ===== Users.List Query isSuspended=true returns only suspended users =====
	suspended, err := svc.Users.List().Customer("my_customer").Query("isSuspended=true").Do()
	if err != nil {
		t.Fatalf("users.list isSuspended: %v", err)
	}
	if len(suspended.Users) != 1 || suspended.Users[0].PrimaryEmail != "bob@mock-domain.com" {
		t.Fatalf("users.list isSuspended=true: want only bob, got %d users", len(suspended.Users))
	}
	if !suspended.Users[0].Suspended {
		t.Errorf("users.list isSuspended=true: bob.Suspended=false")
	}
	Record(t, "google-api-go-client", "google-admin-style", "Users.List Query isSuspended=true returns only suspended users")

	// ===== Users.Update renames the primary email; old key stops resolving =====
	renamed, err := svc.Users.Update("carol@mock-domain.com", &admin.User{
		PrimaryEmail: "carol.ng@mock-domain.com",
	}).Do()
	if err != nil {
		t.Fatalf("users.update rename: %v", err)
	}
	if renamed.PrimaryEmail != "carol.ng@mock-domain.com" || renamed.Id != created.Id {
		t.Errorf("users.update: email=%q id=%q", renamed.PrimaryEmail, renamed.Id)
	}
	if _, err := svc.Users.Get("carol.ng@mock-domain.com").Do(); err != nil {
		t.Errorf("users.get by renamed email: %v", err)
	}
	if _, err := svc.Users.Get("carol@mock-domain.com").Do(); err == nil {
		t.Errorf("users.get by old email: want error after rename, got nil")
	}
	Record(t, "google-api-go-client", "google-admin-style", "Users.Update renames the primary email; old key stops resolving")

	// ===== Users.Delete then Get -> decoded googleapi 404 =====
	if err := svc.Users.Delete(created.Id).Do(); err != nil {
		t.Fatalf("users.delete: %v", err)
	}
	_, err = svc.Users.Get(created.Id).Do()
	if gErr, ok := err.(*googleapi.Error); !ok || gErr.Code != 404 {
		t.Errorf("users.get after delete: want googleapi 404, got %v", err)
	}
	Record(t, "google-api-go-client", "google-admin-style", "Users.Delete then Get -> decoded googleapi 404")

	// ===== Groups.Insert + Members.Insert + Members.List round-trip =====
	group, err := svc.Groups.Insert(&admin.Group{
		Email:       "sdk-conformance@mock-domain.com",
		Name:        "SDK Conformance",
		Description: "created by google-api-go-client",
	}).Do()
	if err != nil {
		t.Fatalf("groups.insert: %v", err)
	}
	if group.Id == "" || group.Kind != "admin#directory#group" || !group.AdminCreated {
		t.Fatalf("groups.insert: id=%q kind=%q adminCreated=%v", group.Id, group.Kind, group.AdminCreated)
	}
	member, err := svc.Members.Insert(group.Email, &admin.Member{
		Email: "alice@mock-domain.com",
		Role:  "MANAGER",
		Type:  "USER",
	}).Do()
	if err != nil {
		t.Fatalf("members.insert: %v", err)
	}
	if member.Email != "alice@mock-domain.com" || member.Role != "MANAGER" || member.Status != "ACTIVE" {
		t.Errorf("members.insert: email=%q role=%q status=%q", member.Email, member.Role, member.Status)
	}
	got, err := svc.Groups.Get(group.Email).Do()
	if err != nil {
		t.Fatalf("groups.get: %v", err)
	}
	if got.Email != group.Email || got.Name != "SDK Conformance" {
		t.Errorf("groups.get: email=%q name=%q", got.Email, got.Name)
	}
	members, err := svc.Members.List(group.Email).Do()
	if err != nil {
		t.Fatalf("members.list: %v", err)
	}
	if len(members.Members) != 1 || members.Members[0].Email != "alice@mock-domain.com" {
		t.Errorf("members.list: want [alice], got %+v", members.Members)
	}
	Record(t, "google-api-go-client", "google-admin-style", "Groups.Insert + Members.Insert + Members.List round-trip")

	// ===== Groups.List UserKey returns only the groups holding that member =====
	hers, err := svc.Groups.List().Domain("mock-domain.com").UserKey("alice@mock-domain.com").Do()
	if err != nil {
		t.Fatalf("groups.list userKey: %v", err)
	}
	gotEmails := map[string]bool{}
	for _, g := range hers.Groups {
		gotEmails[g.Email] = true
	}
	// alice is a seeded MEMBER of engineering@ and all-staff@, plus the
	// MANAGER row inserted above — userKey matches across roles.
	if !gotEmails["engineering@mock-domain.com"] || !gotEmails["all-staff@mock-domain.com"] || !gotEmails["sdk-conformance@mock-domain.com"] {
		t.Errorf("groups.list userKey: want engineering+all-staff+sdk-conformance, got %v", gotEmails)
	}
	Record(t, "google-api-go-client", "google-admin-style", "Groups.List UserKey returns only the groups holding that member")
}
