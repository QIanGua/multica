"use client";

import { useEffect, useState } from "react";
import { Loader2 } from "lucide-react";
import type { RemoteMCPDiscoveryResponse, RemoteMCPTool } from "@multica/core/types";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import { Checkbox } from "@multica/ui/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { useT } from "../../i18n";

function ToolGroup({
  label,
  tools,
  checked,
  disabled,
  onToggle,
  writeBadge,
}: {
  label: string;
  tools: RemoteMCPTool[];
  checked: string[];
  disabled: boolean;
  onToggle: (name: string) => void;
  writeBadge?: string;
}) {
  return (
    <div>
      <div className="text-caption font-medium text-muted-foreground">{label}</div>
      <div className="mt-1.5 space-y-0.5">
        {tools.map((tool) => (
          <label
            key={tool.name}
            className="flex cursor-pointer items-start gap-2.5 rounded-lg px-2 py-1.5 hover:bg-muted/50"
          >
            <Checkbox
              className="mt-0.5"
              checked={checked.includes(tool.name)}
              disabled={disabled}
              onCheckedChange={() => onToggle(tool.name)}
            />
            <span className="min-w-0">
              <span className="flex flex-wrap items-center gap-2">
                <span className="font-mono text-body">{tool.name}</span>
                {writeBadge ? <Badge variant="destructive">{writeBadge}</Badge> : null}
              </span>
              {tool.description ? (
                <span className="mt-0.5 block text-caption leading-5 text-muted-foreground">{tool.description}</span>
              ) : null}
            </span>
          </label>
        ))}
      </div>
    </div>
  );
}

/**
 * Explicit tool-access review for a remote MCP discovery. All tools start
 * checked; approving pins the checked tools' schemas. The parent performs
 * the approve mutation and closes the dialog on success.
 */
export function PluginToolReviewDialog({
  open,
  onOpenChange,
  pluginName,
  endpointDomain,
  discovery,
  onApprove,
  approving = false,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  pluginName: string;
  endpointDomain?: string;
  discovery: RemoteMCPDiscoveryResponse;
  onApprove: (tools: string[]) => void;
  approving?: boolean;
}) {
  const { t } = useT("settings");
  const [checked, setChecked] = useState<string[]>(() => discovery.discovered_tools.map((tool) => tool.name));

  // Reset the selection whenever the dialog (re)opens or a fresh discovery
  // replaces the current one — every review starts from "all checked".
  useEffect(() => {
    if (open) setChecked(discovery.discovered_tools.map((tool) => tool.name));
  }, [open, discovery]);

  const toggleTool = (name: string) => {
    setChecked((current) => current.includes(name)
      ? current.filter((tool) => tool !== name)
      : [...current, name]);
  };

  const readTools = discovery.discovered_tools.filter((tool) => tool.risk !== "write");
  const writeTools = discovery.discovered_tools.filter((tool) => tool.risk === "write");

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{t(($) => $.plugins.review_dialog.title, { name: pluginName })}</DialogTitle>
          <DialogDescription className="flex flex-wrap items-center gap-2 text-caption">
            <Badge variant="secondary" className="bg-success/10 text-success">
              {t(($) => $.plugins.remote_mcp.connected)}
            </Badge>
            {endpointDomain ? <span className="font-mono">{endpointDomain}</span> : null}
            <span>
              {t(($) => $.plugins.review_dialog.discovered_count, { count: discovery.discovered_tools.length })}
            </span>
          </DialogDescription>
        </DialogHeader>

        <div className="max-h-80 space-y-4 overflow-y-auto">
          {discovery.discovered_tools.length === 0 ? (
            <p className="text-caption text-muted-foreground">{t(($) => $.plugins.review_dialog.no_tools)}</p>
          ) : (
            <>
              {readTools.length > 0 ? (
                <ToolGroup
                  label={t(($) => $.plugins.review_dialog.read_group, { count: readTools.length })}
                  tools={readTools}
                  checked={checked}
                  disabled={approving}
                  onToggle={toggleTool}
                />
              ) : null}
              {writeTools.length > 0 ? (
                <ToolGroup
                  label={t(($) => $.plugins.review_dialog.write_group, { count: writeTools.length })}
                  tools={writeTools}
                  checked={checked}
                  disabled={approving}
                  onToggle={toggleTool}
                  writeBadge={t(($) => $.plugins.review_dialog.write)}
                />
              ) : null}
            </>
          )}
        </div>

        <p className="text-caption leading-5 text-muted-foreground">
          {t(($) => $.plugins.review_dialog.schema_pin_note)}
        </p>

        <DialogFooter>
          <Button variant="ghost" disabled={approving} onClick={() => setChecked([])}>
            {t(($) => $.plugins.review_dialog.uncheck_all)}
          </Button>
          <Button
            disabled={checked.length === 0 || approving}
            onClick={() => onApprove(checked)}
          >
            {approving ? <Loader2 className="animate-spin" /> : null}
            {t(($) => $.plugins.review_dialog.approve_count, { count: checked.length })}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
