/**
 * Custom issue properties — workspace-defined, typed fields on issues
 * (MUL-4463). Definitions live in a workspace catalog (managed by owner/admin
 * only); values live on each issue in a bag keyed by definition id, so
 * renames never touch issue rows.
 *
 * Values are typed per definition: select stores an option id, multi_select
 * an array of option ids (config order), date a "YYYY-MM-DD" string, checkbox
 * a boolean, number a number, text/url strings, actor a "<kind>:<uuid>"
 * reference string, multi_actor an array of them (insertion order).
 */
export type IssuePropertyType =
  | "text"
  | "number"
  | "select"
  | "multi_select"
  | "date"
  | "checkbox"
  | "url"
  | "actor"
  | "multi_actor";

export const ISSUE_PROPERTY_TYPES: IssuePropertyType[] = [
  "text",
  "number",
  "select",
  "multi_select",
  "date",
  "checkbox",
  "url",
  "actor",
  "multi_actor",
];

export function isKnownPropertyType(type: string): type is IssuePropertyType {
  return (ISSUE_PROPERTY_TYPES as string[]).includes(type);
}

/**
 * Actor properties (MUL-6286) reference a workspace member or agent. The
 * assignee field additionally accepts a squad; actor properties deliberately
 * do not, because a squad is a routing target rather than a person.
 *
 * Referencing an agent here is NOT an assignment: it never starts a run.
 */
export type IssuePropertyActorKind = "member" | "agent";

export const ISSUE_PROPERTY_ACTOR_KINDS: IssuePropertyActorKind[] = ["member", "agent"];

/** Upper bound on a single multi_actor value; mirrors the server cap. */
export const MAX_ISSUE_PROPERTY_ACTOR_VALUES = 20;

export interface IssuePropertyActorRef {
  kind: IssuePropertyActorKind;
  /** Members are referenced by `user_id`, matching the issue assignee pair. */
  id: string;
}

export function isActorPropertyType(type: string): boolean {
  return type === "actor" || type === "multi_actor";
}

export function formatActorRef(kind: IssuePropertyActorKind, id: string): string {
  return `${kind}:${id}`;
}

/** Returns null for anything that isn't a well-formed reference. */
export function parseActorRef(raw: unknown): IssuePropertyActorRef | null {
  if (typeof raw !== "string") return null;
  const separator = raw.indexOf(":");
  if (separator <= 0) return null;
  const kind = raw.slice(0, separator);
  const id = raw.slice(separator + 1);
  if (!id) return null;
  if (!(ISSUE_PROPERTY_ACTOR_KINDS as string[]).includes(kind)) return null;
  return { kind: kind as IssuePropertyActorKind, id };
}

/**
 * Reads an actor property value as a list, so single and multi render through
 * one code path. Malformed entries are dropped rather than thrown on: a
 * newer backend may ship kinds this client doesn't know yet.
 */
export function actorRefsFromValue(value: IssuePropertyValue | undefined): IssuePropertyActorRef[] {
  if (typeof value === "string") {
    const ref = parseActorRef(value);
    return ref ? [ref] : [];
  }
  if (Array.isArray(value)) {
    return value.map(parseActorRef).filter((ref): ref is IssuePropertyActorRef => ref !== null);
  }
  return [];
}

export interface IssuePropertyOption {
  id: string;
  name: string;
  /** Normalized lowercase hex color, e.g. `#3b82f6`. */
  color: string;
}

export interface IssuePropertyConfig {
  options?: IssuePropertyOption[];
}

export interface IssueProperty {
  id: string;
  workspace_id: string;
  name: string;
  /** Lenient string: newer servers may ship types this client doesn't know. */
  type: string;
  description?: string;
  /** Optional catalog icon key; absent on backends predating icon support. */
  icon?: string;
  config: IssuePropertyConfig;
  position: number;
  archived: boolean;
  archived_at?: string | null;
  usage_count?: number;
  created_at: string;
  updated_at: string;
}

export type IssuePropertyValue = string | number | boolean | string[];
export type IssuePropertyValues = Record<string, IssuePropertyValue>;

export interface CreatePropertyRequest {
  name: string;
  type: IssuePropertyType;
  description?: string;
  icon?: string;
  config?: IssuePropertyConfig;
}

export interface UpdatePropertyRequest {
  name?: string;
  description?: string;
  /** Empty string clears the icon. */
  icon?: string;
  config?: IssuePropertyConfig;
  archived?: boolean;
}

export interface ListPropertiesResponse {
  properties: IssueProperty[];
  total: number;
}

/** Response of PUT/DELETE /api/issues/{id}/properties/{propertyId}: the full post-mutation bag. */
export interface IssuePropertiesResponse {
  properties: IssuePropertyValues;
}
