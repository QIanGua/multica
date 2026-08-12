export interface EnvAssignment {
  key: string;
  value: string;
}

/**
 * Why the line number matters: a paste or a bulk edit can carry dozens of
 * lines, and "couldn't parse" without a location leaves the user guessing.
 * `line` is 1-based and counts every physical line, including the blanks and
 * comments the parser skips, so it matches what the editor shows.
 */
export type EnvParseError =
  | { kind: "malformed"; line: number }
  | { kind: "duplicate"; line: number; key: string };

export type EnvParseResult =
  | { ok: true; assignments: EnvAssignment[] }
  | { ok: false; error: EnvParseError };

const ASSIGNMENT_PATTERN =
  /^(?:export[\t ]+)?([A-Za-z_][A-Za-z0-9_]*)[\t ]*=(.*)$/;
const ASSIGNMENT_PREFIX_PATTERN =
  /^(?:export[\t ]+)?[A-Za-z_][A-Za-z0-9_]*[\t ]*=/;

function parseQuotedValue(rawValue: string): string | null {
  const quote = rawValue[0];
  if (quote !== '"' && quote !== "'") return null;

  for (let index = 1; index < rawValue.length; index += 1) {
    if (rawValue[index] === quote) {
      const remainder = rawValue.slice(index + 1);
      if (remainder.trim() === "" || /^[\t ]+#/.test(remainder)) {
        return rawValue.slice(1, index);
      }
    }
  }

  return null;
}

function parseUnquotedValue(
  rawValue: string,
  hadLeadingWhitespace: boolean,
): string {
  for (let index = 0; index < rawValue.length; index += 1) {
    const character = rawValue[index];

    if (
      character === "#" &&
      ((index === 0 && hadLeadingWhitespace) ||
        /\s/.test(rawValue[index - 1] ?? ""))
    ) {
      return rawValue.slice(0, index).trimEnd();
    }
  }

  return rawValue.trimEnd();
}

function parseValue(rawValue: string): string | null {
  const hadLeadingWhitespace = /^[\t ]/.test(rawValue);
  const value = rawValue.trimStart();
  if (value.startsWith('"') || value.startsWith("'")) {
    return parseQuotedValue(value);
  }

  return parseUnquotedValue(value, hadLeadingWhitespace);
}

export function isEnvFilePaste(text: string): boolean {
  const trimmedText = text.trim();
  if (!trimmedText) return false;

  const meaningfulLines = trimmedText
    .replace(/\r\n?/g, "\n")
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "" && !line.startsWith("#"));

  return meaningfulLines.some((line) => ASSIGNMENT_PREFIX_PATTERN.test(line));
}

/**
 * Parse a dotenv-style file without evaluating or rewriting values, reporting
 * where the first bad line is. An input with no assignments at all is a
 * success with an empty list — that is how bulk editing expresses "clear
 * everything". Callers that need "this isn't an env file" should use
 * `parseEnvFile`.
 */
export function parseEnvFileResult(text: string): EnvParseResult {
  const assignments: EnvAssignment[] = [];
  const keys = new Set<string>();
  const lines = text.replace(/\r\n?/g, "\n").split("\n");

  for (const [index, line] of lines.entries()) {
    const trimmedLine = line.trim();
    if (trimmedLine === "" || trimmedLine.startsWith("#")) continue;

    const lineNumber = index + 1;
    const match = ASSIGNMENT_PATTERN.exec(trimmedLine);
    if (!match) {
      return { ok: false, error: { kind: "malformed", line: lineNumber } };
    }

    const value = parseValue(match[2] ?? "");
    if (value === null) {
      return { ok: false, error: { kind: "malformed", line: lineNumber } };
    }

    const key = match[1] ?? "";
    if (keys.has(key)) {
      return { ok: false, error: { kind: "duplicate", line: lineNumber, key } };
    }

    keys.add(key);
    assignments.push({ key, value });
  }

  return { ok: true, assignments };
}

/** Parse a pasted dotenv-style file without evaluating or rewriting values. */
export function parseEnvFile(text: string): EnvAssignment[] | null {
  const result = parseEnvFileResult(text);
  if (!result.ok || result.assignments.length === 0) return null;
  return result.assignments;
}

/**
 * Quote only when a bare value would not survive a round trip: the parser
 * trims surrounding whitespace, treats a leading quote as an opening quote,
 * and cuts an inline ` #` comment. Everything else — `$`, backslashes,
 * backticks, embedded quotes — is literal on both sides and stays bare.
 */
function needsQuotes(value: string): boolean {
  if (value === "") return false;
  if (value !== value.trim()) return true;
  if (value.startsWith('"') || value.startsWith("'")) return true;
  return /\s#/.test(value);
}

/** Serialize entries back into the dotenv text `parseEnvFileResult` accepts. */
export function formatEnvFile(assignments: EnvAssignment[]): string {
  return assignments
    .map(({ key, value }) =>
      needsQuotes(value) ? `${key}="${value}"` : `${key}=${value}`,
    )
    .join("\n");
}
