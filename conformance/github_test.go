package conformance

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/go-github/v89/github"
	"golang.org/x/oauth2"
)

// TestGitHubSDKConformance drives go-github against the github-style
// adapter with the seeded static PAT: issue CRUD plus SDK-side pagination
// (go-github parses the Link header into resp.NextPage and walks it).
func TestGitHubSDKConformance(t *testing.T) {
	ctx := context.Background()
	base := Boot(t, "github-style")

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "ghp_pat_token_mock"})
	tc := oauth2.NewClient(ctx, ts)
	tc.Timeout = HTTPClient().Timeout
	// v89 encapsulated Client: NewClient is an options constructor and
	// BaseURL is set via WithURLs (WithEnterpriseURLs would append
	// api/v3/ to our plain host).
	baseURL := base + "/"
	client, err := github.NewClient(github.WithHTTPClient(tc), github.WithURLs(&baseURL, nil))
	if err != nil {
		t.Fatal(err)
	}

	// The adapter serves the seeded octocat/hello-world repo and 404s
	// unknown repos exactly like the real API.
	const owner, repo = "octocat", "hello-world"

	// ===== Create issues via the SDK =====

	for i := 1; i <= 5; i++ {
		issue, _, err := client.Issues.Create(ctx, owner, repo, &github.IssueRequest{
			Title: github.String(fmt.Sprintf("Conformance issue %d", i)),
			Body:  github.String("filed by the go-github SDK against stunt"),
		})
		if err != nil {
			t.Fatalf("Issues.Create %d: %v", i, err)
		}
		if issue.GetNumber() == 0 || issue.GetTitle() == "" {
			t.Fatalf("issue %d: number=%d title=%q", i, issue.GetNumber(), issue.GetTitle())
		}
	}
	Record(t, "go-github/v89", "github-style", "Issues.Create x5 (number assignment)")

	// ===== SDK pagination: Link header -> resp.NextPage walking =====

	var collected []*github.Issue
	opts := &github.IssueListByRepoOptions{ListOptions: github.ListOptions{PerPage: 2}}
	pages := 0
	for {
		page, resp, err := client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			t.Fatalf("Issues.ListByRepo: %v", err)
		}
		collected = append(collected, page...)
		pages++
		if resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage // Page is ambiguous since cursor opts merged in
	}
	// The seeded repo ships one pre-existing issue; ours are 5 more.
	if len(collected) < 6 {
		t.Fatalf("paginated %d issues, want >= 6 (Link header not followed?)", len(collected))
	}
	if pages < 3 {
		t.Fatalf("walked %d pages with PerPage=2 over %d issues — pagination not followed", pages, len(collected))
	}
	ours := 0
	for _, is := range collected {
		for i := 1; i <= 5; i++ {
			if is.GetTitle() == fmt.Sprintf("Conformance issue %d", i) {
				ours++
			}
		}
	}
	if ours != 5 {
		t.Fatalf("found %d/5 created issues across pages", ours)
	}
	Record(t, "go-github/v89", "github-style", "Issues.ListByRepo walks Link headers across pages")

	// ===== Comment on an issue =====

	num := collected[0].GetNumber()
	cmt, _, err := client.Issues.CreateComment(ctx, owner, repo, num, &github.IssueComment{
		Body: github.String("SDK comment"),
	})
	if err != nil {
		t.Fatalf("Issues.CreateComment: %v", err)
	}
	if cmt.GetBody() != "SDK comment" || cmt.GetID() == 0 {
		t.Fatalf("comment = %+v", cmt)
	}
	Record(t, "go-github/v89", "github-style", "Issues.CreateComment")

	listed, _, err := client.Issues.ListComments(ctx, owner, repo, num, nil)
	if err != nil {
		t.Fatalf("Issues.ListComments: %v", err)
	}
	if len(listed) == 0 || listed[len(listed)-1].GetBody() != "SDK comment" {
		t.Fatalf("ListComments = %d comments", len(listed))
	}
	Record(t, "go-github/v89", "github-style", "Issues.ListComments round-trip")

	// ===== Close an issue (state transition) =====

	closed, _, err := client.Issues.Edit(ctx, owner, repo, num, &github.IssueRequest{
		State: github.String("closed"),
	})
	if err != nil {
		t.Fatalf("Issues.Edit close: %v", err)
	}
	if closed.GetState() != "closed" {
		t.Fatalf("state = %q, want closed", closed.GetState())
	}
	Record(t, "go-github/v89", "github-style", "Issues.Edit state transition")
}
