import { describe, expect, it } from "vitest";

import {
  formatEnvFile,
  isEnvFilePaste,
  parseEnvFile,
  parseEnvFileResult,
} from "./env-file";

describe("parseEnvFile", () => {
  it("parses dotenv-style assignments while preserving literal values", () => {
    expect(
      parseEnvFile(String.raw`
# Agent credentials
export API_KEY="secret value"
EMPTY=
URL=https://example.com/path?first=one&second=two
HASH='value # kept'
PLAIN=value # ignored comment
WIN_DIR=C:\Users\me\.ssh
PASSWORD=p$ss${"`"}word
JSON={"a":1}
QUOTED='say "hello" with $HOME and C:\path'
`),
    ).toEqual([
      { key: "API_KEY", value: "secret value" },
      { key: "EMPTY", value: "" },
      {
        key: "URL",
        value: "https://example.com/path?first=one&second=two",
      },
      { key: "HASH", value: "value # kept" },
      { key: "PLAIN", value: "value" },
      { key: "WIN_DIR", value: String.raw`C:\Users\me\.ssh` },
      { key: "PASSWORD", value: "p$ss`word" },
      { key: "JSON", value: '{"a":1}' },
      { key: "QUOTED", value: String.raw`say "hello" with $HOME and C:\path` },
    ]);
  });

  it("supports Windows line endings", () => {
    expect(parseEnvFile("FIRST=one\r\nSECOND=two\r\n")).toEqual([
      { key: "FIRST", value: "one" },
      { key: "SECOND", value: "two" },
    ]);
  });

  it("rejects partial files instead of silently dropping unsupported lines", () => {
    expect(parseEnvFile("FIRST=one\necho hello\nSECOND=two")).toBeNull();
    expect(parseEnvFile('BROKEN="unterminated')).toBeNull();
    expect(parseEnvFile('TOKEN="abc"#suffix')).toBeNull();
    expect(parseEnvFile("FIRST=one\nFIRST=two")).toBeNull();
  });

  it("does not treat ordinary key text as an environment file", () => {
    expect(parseEnvFile("API_KEY")).toBeNull();
    expect(isEnvFilePaste("API_KEY")).toBe(false);
    expect(isEnvFilePaste("API_KEY\n")).toBe(false);
    expect(isEnvFilePaste("# database settings\n")).toBe(false);
    expect(isEnvFilePaste("API_KEY=value")).toBe(true);
    expect(isEnvFilePaste("FIRST=one\nSECOND=two")).toBe(true);
  });
});

describe("parseEnvFileResult", () => {
  it("reports the physical line of the first malformed line", () => {
    expect(parseEnvFileResult("# comment\n\nFIRST=one\necho hello")).toEqual({
      ok: false,
      error: { kind: "malformed", line: 4 },
    });
    expect(parseEnvFileResult('BROKEN="unterminated')).toEqual({
      ok: false,
      error: { kind: "malformed", line: 1 },
    });
  });

  it("reports which key was duplicated and where", () => {
    expect(parseEnvFileResult("FIRST=one\nSECOND=two\nFIRST=three")).toEqual({
      ok: false,
      error: { kind: "duplicate", line: 3, key: "FIRST" },
    });
  });

  it("treats an assignment-free input as an empty success", () => {
    expect(parseEnvFileResult("")).toEqual({ ok: true, assignments: [] });
    expect(parseEnvFileResult("# only a comment\n")).toEqual({
      ok: true,
      assignments: [],
    });
    // parseEnvFile keeps the stricter "this is not an env file" contract the
    // paste path depends on.
    expect(parseEnvFile("# only a comment\n")).toBeNull();
  });
});

describe("formatEnvFile", () => {
  it("round-trips values that would otherwise change meaning", () => {
    const assignments = [
      { key: "PLAIN", value: "value" },
      { key: "EMPTY", value: "" },
      { key: "WIN_DIR", value: String.raw`C:\Users\me\.ssh` },
      { key: "PASSWORD", value: "p$ss`word" },
      { key: "JSON", value: '{"a":1}' },
      { key: "PADDED", value: "  spaced  " },
      { key: "HASH", value: "value # kept" },
      { key: "LEADING_QUOTE", value: '"quoted"' },
      { key: "APOSTROPHE", value: "it's" },
    ];

    const result = parseEnvFileResult(formatEnvFile(assignments));
    expect(result).toEqual({ ok: true, assignments });
  });

  it("leaves values bare unless quoting is required", () => {
    expect(
      formatEnvFile([
        { key: "A", value: "plain" },
        { key: "B", value: "" },
        { key: "C", value: "  padded" },
      ]),
    ).toBe('A=plain\nB=\nC="  padded"');
  });
});
