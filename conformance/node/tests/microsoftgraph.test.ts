import { describe, expect, test } from "bun:test";
import { bootAdapter } from "../helpers";
import {
  Client,
  GraphError,
  HTTPMessageHandler,
  Middleware,
} from "@microsoft/microsoft-graph-client";

// Drives @microsoft/microsoft-graph-client (the core Graph JavaScript client)
// against the microsoft-graph-style adapter. baseUrl pins the client on the
// booted adapter (GraphRequest urlJoins baseUrl + "v1.0" + path); the bearer
// token is stamped by a middleware because v3's stock AuthenticationHandler
// only attaches tokens for https://graph.microsoft.com (or customHosts), and
// a local adapter is plain http. The adapter's documented static token is
// used; unknown tokens exercise the 401 path.
describe("@microsoft/microsoft-graph-client against microsoft-graph-style", () => {
  test(
    "me, directory, mail, calendar, teams, drive, subscriptions",
    async () => {
      const h = await bootAdapter("microsoft-graph-style");
      try {
        // makeClient builds a Client whose request pipeline pins the bearer
        // token on every request (see header comment for why not authProvider).
        const makeClient = (token: string): Client => {
          let next: Middleware | undefined;
          const bearer: Middleware = {
            async execute(ctx) {
              ctx.options.headers = {
                ...(ctx.options.headers ?? {}),
                Authorization: `Bearer ${token}`,
              };
              await next!.execute(ctx);
            },
            setNext(m) {
              next = m;
            },
          };
          return Client.initWithMiddleware({
            baseUrl: h.base,
            middleware: [bearer, new HTTPMessageHandler()],
          });
        };
        const client = makeClient("mock-bearer-token");
        const badClient = makeClient("not-a-token-the-tenant-issued");

        // ===== /me resolves the signed-in profile =====
        const me = await client.api("/me").get();
        expect(me.displayName).toBe("Alex Mockerman");
        expect(me.userPrincipalName).toBe("alex@mock-tenant.onmicrosoft.com");

        // ===== users directory pages via $top and the @odata.nextLink cursor =====
        const page1 = await client.api("/users").top(2).get();
        expect(page1.value.length).toBe(2);
        expect(page1["@odata.nextLink"]).toBeTruthy();
        // The v3 client only re-parses absolute https URLs, so the link is
        // followed by its path+query against the same adapter (minus /v1.0,
        // which the client itself prepends as the default version).
        const link = new URL(page1["@odata.nextLink"]);
        expect(link.searchParams.get("$skip")).toBe("2");
        const page2 = await client
          .api(link.pathname.replace(/^\/v1\.0/, "") + link.search)
          .get();
        expect(page2.value.length).toBe(1);
        expect(page2.value[0].id).not.toBe(page1.value[0].id);
        expect(page2["@odata.nextLink"]).toBeUndefined();
        // A user resolves by id and by UPN alike.
        const byId = await client.api(`/users/${page1.value[0].id}`).get();
        expect(byId.displayName).toBe(page1.value[0].displayName);
        const byUpn = await client
          .api(`/users/${page1.value[0].userPrincipalName}`)
          .get();
        expect(byUpn.id).toBe(page1.value[0].id);

        // ===== $filter and $select are applied server-side =====
        const qa = await client
          .api("/users")
          .filter("jobTitle eq 'QA Engineer'")
          .select(["displayName", "jobTitle"])
          .get();
        expect(qa.value.length).toBe(1);
        expect(qa.value[0].displayName).toBe("Brenda Tester");
        expect(Object.keys(qa.value[0]).sort()).toEqual([
          "displayName",
          "jobTitle",
        ]);

        // ===== draft lifecycle: create, PATCH, send, land in Sent Items =====
        const draft = await client.api("/me/messages").post({
          subject: "SDK conformance draft",
          body: { contentType: "Text", content: "drafted by the Graph SDK" },
          toRecipients: [
            { emailAddress: { address: "brenda@mock-tenant.onmicrosoft.com" } },
          ],
        });
        expect(draft.isDraft).toBe(true);
        expect(draft.receivedDateTime).toBeNull();
        const flagged = await client
          .api(`/me/messages/${draft.id}`)
          .patch({ isRead: false, flag: { flagStatus: "flagged" } });
        expect(flagged.isRead).toBe(false);
        expect(flagged.flag.flagStatus).toBe("flagged");
        await client.api(`/me/messages/${draft.id}/send`).post();
        const sentItems = await client
          .api("/me/mailFolders/sentitems/messages")
          .get();
        const sent = sentItems.value.find((m: any) => m.id === draft.id);
        expect(sent).toBeTruthy();
        expect(sent.isDraft).toBe(false);
        expect(sent.sentDateTime).toBeTruthy();

        // ===== sendMail posts straight to Sent Items (no draft) =====
        await client.api("/me/sendMail").post({
          message: {
            subject: "Sent straight from the SDK",
            body: { contentType: "Text", content: "no draft involved" },
            toRecipients: [
              {
                emailAddress: {
                  address: "charlie@mock-tenant.onmicrosoft.com",
                },
              },
            ],
          },
          saveToSentItems: true,
        });
        const afterSend = await client
          .api("/me/mailFolders/sentitems/messages")
          .get();
        expect(
          afterSend.value.some((m: any) => m.subject === "Sent straight from the SDK"),
        ).toBe(true);

        // ===== subscription lifecycle with clientState-echoed notifications =====
        const sub = await client.api("/subscriptions").post({
          changeType: "created,updated",
          notificationUrl: h.sinkUrl,
          resource: "me/messages",
          clientState: "sdk-conformance-secret",
        });
        expect(sub.id).toBeTruthy();
        expect(sub.resource).toBe("me/messages");
        expect(sub.latestSupportedTlsVersion).toBe("v1_2");
        // The validation handshake POST reaches the notification URL.
        const handshake = await h.waitFor((d) =>
          d.body.includes("validationToken"),
        );
        expect(handshake.body).toContain(sub.id);
        // A change on the subscribed resource delivers a notification that
        // echoes clientState (Graph's documented verification mechanism).
        const triggered = await client
          .api("/me/messages")
          .post({ subject: "notify me" });
        const note = await h.waitFor(
          (d) => d.body.includes("changeNotification") && d.body.includes(triggered.id),
        );
        expect(note.body).toContain("sdk-conformance-secret");
        const gotSub = await client.api(`/subscriptions/${sub.id}`).get();
        expect(gotSub.id).toBe(sub.id);
        await client.api(`/subscriptions/${sub.id}`).delete();
        let subGone = false;
        try {
          await client.api(`/subscriptions/${sub.id}`).get();
        } catch {
          subGone = true;
        }
        expect(subGone).toBe(true);

        // ===== calendar: create, calendarView window, accept records the response =====
        const evt = await client.api("/me/events").post({
          subject: "SDK conformance sync",
          start: { dateTime: "2024-10-02T09:00:00", timeZone: "UTC" },
          end: { dateTime: "2024-10-02T09:30:00", timeZone: "UTC" },
        });
        expect(evt.id).toBeTruthy();
        // The view window (October) excludes the seeded July "Weekly team sync".
        const view = await client
          .api("/me/calendarView")
          .query({
            startDateTime: "2024-10-01T00:00:00",
            endDateTime: "2024-10-31T00:00:00",
          })
          .get();
        expect(view.value.some((e: any) => e.id === evt.id)).toBe(true);
        expect(
          view.value.every((e: any) => e.subject !== "Weekly team sync"),
        ).toBe(true);
        await client
          .api(`/me/events/${evt.id}/accept`)
          .post({ comment: "See you there" });
        const accepted = await client.api(`/me/events/${evt.id}`).get();
        expect(accepted.responseStatus.response).toBe("accepted");

        // ===== teams chat: create chat, send message, list, delete =====
        const chat = await client
          .api("/me/chats")
          .post({ chatType: "oneOnOne", topic: "SDK confab" });
        expect(chat.id).toContain("@thread.v2");
        const chatMsg = await client
          .api(`/chats/${chat.id}/messages`)
          .post({ body: { content: "hello from the Graph SDK" } });
        expect(chatMsg.body.content).toBe("hello from the Graph SDK");
        const chatMsgs = await client
          .api(`/chats/${chat.id}/messages`)
          .get();
        expect(
          chatMsgs.value.some((m: any) => m.id === chatMsg.id),
        ).toBe(true);
        await client
          .api(`/chats/${chat.id}/messages/${chatMsg.id}`)
          .delete();
        const afterDelete = await client
          .api(`/chats/${chat.id}/messages`)
          .get();
        expect(
          afterDelete.value.some((m: any) => m.id === chatMsg.id),
        ).toBe(false);
        await client.api(`/chats/${chat.id}`).delete();

        // ===== drive: quota, simple upload round-trip, path resolution =====
        const drive = await client.api("/me/drive").get();
        expect(drive.driveType).toBe("business");
        expect(drive.quota.state).toBe("normal");
        const root = await client.api("/me/drive/root/children").get();
        expect(root.value.some((i: any) => i.name === "Budget.xlsx")).toBe(true);
        const bytes = "uploaded by the Graph SDK";
        const up = await client
          .api("/me/drive/root:/sdk-conformance.txt:/content")
          .header("Content-Type", "text/plain")
          .put(bytes);
        expect(up.name).toBe("sdk-conformance.txt");
        expect(up.size).toBe(bytes.length);
        const down = await client
          .api(`/me/drive/items/${up.id}/content`)
          .get();
        expect(down).toBe(bytes);
        const byPath = await client
          .api("/me/drive/root:/sdk-conformance.txt:/")
          .get();
        expect(byPath.id).toBe(up.id);

        // ===== error envelope: unknown user 404, unknown token 401 =====
        let notFound: any;
        try {
          await client.api("/users/00000000-0000-0000-0000-000000000000").get();
        } catch (e) {
          notFound = e;
        }
        expect(notFound).toBeInstanceOf(GraphError);
        expect(notFound.statusCode).toBe(404);
        expect(notFound.code).toBe("Request_ResourceNotFound");
        let unauthorized: any;
        try {
          await badClient.api("/me").get();
        } catch (e) {
          unauthorized = e;
        }
        expect(unauthorized).toBeInstanceOf(GraphError);
        expect(unauthorized.statusCode).toBe(401);
        expect(unauthorized.code).toBe("InvalidAuthenticationToken");
      } finally {
        await h.stop();
      }
    },
    { timeout: 30_000 },
  );
});
