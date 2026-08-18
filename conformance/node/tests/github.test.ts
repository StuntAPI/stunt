// octokit conformance: baseUrl override + the seeded PAT, issue creates,
// and octokit.paginate walking the Link headers.
import { describe, expect, test } from "bun:test";
import { Octokit } from "octokit";
import { bootAdapter } from "../helpers";

describe("octokit against github-style", () => {
  test(
    "issue CRUD + paginate over Link headers",
    async () => {
      const h = await bootAdapter("github-style");
      try {
        const octokit = new Octokit({
          auth: "ghp_pat_token_mock",
          baseUrl: h.base,
        });

        const OWNER = "octocat",
          REPO = "hello-world";

        for (let i = 1; i <= 5; i++) {
          const { data: issue } = await octokit.rest.issues.create({
            owner: OWNER,
            repo: REPO,
            title: `node-conformance ${i}`,
            body: "filed by octokit against stunt",
          });
          expect(issue.number).toBeGreaterThan(0);
        }

        // paginate() follows the Link header through every page.
        const issues = await octokit.paginate(octokit.rest.issues.listForRepo, {
          owner: OWNER,
          repo: REPO,
          per_page: 2,
        });
        const ours = issues.filter((i) => String(i.title).startsWith("node-conformance"));
        expect(ours.length).toBe(5);
        expect(issues.length).toBeGreaterThanOrEqual(6); // + the seeded issue

        // Comment + state transition through the SDK.
        const target = ours[0];
        const { data: cmt } = await octokit.rest.issues.createComment({
          owner: OWNER,
          repo: REPO,
          issue_number: target.number,
          body: "octokit comment",
        });
        expect(cmt.body).toBe("octokit comment");

        const { data: closed } = await octokit.rest.issues.update({
          owner: OWNER,
          repo: REPO,
          issue_number: target.number,
          state: "closed",
        });
        expect(closed.state).toBe("closed");
      } finally {
        await h.stop();
      }
    },
    { timeout: 120_000 },
  );
});
