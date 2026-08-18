// @vitest-environment node
import { describe, expect, it } from "vitest";
import { buildIssueStatusCatalog } from "../issue-statuses";
import type { Issue, IssueStatusEntry } from "../types";
import { issueBehavesAs, issueBehavesAsAny, statusFilterColumns } from "./status-category";

function issue(status: string, statusCategory?: string): Pick<Issue, "status" | "status_category"> {
  return { status, status_category: statusCategory } as Pick<Issue, "status" | "status_category">;
}

function entry(key: string, category: string, isSystem = false): IssueStatusEntry {
  return {
    id: key,
    workspace_id: "ws-1",
    key,
    name: key,
    description: "",
    category: category as IssueStatusEntry["category"],
    color: "#000000",
    is_system: isSystem,
    position: 0,
    archived_at: null,
    created_at: "",
    updated_at: "",
  };
}

/**
 * Every status-coupled product rule asks "does this issue behave as X". These
 * are the cases where comparing the raw key gives the wrong answer.
 */
describe("issueBehavesAs", () => {
  it("answers for a built-in with no catalog and no server hint", () => {
    expect(issueBehavesAs(issue("done"), "done")).toBe(true);
    expect(issueBehavesAs(issue("todo"), "done")).toBe(false);
  });

  // The regression: a custom status in the done category is finished work.
  // Code comparing `status === "done"` kept it visible under "hide completed",
  // left it out of sub-issue progress, and ranked it as live in search.
  it("answers for a custom status from the payload's category", () => {
    expect(issueBehavesAs(issue("shipped", "done"), "done")).toBe(true);
    expect(issueBehavesAs(issue("shipped", "done"), "in_review")).toBe(false);
  });

  it("treats a custom backlog status as backlog", () => {
    expect(issueBehavesAs(issue("someday", "backlog"), "backlog")).toBe(true);
  });

  // Fails toward showing more / asking more, never toward hiding work or
  // starting an agent without confirmation.
  it("answers false for a custom key this response did not resolve", () => {
    expect(issueBehavesAs(issue("shipped"), "done")).toBe(false);
    expect(issueBehavesAs(issue("someday"), "backlog")).toBe(false);
  });

  it("matches any of several categories", () => {
    expect(issueBehavesAsAny(issue("qa", "cancelled"), ["done", "cancelled"])).toBe(true);
    expect(issueBehavesAsAny(issue("qa", "in_review"), ["done", "cancelled"])).toBe(false);
  });
});

describe("statusFilterColumns", () => {
  const loaded = buildIssueStatusCatalog([entry("in_review", "in_review", true), entry("qa", "in_review")]);
  const pending = buildIssueStatusCatalog(undefined);

  it("resolves built-ins without waiting for the catalog", () => {
    expect([...statusFilterColumns(["todo", "done"], pending)]).toEqual(["todo", "done"]);
  });

  it("maps a custom key to its category once the catalog is loaded", () => {
    expect([...statusFilterColumns(["qa"], loaded)]).toEqual(["in_review"]);
  });

  // The regression: `categoryOf` answers `todo` for anything it has not loaded,
  // which is indistinguishable from a real `todo`. Routing on that guess paged
  // the todo column for a saved `qa` filter while the query still restricted
  // `status=qa` — an empty board on every cold load, permanent on a failed
  // catalog fetch. Contributing NO column is what makes the surface wait.
  it("contributes no column for a custom key while the catalog is in flight", () => {
    expect([...statusFilterColumns(["qa"], pending)]).toEqual([]);
  });

  it("does not guess a column for a key the loaded catalog has never heard of", () => {
    expect([...statusFilterColumns(["gone"], loaded)]).toEqual([]);
  });

  it("keeps built-in columns alongside an unresolved custom key", () => {
    expect([...statusFilterColumns(["todo", "qa"], pending)]).toEqual(["todo"]);
  });
});

describe("IssueStatusCatalog.hasCustomStatuses", () => {
  // This is the switch for the category-grouped server contract, so it has to
  // be false in BOTH unsafe states: catalog in flight (cold load), and a
  // workspace that has none (a backend that may predate the contract).
  it("is false while the catalog is in flight", () => {
    expect(buildIssueStatusCatalog(undefined).hasCustomStatuses).toBe(false);
  });

  it("is false for a workspace holding only the built-ins", () => {
    const builtInsOnly = ["backlog", "todo", "in_progress", "in_review", "done", "blocked", "cancelled"].map(
      (key) => entry(key, key, true),
    );
    expect(buildIssueStatusCatalog(builtInsOnly).hasCustomStatuses).toBe(false);
  });

  it("is true once a custom status exists", () => {
    expect(buildIssueStatusCatalog([entry("qa", "in_review")]).hasCustomStatuses).toBe(true);
  });
});
