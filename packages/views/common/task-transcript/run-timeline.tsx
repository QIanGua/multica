"use client";

import { cn } from "@multica/ui/lib/utils";
import { timelineTicks, type LaneSegment, type TraceLanes } from "./build-steps";
import { useT } from "../../i18n";

/**
 * Where a run's time went, on a real wall-clock axis.
 *
 * Two lanes, not one: model turns and tool calls never overlap in an agent
 * loop, so on a single row they can only occlude each other. Split, each lane
 * reads as its own comb and carries its own total — "was it the model or the
 * commands" is answerable without hovering anything. Two 15px lanes cost the
 * vertical space one 30px track did.
 *
 * Colour carries state, never taxonomy: the token set has one blue lightness
 * ramp and no categorical palette, so which lane a bar sits in says what kind
 * of work it was, and colour is spent on delivered (success) and failed
 * (destructive) instead.
 */

const SEGMENT_TONE: Record<LaneSegment["kind"], string> = {
  tool: "bg-chart-2",
  think: "bg-chart-3",
  report: "bg-success",
  error: "bg-destructive",
};

function formatTick(ms: number): string {
  const totalSeconds = Math.round(ms / 1000);
  if (totalSeconds < 60) return `${totalSeconds}s`;
  const minutes = Math.floor(totalSeconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  const rest = minutes % 60;
  return rest === 0 ? `${hours}h` : `${hours}h ${rest}m`;
}

export function formatLaneTotal(ms: number): string {
  const totalSeconds = Math.round(ms / 1000);
  if (totalSeconds < 60) return `${totalSeconds}s`;
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes < 60) return `${minutes}m ${String(seconds).padStart(2, "0")}s`;
  const hours = Math.floor(minutes / 60);
  return `${hours}h ${String(minutes % 60).padStart(2, "0")}m`;
}

function Lane({
  label,
  total,
  segments,
  totalMs,
  onSeek,
}: {
  label: string;
  total: string;
  segments: LaneSegment[];
  totalMs: number;
  onSeek: (ms: number) => void;
}) {
  return (
    <div className="flex h-[15px] items-center">
      <div className="flex w-[5.75rem] shrink-0 items-baseline gap-1.5 pr-2.5 text-micro text-muted-foreground">
        <span className="truncate">{label}</span>
        <span className="ml-auto shrink-0 tabular-nums text-faint-foreground">{total}</span>
      </div>
      <div className="relative h-full flex-1 rounded-sm bg-muted/55">
        {segments.map((segment) => (
          <button
            key={`${segment.kind}:${segment.startMs}`}
            type="button"
            aria-label={`${label} ${formatLaneTotal(segment.durationMs)}`}
            title={`${label} · ${formatLaneTotal(segment.durationMs)}`}
            onClick={() => onSeek(segment.startMs)}
            className={cn(
              "absolute inset-y-0 rounded-[3px] transition-opacity hover:opacity-80",
              SEGMENT_TONE[segment.kind],
            )}
            style={{
              left: `${(segment.startMs / totalMs) * 100}%`,
              // Sub-pixel bars would vanish; a visible minimum keeps a fast
              // call clickable without pretending it took longer than it did.
              width: `max(3px, ${(segment.durationMs / totalMs) * 100}%)`,
            }}
          />
        ))}
      </div>
    </div>
  );
}

export function RunTimeline({
  lanes,
  selectedOffsetMs,
  onSeek,
}: {
  lanes: TraceLanes;
  /** Offset of the selected step, drawn as a playhead across both lanes. */
  selectedOffsetMs?: number;
  onSeek: (ms: number) => void;
}) {
  const { t } = useT("agents");
  const ticks = timelineTicks(lanes.totalMs);

  return (
    <div className="shrink-0 border-b px-4 pb-2.5 pt-2">
      <div className="relative ml-[5.75rem] h-3">
        {ticks.map((tick, index) => (
          <span
            key={tick}
            className={cn(
              "absolute top-0 text-micro tabular-nums text-faint-foreground",
              index === 0 ? "" : index === ticks.length - 1 ? "-translate-x-full" : "-translate-x-1/2",
            )}
            style={{ left: `${(tick / lanes.totalMs) * 100}%` }}
          >
            {formatTick(tick)}
          </span>
        ))}
      </div>
      <div className="relative mt-1 space-y-1">
        <Lane
          label={t(($) => $.transcript.lane_model)}
          total={formatLaneTotal(lanes.modelMs)}
          segments={lanes.model}
          totalMs={lanes.totalMs}
          onSeek={onSeek}
        />
        <Lane
          label={t(($) => $.transcript.lane_tool)}
          total={formatLaneTotal(lanes.toolMs)}
          segments={lanes.tool}
          totalMs={lanes.totalMs}
          onSeek={onSeek}
        />
        {selectedOffsetMs !== undefined && (
          <div
            aria-hidden
            className="pointer-events-none absolute -top-0.5 bottom-[-2px] w-0.5 rounded-full bg-brand"
            style={{
              // The track starts after the lane labels, so the playhead walks
              // the remaining width, not the whole row.
              left: `calc(5.75rem + (100% - 5.75rem) * ${(
                Math.min(Math.max(selectedOffsetMs, 0), lanes.totalMs) / lanes.totalMs
              ).toFixed(5)})`,
            }}
          />
        )}
      </div>
    </div>
  );
}
