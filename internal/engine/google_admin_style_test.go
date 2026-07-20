package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestGoogleAdminStyleAdapter exercises the google-admin-style adapter:
//
//   - list users (seeded) → users with primaryEmail, orgUnitPath
//   - create user → appears in listing
//   - get user by key (email)
//   - list groups (seeded) → groups
//   - create group → appears in listing
//   - list group members
//   - 401 without bearer
func TestGoogleAdminStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "google-admin-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"gadmin": {Adapter: absAdapterDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer cancel()
	time.Sleep(50 * time.Millisecond)

	base := addrs["gadmin"]
	const token = "ya29.mock-admin-token"

	// ===== List users (seeded) =====

	body, status := gadminGetAuth(t, base+"/admin/directory/v1/users", token)
	if status != 200 {
		t.Fatalf("list users -> status %d, want 200; body %s", status, body)
	}
	var usersResp map[string]any
	if err := json.Unmarshal([]byte(body), &usersResp); err != nil {
		t.Fatalf("unmarshal users: %v (body %s)", err, body)
	}
	if usersResp["kind"] != "admin#directory#users" {
		t.Fatalf("kind = %v, want admin#directory#users", usersResp["kind"])
	}
	users, ok := usersResp["users"].([]any)
	if !ok || len(users) == 0 {
		t.Fatalf("users = %v, want non-empty list", usersResp["users"])
	}
	firstUser := users[0].(map[string]any)
	if _, ok := firstUser["primaryEmail"].(string); !ok {
		t.Fatalf("primaryEmail = %v, want string", firstUser["primaryEmail"])
	}
	if _, ok := firstUser["orgUnitPath"].(string); !ok {
		t.Fatalf("orgUnitPath = %v, want string", firstUser["orgUnitPath"])
	}
	if _, ok := firstUser["id"].(string); !ok {
		t.Fatalf("id = %v, want string", firstUser["id"])
	}

	// ===== Create user =====

	newUser := map[string]any{
		"primaryEmail": "charlie@mock-domain.com",
		"name": map[string]any{
			"fullName":   "Charlie Brown",
			"familyName": "Brown",
			"givenName":  "Charlie",
		},
		"orgUnitPath": "/Engineering",
	}
	body, status = gadminPostJSONAuth(t, base+"/admin/directory/v1/users", token, newUser)
	if status != 200 {
		t.Fatalf("create user -> status %d, want 200; body %s", status, body)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("unmarshal created user: %v (body %s)", err, body)
	}
	if created["primaryEmail"] != "charlie@mock-domain.com" {
		t.Fatalf("created primaryEmail = %v, want charlie@mock-domain.com", created["primaryEmail"])
	}
	createdID, ok := created["id"].(string)
	if !ok || createdID == "" {
		t.Fatalf("created id = %v, want non-empty string", created["id"])
	}

	// ===== Created user appears in listing =====

	body, status = gadminGetAuth(t, base+"/admin/directory/v1/users", token)
	if status != 200 {
		t.Fatalf("list users after create -> status %d, want 200", status)
	}
	json.Unmarshal([]byte(body), &usersResp)
	users = usersResp["users"].([]any)
	foundCharlie := false
	for _, u := range users {
		um := u.(map[string]any)
		if um["primaryEmail"] == "charlie@mock-domain.com" {
			foundCharlie = true
		}
	}
	if !foundCharlie {
		t.Fatal("created user charlie@mock-domain.com not found in listing")
	}

	// ===== Get user by email =====

	body, status = gadminGetAuth(t, base+"/admin/directory/v1/users/charlie@mock-domain.com", token)
	if status != 200 {
		t.Fatalf("get user -> status %d, want 200; body %s", status, body)
	}
	var fetched map[string]any
	if err := json.Unmarshal([]byte(body), &fetched); err != nil {
		t.Fatalf("unmarshal fetched user: %v (body %s)", err, body)
	}
	if fetched["kind"] != "admin#directory#user" {
		t.Fatalf("kind = %v, want admin#directory#user", fetched["kind"])
	}

	// ===== List groups (seeded) =====

	body, status = gadminGetAuth(t, base+"/admin/directory/v1/groups", token)
	if status != 200 {
		t.Fatalf("list groups -> status %d, want 200; body %s", status, body)
	}
	var groupsResp map[string]any
	if err := json.Unmarshal([]byte(body), &groupsResp); err != nil {
		t.Fatalf("unmarshal groups: %v (body %s)", err, body)
	}
	groups, ok := groupsResp["groups"].([]any)
	if !ok || len(groups) == 0 {
		t.Fatalf("groups = %v, want non-empty list", groupsResp["groups"])
	}

	// ===== Create group =====

	newGroup := map[string]any{
		"email":       "newteam@mock-domain.com",
		"name":        "New Team",
		"description": "A new team group",
	}
	body, status = gadminPostJSONAuth(t, base+"/admin/directory/v1/groups", token, newGroup)
	if status != 200 {
		t.Fatalf("create group -> status %d, want 200; body %s", status, body)
	}

	// Created group appears in listing.
	body, status = gadminGetAuth(t, base+"/admin/directory/v1/groups", token)
	json.Unmarshal([]byte(body), &groupsResp)
	groups = groupsResp["groups"].([]any)
	foundNewTeam := false
	for _, g := range groups {
		gm := g.(map[string]any)
		if gm["email"] == "newteam@mock-domain.com" {
			foundNewTeam = true
		}
	}
	if !foundNewTeam {
		t.Fatal("created group newteam@mock-domain.com not found in listing")
	}

	// ===== List group members =====

	body, status = gadminGetAuth(t, base+"/admin/directory/v1/groups/engineering@mock-domain.com/members", token)
	if status != 200 {
		t.Fatalf("list members -> status %d, want 200; body %s", status, body)
	}
	var membersResp map[string]any
	if err := json.Unmarshal([]byte(body), &membersResp); err != nil {
		t.Fatalf("unmarshal members: %v (body %s)", err, body)
	}
	members, ok := membersResp["members"].([]any)
	if !ok || len(members) == 0 {
		t.Fatalf("members = %v, want non-empty list", membersResp["members"])
	}

	// ===== 401 without bearer =====

	body, status = gadminGetAuth(t, base+"/admin/directory/v1/users", "")
	if status != 401 {
		t.Fatalf("list users without token -> status %d, want 401; body %s", status, body)
	}
}

// === Helpers ===

func gadminGetAuth(t *testing.T, url, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func gadminPostJSONAuth(t *testing.T, url, token string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// Ensure strings import is used.
var _ = strings.Contains
