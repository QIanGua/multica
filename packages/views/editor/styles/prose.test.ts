import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

describe("table prose styles", () => {
  it("draws dividers between adjacent columns", () => {
    const css = readFileSync("editor/styles/prose.css", "utf8");

    expect(css).toMatch(
      /\.rich-text-editor th:not\(:first-child\),\s*\.rich-text-editor td:not\(:first-child\)\s*\{[^}]*border-left: 1px solid var\(--border\);/s,
    );
  });

  it("reserves editor-only space above tables for spatial controls", () => {
    const css = readFileSync("editor/styles/prose.css", "utf8");

    expect(css).toMatch(
      /\.rich-text-editor\.ProseMirror \.tableWrapper\s*\{[^}]*margin-top: 2rem;/s,
    );
  });
});
