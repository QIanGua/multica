"use client";

import { useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { Loader2 } from "lucide-react";
import { useAuthStore } from "@multica/core/auth";
import { paths } from "@multica/core/paths";
import { workspaceListOptions } from "@multica/core/workspace/queries";
import { useNavigation } from "../navigation";
import { useT } from "../i18n";

/**
 * Landing page for Stripe Checkout and Billing Portal returns.
 *
 * Stripe only knows the return URLs a deployment configured, and a deployment
 * has exactly one set of them — so they cannot contain a workspace slug. Cloud
 * therefore appends the workspace it already authorized (`workspace_id`) plus
 * `result`, and this page turns that into the slug-based Settings URL the user
 * actually needs. Hardcoding one workspace's slug in the Stripe configuration
 * would strand every other workspace.
 *
 * Two things this page deliberately does NOT do:
 *
 *   - It never trusts a redirect target from the query string. Only
 *     `workspace_id` and `result` are read, and the destination is rebuilt from
 *     `paths`, so a crafted return link cannot become an open redirect.
 *   - It does not report success on its own. `result=success` means Stripe
 *     finished, not that the subscription is recorded; the destination page is
 *     what waits for the webhook. This page only forwards the marker.
 */

const RESULT_VALUES = new Set(["success", "cancel", "portal"]);

export function BillingReturnPage() {
  const { t } = useT("billing");
  const navigation = useNavigation();
  const user = useAuthStore((s) => s.user);
  const isAuthLoading = useAuthStore((s) => s.isLoading);

  const workspaceIdParam = navigation.searchParams.get("workspace_id");
  const resultParam = navigation.searchParams.get("result");
  const result = resultParam && RESULT_VALUES.has(resultParam) ? resultParam : null;

  const { data: workspaces, isPending: isWorkspacesPending } = useQuery({
    ...workspaceListOptions(),
    enabled: !!user,
  });

  useEffect(() => {
    if (isAuthLoading) return;
    // Stripe sends the browser here directly, so an expired session is normal.
    // Send the user to login rather than rendering a broken page; they can
    // reopen Billing afterwards and the subscription state is already correct
    // server-side.
    if (!user) {
      navigation.replace(paths.login());
      return;
    }
    if (isWorkspacesPending || !workspaces) return;

    const match = workspaceIdParam
      ? workspaces.find((ws) => ws.id === workspaceIdParam)
      : undefined;

    // No match means the workspace was deleted, or this user is no longer a
    // member, or the link was hand-edited. There is nothing safe to show for a
    // workspace we cannot name, so fall back to the app root, which resolves to
    // the user's default workspace.
    if (!match) {
      navigation.replace(paths.root());
      return;
    }

    const destination = `${paths.workspace(match.slug).settings()}?tab=billing${
      result ? `&result=${result}` : ""
    }`;
    // replace(), not push(): the Stripe hop should not sit in history where a
    // back navigation would re-run this redirect.
    navigation.replace(destination);
  }, [
    isAuthLoading,
    isWorkspacesPending,
    navigation,
    result,
    user,
    workspaceIdParam,
    workspaces,
  ]);

  return (
    <div
      className="flex min-h-dvh items-center justify-center bg-background p-6"
      role="status"
      aria-live="polite"
    >
      <div className="flex items-center gap-3 text-body text-muted-foreground">
        <Loader2 className="size-4 animate-spin motion-reduce:animate-none" />
        <span>{t(($) => $.return_page.redirecting)}</span>
      </div>
    </div>
  );
}
