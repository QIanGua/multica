// @vitest-environment node
import { describe, expect, it } from "vitest";
import { buildIssueStatusCatalog, isIssueStatusCategory } from "./queries";
import type { IssueStatusEntry } from "../types";

function entry(key: string, category: string, name = key): IssueStatusEntry {
  return {
    id: key, workspace_id: "ws-1", key, name, description: "",
    category: category as IssueStatusEntry["category"], color: "#123456",
    is_system: false, position: 0, archived_at: null,
    created_at: "", updated_at: "",
  };
}

describe("buildIssueStatusCatalog", () => {
  // The catalog is fetched async, but a status must render on the very first
  // paint. Built-in keys are their own category, so an unloaded catalog still
  // resolves all 7 — which is what keeps the default workspace identical
  // before the request lands.
  it("resolves every built-in with no catalog loaded", () => {
    const c = buildIssueStatusCatalog(undefined);
    expect(c.isLoaded).toBe(false);
    for (const key of ["backlog", "todo", "in_progress", "in_review", "done", "blocked", "cancelled"]) {
      expect(c.categoryOf(key)).toBe(key);
    }
    expect(c.labelOf("in_review")).toBe("In Review");
  });

  it("maps a custom status to its category and name", () => {
    const c = buildIssueStatusCatalog([entry("human_review", "in_review", "Human Review")]);
    expect(c.categoryOf("human_review")).toBe("in_review");
    expect(c.labelOf("human_review")).toBe("Human Review");
    expect(c.entryOf("human_review")?.key).toBe("human_review");
  });

  // An issue can carry a status created moments ago in another session. Falling
  // back to a renderable category beats dropping the issue.
  it("falls back for a status the catalog does not know", () => {
    const c = buildIssueStatusCatalog([]);
    expect(c.categoryOf("ghost")).toBe("todo");
    expect(c.labelOf("ghost")).toBe("ghost");
    expect(c.entryOf("ghost")).toBeUndefined();
  });

  it("ignores a corrupt category rather than trusting it", () => {
    const c = buildIssueStatusCatalog([entry("weird", "started")]);
    expect(c.categoryOf("weird")).toBe("todo");
  });

  it("groups by category in catalog order", () => {
    const c = buildIssueStatusCatalog([
      entry("human_review", "in_review"),
      entry("gate_approved", "done"),
    ]);
    expect(c.inCategory("in_review").map((e) => e.key)).toEqual(["human_review"]);
    expect(c.inCategory("backlog")).toEqual([]);
  });

  it("isIssueStatusCategory accepts exactly the 7", () => {
    expect(isIssueStatusCategory("in_review")).toBe(true);
    expect(isIssueStatusCategory("human_review")).toBe(false);
  });
});
