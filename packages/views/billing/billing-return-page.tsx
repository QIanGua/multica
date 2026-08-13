"use client";

import { useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { AlertCircle, Loader2, RefreshCw } from "lucide-react";
import { useAuthStore } from "@multica/core/auth";
import { paths } from "@multica/core/paths";
import { workspaceListOptions } from "@multica/core/workspace/queries";
import { Button } from "@multica/ui/components/ui/button";
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
 * Three things this page deliberately does NOT do:
 *
 *   - It never trusts a redirect target from the query string. Only
 *     `workspace_id` and `result` are read, and the destination is rebuilt from
 *     `paths`, so a crafted return link cannot become an open redirect.
 *   - It does not report success on its own. `result=success` means Stripe
 *     finished, not that the subscription is recorded; the destination page is
 *     what waits for the webhook. This page only forwards the marker.
 *   - It does not silently drop the user somewhere else when something is
 *     wrong. Paying and then landing on an unexplained page is worse than a
 *     short message, so the failure states below say what happened.
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

  const {
    data: workspaces,
    isPending: isWorkspacesPending,
    isError: isWorkspacesError,
    refetch: refetchWorkspaces,
  } = useQuery({
    ...workspaceListOptions(),
    enabled: !!user,
  });

  const match =
    workspaceIdParam && workspaces
      ? workspaces.find((ws) => ws.id === workspaceIdParam)
      : undefined;
  // Only after the list has actually arrived can "not found" mean anything.
  const isUnresolvable = !!workspaces && !match;

  useEffect(() => {
    if (isAuthLoading) return;
    // Checkout can take long enough for a session to expire, so an
    // unauthenticated return is a normal case rather than an error. Carry this
    // exact URL through login so the user resumes the return instead of landing
    // on their default workspace with no idea whether the payment took.
    // sanitizeNextUrl in the login page only accepts relative paths, so this
    // cannot be turned into an off-site redirect.
    if (!user) {
      const returnTo = `${navigation.pathname}?${navigation.searchParams.toString()}`;
      navigation.replace(`${paths.login()}?next=${encodeURIComponent(returnTo)}`);
      return;
    }
    if (isWorkspacesPending || isWorkspacesError || !workspaces) return;
    if (!match) return;

    const destination = `${paths.workspace(match.slug).settings()}?tab=billing${
      result ? `&result=${result}` : ""
    }`;
    // replace(), not push(): the Stripe hop should not sit in history where a
    // back navigation would re-run this redirect.
    navigation.replace(destination);
  }, [
    isAuthLoading,
    isWorkspacesError,
    isWorkspacesPending,
    match,
    navigation,
    result,
    user,
    workspaces,
  ]);

  // The workspace list is what turns an ID into a slug, so without it there is
  // no destination to send anyone to. Retrying beats an endless spinner.
  if (isWorkspacesError) {
    return (
      <ReturnMessage
        title={t(($) => $.return_page.load_failed_title)}
        description={t(($) => $.return_page.load_failed_description)}
        action={
          <Button
            className="h-11"
            variant="outline"
            onClick={() => void refetchWorkspaces()}
          >
            <RefreshCw />
            {t(($) => $.return_page.retry)}
          </Button>
        }
      />
    );
  }

  // Deleted workspace, revoked membership, or a hand-edited link. The billing
  // change itself is already recorded server-side, so say that plainly instead
  // of dropping the user on an unrelated workspace with no explanation.
  if (isUnresolvable) {
    return (
      <ReturnMessage
        title={t(($) => $.return_page.workspace_missing_title)}
        description={t(($) => $.return_page.workspace_missing_description)}
        action={
          <Button className="h-11" onClick={() => navigation.replace(paths.root())}>
            {t(($) => $.return_page.go_to_app)}
          </Button>
        }
      />
    );
  }

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

function ReturnMessage({
  title,
  description,
  action,
}: {
  title: string;
  description: string;
  action: React.ReactNode;
}) {
  return (
    <div className="flex min-h-dvh items-center justify-center bg-background p-6">
      <div
        className="flex max-w-[60ch] flex-col items-start gap-3"
        role="alert"
        aria-live="polite"
      >
        <div className="flex items-center gap-2 text-body font-medium">
          <AlertCircle className="size-4 text-destructive" />
          <span>{title}</span>
        </div>
        <p className="text-caption leading-5 text-muted-foreground">
          {description}
        </p>
        {action}
      </div>
    </div>
  );
}
