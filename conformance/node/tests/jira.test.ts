import { describe, expect, test } from "bun:test";
import { bootAdapter } from "../helpers";
import { createCloudClient } from "jira.js";

// Drives jira.js (the de-facto standard Jira Cloud JavaScript client)
// against the jira-style adapter: Basic auth, issue CRUD, JQL search, and
// transitions through the SDK's own typed serialization. host pins it
// locally.
describe("jira.js against jira-style", () => {
  test(
    "issues CRUD, JQL search, transition",
    async () => {
      const h = await bootAdapter("jira-style");
      try {
        const jira = createCloudClient({
          host: h.base,
          auth: { type: "basic", email: "sdk@example.test", apiToken: "local-token" },
        });

        // ===== myself + project resolve =====
        const me = await jira.myself.getCurrentUser();
        expect(me.accountId ?? me.emailAddress ?? me.name).toBeTruthy();
        const projects = await jira.projects.searchProjects();
        expect((projects.values ?? []).some((p: any) => p.key === "TEST")).toBe(true);

        // ===== create assigns a TEST-n key =====
        const created = await jira.issues.createIssue({
          fields: {
            project: { key: "TEST" },
            summary: "SDK conformance issue",
            issuetype: { name: "Task" },
            description: "created by jira.js",
          },
        });
        expect(created.key).toBeTruthy();
        expect(created.key!.startsWith("TEST-")).toBe(true);

        // ===== get round-trips; edit persists =====
        const got = await jira.issues.getIssue({ issueIdOrKey: created.key!, });
        expect(got.fields!.summary).toBe("SDK conformance issue");
        await jira.issues.editIssue({
          issueIdOrKey: created.key!,
          fields: { summary: "renamed by jira.js" },
        });
        const after = await jira.issues.getIssue({ issueIdOrKey: created.key!, });
        expect(after.fields!.summary).toBe("renamed by jira.js");

        // ===== JQL search finds it =====
        // v6 ships Atlassian's enhanced search (POST /rest/api/3/search/jql)
        const results = await jira.issueSearch.searchAndReconsileIssuesUsingJqlPost({
          jql: 'project = TEST AND summary ~ "renamed"',
          maxResults: 20,
        });
        expect((results.issues ?? []).some((i: any) => i.key === created.key)).toBe(true);

        // ===== add comment + list =====
        await jira.issueComments.addComment({
          issueIdOrKey: created.key!,
          body: "a comment from jira.js",
        });
        const comments = await jira.issueComments.getComments({
          issueIdOrKey: created.key!,
        });
        expect((comments.comments ?? [])[0]?.body).toBe("a comment from jira.js");

        // ===== transition moves the status =====
        const transitions = await jira.issues.getTransitions({
          issueIdOrKey: created.key!,
        });
        expect((transitions.transitions ?? []).length).toBeGreaterThan(0);
        const target = (transitions.transitions ?? [])[0];
        await jira.issues.doTransition({
          issueIdOrKey: created.key!,
          transition: { id: target.id! },
        });
        const moved = await jira.issues.getIssue({ issueIdOrKey: created.key! });
        expect(moved.fields!.status!.name).toBe(target.to?.name ?? "In Progress");

        // ===== delete removes it =====
        await jira.issues.deleteIssue({ issueIdOrKey: created.key!, });
        let gone = false;
        try {
          await jira.issues.getIssue({ issueIdOrKey: created.key!, });
        } catch {
          gone = true;
        }
        expect(gone).toBe(true);
      } finally {
        await h.stop();
      }
    },
    { timeout: 30_000 },
  );
});
