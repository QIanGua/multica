import { describe, expect, it, vi } from "vitest";
import type { Editor } from "@tiptap/core";

import { shouldShowBubbleMenu } from "./bubble-menu";

function createEditor({ inTable = false }: { inTable?: boolean } = {}) {
  return {
    isEditable: true,
    isActive: vi.fn((name: string) => name === "table" && inTable),
    state: {
      selection: { empty: false, from: 1, to: 5 },
      doc: {
        textBetween: vi.fn(() => "text"),
        resolve: vi.fn(() => ({ parent: { type: { name: "paragraph" } } })),
      },
    },
  } as unknown as Editor;
}

describe("shouldShowBubbleMenu", () => {
  it("hides the formatting toolbar for text selected inside a table", () => {
    expect(shouldShowBubbleMenu(createEditor({ inTable: true }))).toBe(false);
  });

  it("shows the formatting toolbar for ordinary text selections", () => {
    expect(shouldShowBubbleMenu(createEditor())).toBe(true);
  });
});
