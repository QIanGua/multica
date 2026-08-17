import { describe, expect, it } from "vitest";
import {
  actorRefsFromValue,
  formatActorRef,
  isActorPropertyType,
  isKnownPropertyType,
  parseActorRef,
} from "./property";

const MEMBER = "11111111-2222-3333-4444-555555555555";
const SECOND_MEMBER = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";
const AGENT = "66666666-7777-8888-9999-000000000000";

describe("isKnownPropertyType", () => {
  it("accepts the actor types", () => {
    expect(isKnownPropertyType("actor")).toBe(true);
    expect(isKnownPropertyType("multi_actor")).toBe(true);
  });

  it("still rejects unknown types", () => {
    expect(isKnownPropertyType("relation")).toBe(false);
  });
});

describe("isActorPropertyType", () => {
  it("covers both actor types and nothing else", () => {
    expect(isActorPropertyType("actor")).toBe(true);
    expect(isActorPropertyType("multi_actor")).toBe(true);
    expect(isActorPropertyType("select")).toBe(false);
    expect(isActorPropertyType("multi_select")).toBe(false);
  });
});

describe("parseActorRef", () => {
  it("parses member references", () => {
    expect(parseActorRef(`member:${MEMBER}`)).toEqual({ kind: "member", id: MEMBER });
  });

  it("rejects kinds outside the V1 range", () => {
    // Agents and squads are assignable but deliberately not referenceable.
    expect(parseActorRef(`agent:${AGENT}`)).toBeNull();
    expect(parseActorRef(`squad:${AGENT}`)).toBeNull();
    expect(parseActorRef(`user:${MEMBER}`)).toBeNull();
  });

  it("rejects malformed input", () => {
    expect(parseActorRef(MEMBER)).toBeNull();
    expect(parseActorRef("member:")).toBeNull();
    expect(parseActorRef(":" + MEMBER)).toBeNull();
    expect(parseActorRef(42)).toBeNull();
    expect(parseActorRef(undefined)).toBeNull();
  });
});

describe("formatActorRef", () => {
  it("round-trips through parseActorRef", () => {
    const ref = formatActorRef("member", MEMBER);
    expect(ref).toBe(`member:${MEMBER}`);
    expect(parseActorRef(ref)).toEqual({ kind: "member", id: MEMBER });
  });
});

describe("actorRefsFromValue", () => {
  it("reads a single actor value as a one-item list", () => {
    expect(actorRefsFromValue(`member:${MEMBER}`)).toEqual([{ kind: "member", id: MEMBER }]);
  });

  it("preserves multi_actor order rather than sorting", () => {
    expect(actorRefsFromValue([`member:${SECOND_MEMBER}`, `member:${MEMBER}`])).toEqual([
      { kind: "member", id: SECOND_MEMBER },
      { kind: "member", id: MEMBER },
    ]);
  });

  it("drops entries a newer backend may ship instead of throwing", () => {
    // A backend that later widens the kind set must not blank the whole value
    // on an older client — the unknown entries drop, the known ones render.
    expect(actorRefsFromValue([`member:${MEMBER}`, `agent:${AGENT}`, "garbage"])).toEqual([
      { kind: "member", id: MEMBER },
    ]);
  });

  it("returns an empty list for unset or wrongly-typed values", () => {
    expect(actorRefsFromValue(undefined)).toEqual([]);
    expect(actorRefsFromValue(true)).toEqual([]);
    expect(actorRefsFromValue(7)).toEqual([]);
  });
});
