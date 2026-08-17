import { describe, it, expect } from "vitest";
import {
  actorFilterValues,
  hasActorPropertyFilterSelection,
} from "./filter-chips-bar";

const ALICE = "11111111-1111-4111-8111-111111111111";
const BOB = "22222222-2222-4222-8222-222222222222";

describe("actorFilterValues", () => {
  it("turns stored references into actor refs for the avatar stack", () => {
    expect(actorFilterValues([`member:${ALICE}`, `member:${BOB}`])).toEqual([
      { type: "member", id: ALICE },
      { type: "member", id: BOB },
    ]);
  });

  it("drops entries it cannot parse instead of rendering a broken avatar", () => {
    expect(actorFilterValues([`member:${ALICE}`, "garbage", `agent:${BOB}`])).toEqual([
      { type: "member", id: ALICE },
    ]);
  });

  it("returns nothing for an empty selection", () => {
    expect(actorFilterValues([])).toEqual([]);
  });
});

describe("hasActorPropertyFilterSelection", () => {
  const properties = [
    { id: "p-actor", type: "actor" },
    { id: "p-multi", type: "multi_actor" },
    { id: "p-select", type: "select" },
  ];

  // The regression Elon flagged: without this the member directory never
  // loads, so a "Reviewer = Bohan" chip can only show a bare count.
  it("is true when an actor property is filtered", () => {
    expect(
      hasActorPropertyFilterSelection({ "p-actor": [`member:${ALICE}`] }, properties),
    ).toBe(true);
    expect(
      hasActorPropertyFilterSelection({ "p-multi": [`member:${ALICE}`] }, properties),
    ).toBe(true);
  });

  it("is false for non-actor property filters", () => {
    expect(hasActorPropertyFilterSelection({ "p-select": ["opt-1"] }, properties)).toBe(
      false,
    );
  });

  it("ignores an actor property whose selection is empty", () => {
    expect(hasActorPropertyFilterSelection({ "p-actor": [] }, properties)).toBe(false);
  });

  it("is false while the property catalog is still loading", () => {
    // The catalog query resolves after this runs on first render; the member
    // query simply enables on the next pass rather than throwing.
    expect(hasActorPropertyFilterSelection({ "p-actor": [`member:${ALICE}`] }, [])).toBe(
      false,
    );
  });
});
