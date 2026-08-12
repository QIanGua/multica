import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { Issue } from "@multica/core/types";
import { BoardCardContent } from "./board-card";

vi.mock("@tanstack/react-query", () => ({
  useQuery: () => ({ data: [] }),
}));

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "ws-1",
}));

vi.mock("@multica/core/properties", () => ({
  propertyListOptions: () => ({ queryKey: ["properties"] }),
}));

const viewState = vi.hoisted(() => ({
  cardProperties: {
    priority: false,
    description: false,
    assignee: true,
    startDate: false,
    dueDate: false,
    project: false,
    childProgress: false,
    labels: false,
  },
  cardPropertyIds: [],
}));

vi.mock("@multica/core/issues/stores/view-store-context", () => ({
  useViewStore: (selector: (state: typeof viewState) => unknown) => selector(viewState),
}));

vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: () => "Ada Lovelace" }),
}));

vi.mock("../../i18n", () => ({
  useT: () => ({ t: () => "Translated" }),
  useTimeAgo: () => () => "now",
}));

vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: ({
    actorType,
    actorId,
    profileLinkOwnsClick,
  }: {
    actorType: string;
    actorId: string;
    profileLinkOwnsClick?: boolean;
  }) => (
    <span
      data-testid="actor-avatar"
      data-actor-type={actorType}
      data-actor-id={actorId}
      data-profile-link-owns-click={String(profileLinkOwnsClick)}
    />
  ),
}));

vi.mock("./issue-agent-activity-indicator", () => ({
  IssueAgentActivityIndicator: () => null,
}));

const ISSUE: Issue = {
  id: "issue-1",
  workspace_id: "ws-1",
  number: 6082,
  identifier: "MUL-6082",
  title: "Fix Board avatar navigation",
  description: null,
  status: "todo",
  priority: "none",
  assignee_type: "member",
  assignee_id: "member-1",
  creator_type: "member",
  creator_id: "member-1",
  parent_issue_id: null,
  project_id: null,
  position: 1,
  stage: null,
  start_date: null,
  due_date: null,
  metadata: {},
  properties: {},
  labels: [],
  created_at: "2026-08-12T00:00:00Z",
  updated_at: "2026-08-12T00:00:00Z",
};

describe("BoardCardContent profile navigation", () => {
  it("lets the assignee avatar own its profile click", () => {
    render(<BoardCardContent issue={ISSUE} />);

    expect(screen.getByTestId("actor-avatar")).toHaveAttribute(
      "data-profile-link-owns-click",
      "true",
    );
  });
});
