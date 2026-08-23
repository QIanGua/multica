"use client";

import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type CSSProperties,
  type ReactNode,
} from "react";
import { createPortal } from "react-dom";
import { autoUpdate } from "@floating-ui/dom";
import type { Editor } from "@tiptap/core";
import {
  BetweenHorizontalEnd,
  BetweenHorizontalStart,
  BetweenVerticalEnd,
  BetweenVerticalStart,
  Columns3,
  GripHorizontal,
  GripVertical,
  Plus,
  Rows3,
  Table2,
  Trash2,
} from "lucide-react";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@multica/ui/components/ui/dropdown-menu";
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from "@multica/ui/components/ui/tooltip";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../i18n";

type MenuKind = "table" | "row" | "column";
type GuideKind = "row-before" | "row-after" | "column-before" | "column-after";

interface TableElements {
  cell: HTMLTableCellElement;
  row: HTMLTableRowElement;
  table: HTMLTableElement;
  wrapper: HTMLElement;
}

interface TableGeometry {
  tableLeft: number;
  tableRight: number;
  tableTop: number;
  tableBottom: number;
  cellLeft: number;
  cellRight: number;
  rowTop: number;
  rowBottom: number;
}

function shouldShowTableMenu(editor: Editor): boolean {
  return editor.isEditable && !editor.isDestroyed && editor.isActive("table");
}

function selectedTableElements(editor: Editor): TableElements | null {
  try {
    const { from } = editor.state.selection;
    const { node } = editor.view.domAtPos(from);
    const element = node instanceof Element ? node : node.parentElement;
    const cell = element?.closest("td, th");
    const row = cell?.closest("tr");
    const table = row?.closest("table");
    const wrapper = table?.closest(".tableWrapper") ?? table?.parentElement;

    if (
      !(cell instanceof HTMLTableCellElement) ||
      !(row instanceof HTMLTableRowElement) ||
      !(table instanceof HTMLTableElement) ||
      !(wrapper instanceof HTMLElement)
    ) {
      return null;
    }

    return { cell, row, table, wrapper };
  } catch {
    return null;
  }
}

function measureTable(elements: TableElements): TableGeometry | null {
  const cell = elements.cell.getBoundingClientRect();
  const row = elements.row.getBoundingClientRect();
  const table = elements.table.getBoundingClientRect();
  const wrapper = elements.wrapper.getBoundingClientRect();
  const tableLeft = Math.max(table.left, wrapper.left);
  const tableRight = Math.min(table.right, wrapper.right);
  const tableTop = Math.max(table.top, wrapper.top);
  const tableBottom = Math.min(table.bottom, wrapper.bottom);

  if (tableRight <= tableLeft || tableBottom <= tableTop) return null;

  return {
    tableLeft,
    tableRight,
    tableTop,
    tableBottom,
    cellLeft: cell.left,
    cellRight: cell.right,
    rowTop: row.top,
    rowBottom: row.bottom,
  };
}

function sameGeometry(
  current: TableGeometry | null,
  next: TableGeometry | null,
): boolean {
  if (current === next) return true;
  if (!current || !next) return false;
  return (Object.keys(current) as Array<keyof TableGeometry>).every(
    (key) => current[key] === next[key],
  );
}

function boundaryIsVisible(value: number, start: number, end: number): boolean {
  return value >= start - 1 && value <= end + 1;
}

function SpatialMenu({
  open,
  onOpenChange,
  label,
  style,
  side,
  align = "center",
  className,
  onPointerEnter,
  onPointerLeave,
  children,
  trigger,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  label: string;
  style: CSSProperties;
  side: "top" | "bottom" | "left" | "right";
  align?: "start" | "center" | "end";
  className?: string;
  onPointerEnter?: () => void;
  onPointerLeave?: () => void;
  children: ReactNode;
  trigger: ReactNode;
}) {
  return (
    <DropdownMenu open={open} onOpenChange={onOpenChange}>
      <DropdownMenuTrigger
        render={
          <button
            type="button"
            aria-label={label}
            title={label}
            className={cn(
              "pointer-events-auto fixed flex items-center justify-center rounded-md border border-border bg-surface-raised text-muted-foreground shadow-sm transition-colors hover:bg-accent hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
              className,
            )}
            style={style}
            onMouseDown={(event) => event.preventDefault()}
            onPointerEnter={onPointerEnter}
            onPointerLeave={onPointerLeave}
          />
        }
      >
        {trigger}
      </DropdownMenuTrigger>
      <DropdownMenuContent side={side} align={align} sideOffset={6}>
        {children}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

function InsertionHandle({
  label,
  style,
  tooltipSide,
  active,
  onActiveChange,
  onAction,
}: {
  label: string;
  style: CSSProperties;
  tooltipSide: "top" | "left";
  active: boolean;
  onActiveChange: (active: boolean) => void;
  onAction: () => void;
}) {
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <button
            type="button"
            aria-label={label}
            className="group pointer-events-auto fixed flex size-8 items-center justify-center rounded-full focus-visible:outline-none"
            style={style}
            onMouseDown={(event) => event.preventDefault()}
            onPointerEnter={() => onActiveChange(true)}
            onPointerLeave={() => onActiveChange(false)}
            onFocus={() => onActiveChange(true)}
            onBlur={() => onActiveChange(false)}
            onClick={onAction}
          />
        }
      >
        <span
          className={cn(
            "flex size-6 items-center justify-center rounded-full bg-brand text-brand-foreground shadow-md transition-all group-hover:scale-100 group-hover:opacity-100 group-focus-visible:scale-100 group-focus-visible:opacity-100",
            active ? "scale-100 opacity-100" : "scale-75 opacity-0",
          )}
        >
          <Plus className="size-4" />
        </span>
      </TooltipTrigger>
      <TooltipContent side={tooltipSide} sideOffset={8}>
        {label}
      </TooltipContent>
    </Tooltip>
  );
}

/** Feishu-style spatial controls around the table cell holding the selection. */
function EditorTableMenu({ editor }: { editor: Editor }) {
  const { t } = useT("editor");
  const initialElements = selectedTableElements(editor);
  const [visible, setVisible] = useState(
    () => shouldShowTableMenu(editor) && initialElements !== null,
  );
  const [elements, setElements] = useState<TableElements | null>(
    initialElements,
  );
  const [geometry, setGeometry] = useState<TableGeometry | null>(null);
  const [openMenu, setOpenMenu] = useState<MenuKind | null>(null);
  const [hoveredMenu, setHoveredMenu] = useState<MenuKind | null>(null);
  const [activeGuide, setActiveGuide] = useState<GuideKind | null>(null);
  const overlayRef = useRef<HTMLDivElement>(null);
  const openMenuRef = useRef<MenuKind | null>(null);

  useEffect(() => {
    openMenuRef.current = openMenu;
  }, [openMenu]);

  useEffect(() => {
    const onTransaction = () => {
      if (!editor.isInitialized) return;
      const nextElements = selectedTableElements(editor);
      const nextVisible = shouldShowTableMenu(editor) && nextElements !== null;
      setVisible(nextVisible);

      if (!nextVisible || !nextElements) {
        setElements(null);
        setGeometry(null);
        setOpenMenu(null);
        setActiveGuide(null);
        return;
      }

      setElements((current) =>
        current?.cell === nextElements.cell ? current : nextElements,
      );
    };

    editor.on("transaction", onTransaction);
    return () => {
      editor.off("transaction", onTransaction);
    };
  }, [editor]);

  useEffect(() => {
    const overlay = overlayRef.current;
    if (!visible || !elements || !overlay) return;

    const updateGeometry = () => {
      const next = measureTable(elements);
      setGeometry((current) => (sameGeometry(current, next) ? current : next));
    };

    updateGeometry();
    return autoUpdate(elements.cell, overlay, updateGeometry);
  }, [elements, visible]);

  useEffect(() => {
    const onBlur = () => {
      setTimeout(() => {
        if (editor.isDestroyed || editor.view.hasFocus()) return;
        if (openMenuRef.current) return;
        if (overlayRef.current?.contains(document.activeElement)) return;
        setVisible(false);
      }, 0);
    };

    editor.on("blur", onBlur);
    return () => {
      editor.off("blur", onBlur);
    };
  }, [editor]);

  useEffect(() => {
    if (!visible) return;
    const onMouseDown = (event: MouseEvent) => {
      const target = event.target;
      if (!(target instanceof Node)) return;
      if (editor.view.dom.contains(target)) return;
      if (overlayRef.current?.contains(target)) return;
      if (
        target instanceof Element &&
        target.closest('[data-slot="dropdown-menu-content"]')
      ) {
        return;
      }
      setVisible(false);
    };

    document.addEventListener("mousedown", onMouseDown);
    return () => document.removeEventListener("mousedown", onMouseDown);
  }, [editor, visible]);

  const run = useCallback(
    (
      command: (
        chain: ReturnType<Editor["chain"]>,
      ) => ReturnType<Editor["chain"]>,
    ) => {
      setOpenMenu(null);
      setActiveGuide(null);
      command(editor.chain().focus()).run();
    },
    [editor],
  );

  if (!visible || !elements || typeof document === "undefined") return null;

  const activeSelection = openMenu ?? hoveredMenu;
  const tableWidth = geometry ? geometry.tableRight - geometry.tableLeft : 0;
  const tableHeight = geometry ? geometry.tableBottom - geometry.tableTop : 0;
  const selectedColumnLeft = geometry
    ? Math.max(geometry.cellLeft, geometry.tableLeft)
    : 0;
  const selectedColumnRight = geometry
    ? Math.min(geometry.cellRight, geometry.tableRight)
    : 0;
  const selectedRowTop = geometry
    ? Math.max(geometry.rowTop, geometry.tableTop)
    : 0;
  const selectedRowBottom = geometry
    ? Math.min(geometry.rowBottom, geometry.tableBottom)
    : 0;

  return createPortal(
    <div
      ref={overlayRef}
      role="toolbar"
      aria-label={t(($) => $.table_menu.label)}
      className="pointer-events-none fixed inset-0 z-50"
    >
      {geometry && activeSelection === "column" && (
        <div
          aria-hidden="true"
          className="fixed bg-brand/10 ring-1 ring-inset ring-brand/40"
          style={{
            left: selectedColumnLeft,
            top: geometry.tableTop,
            width: Math.max(0, selectedColumnRight - selectedColumnLeft),
            height: tableHeight,
          }}
        />
      )}
      {geometry && activeSelection === "row" && (
        <div
          aria-hidden="true"
          className="fixed bg-brand/10 ring-1 ring-inset ring-brand/40"
          style={{
            left: geometry.tableLeft,
            top: selectedRowTop,
            width: tableWidth,
            height: Math.max(0, selectedRowBottom - selectedRowTop),
          }}
        />
      )}

      {geometry && activeGuide?.startsWith("column") && (
        <div
          aria-hidden="true"
          data-table-guide={activeGuide}
          className="fixed w-0.5 bg-brand shadow-sm"
          style={{
            left:
              (activeGuide === "column-before"
                ? geometry.cellLeft
                : geometry.cellRight) - 1,
            top: geometry.tableTop,
            height: tableHeight,
          }}
        />
      )}
      {geometry && activeGuide?.startsWith("row") && (
        <div
          aria-hidden="true"
          data-table-guide={activeGuide}
          className="fixed h-0.5 bg-brand shadow-sm"
          style={{
            left: geometry.tableLeft,
            top:
              (activeGuide === "row-before"
                ? geometry.rowTop
                : geometry.rowBottom) - 1,
            width: tableWidth,
          }}
        />
      )}

      {geometry && (
        <TooltipProvider delay={200}>
          {boundaryIsVisible(
            geometry.cellLeft,
            geometry.tableLeft,
            geometry.tableRight,
          ) && (
            <InsertionHandle
              label={t(($) => $.table_menu.add_column_left)}
              style={{
                left: geometry.cellLeft - 16,
                top: Math.max(8, geometry.tableTop - 30),
              }}
              tooltipSide="top"
              active={activeGuide === "column-before"}
              onActiveChange={(active) =>
                setActiveGuide(active ? "column-before" : null)
              }
              onAction={() => run((chain) => chain.addColumnBefore())}
            />
          )}
          {boundaryIsVisible(
            geometry.cellRight,
            geometry.tableLeft,
            geometry.tableRight,
          ) && (
            <InsertionHandle
              label={t(($) => $.table_menu.add_column_right)}
              style={{
                left: geometry.cellRight - 16,
                top: Math.max(8, geometry.tableTop - 30),
              }}
              tooltipSide="top"
              active={activeGuide === "column-after"}
              onActiveChange={(active) =>
                setActiveGuide(active ? "column-after" : null)
              }
              onAction={() => run((chain) => chain.addColumnAfter())}
            />
          )}
          {boundaryIsVisible(
            geometry.rowTop,
            geometry.tableTop,
            geometry.tableBottom,
          ) && (
            <InsertionHandle
              label={t(($) => $.table_menu.add_row_above)}
              style={{
                left: Math.max(8, geometry.tableLeft - 30),
                top: geometry.rowTop - 16,
              }}
              tooltipSide="left"
              active={activeGuide === "row-before"}
              onActiveChange={(active) =>
                setActiveGuide(active ? "row-before" : null)
              }
              onAction={() => run((chain) => chain.addRowBefore())}
            />
          )}
          {boundaryIsVisible(
            geometry.rowBottom,
            geometry.tableTop,
            geometry.tableBottom,
          ) && (
            <InsertionHandle
              label={t(($) => $.table_menu.add_row_below)}
              style={{
                left: Math.max(8, geometry.tableLeft - 30),
                top: geometry.rowBottom - 16,
              }}
              tooltipSide="left"
              active={activeGuide === "row-after"}
              onActiveChange={(active) =>
                setActiveGuide(active ? "row-after" : null)
              }
              onAction={() => run((chain) => chain.addRowAfter())}
            />
          )}

          <SpatialMenu
            open={openMenu === "table"}
            onOpenChange={(open) => setOpenMenu(open ? "table" : null)}
            label={t(($) => $.table_menu.label)}
            style={{
              left: Math.max(8, geometry.tableLeft - 46),
              top: Math.max(8, geometry.tableTop - 30),
            }}
            side="bottom"
            align="start"
            className="h-7 gap-0.5 px-1.5"
            trigger={
              <>
                <Table2 className="size-4 text-brand" />
                <GripVertical className="size-3" />
              </>
            }
          >
            <DropdownMenuItem
              onClick={() => run((chain) => chain.addRowBefore())}
            >
              <BetweenHorizontalStart />
              {t(($) => $.table_menu.add_row_above)}
            </DropdownMenuItem>
            <DropdownMenuItem
              onClick={() => run((chain) => chain.addRowAfter())}
            >
              <BetweenHorizontalEnd />
              {t(($) => $.table_menu.add_row_below)}
            </DropdownMenuItem>
            <DropdownMenuItem
              onClick={() => run((chain) => chain.addColumnBefore())}
            >
              <BetweenVerticalStart />
              {t(($) => $.table_menu.add_column_left)}
            </DropdownMenuItem>
            <DropdownMenuItem
              onClick={() => run((chain) => chain.addColumnAfter())}
            >
              <BetweenVerticalEnd />
              {t(($) => $.table_menu.add_column_right)}
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem
              variant="destructive"
              onClick={() => run((chain) => chain.deleteTable())}
            >
              <Trash2 />
              {t(($) => $.table_menu.delete_table)}
            </DropdownMenuItem>
          </SpatialMenu>

          <SpatialMenu
            open={openMenu === "column"}
            onOpenChange={(open) => setOpenMenu(open ? "column" : null)}
            label={t(($) => $.table_menu.column_options)}
            style={{
              left:
                Math.max(
                  geometry.tableLeft + 14,
                  Math.min(
                    geometry.tableRight - 14,
                    (geometry.cellLeft + geometry.cellRight) / 2,
                  ),
                ) - 14,
              top: Math.max(8, geometry.tableTop - 26),
            }}
            side="top"
            className="h-5 w-7"
            onPointerEnter={() => setHoveredMenu("column")}
            onPointerLeave={() => setHoveredMenu(null)}
            trigger={<GripHorizontal className="size-4" />}
          >
            <DropdownMenuItem
              onClick={() => run((chain) => chain.addColumnBefore())}
            >
              <BetweenVerticalStart />
              {t(($) => $.table_menu.add_column_left)}
            </DropdownMenuItem>
            <DropdownMenuItem
              onClick={() => run((chain) => chain.addColumnAfter())}
            >
              <BetweenVerticalEnd />
              {t(($) => $.table_menu.add_column_right)}
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem
              variant="destructive"
              onClick={() => run((chain) => chain.deleteColumn())}
            >
              <Columns3 />
              {t(($) => $.table_menu.delete_column)}
            </DropdownMenuItem>
          </SpatialMenu>

          <SpatialMenu
            open={openMenu === "row"}
            onOpenChange={(open) => setOpenMenu(open ? "row" : null)}
            label={t(($) => $.table_menu.row_options)}
            style={{
              left: Math.max(8, geometry.tableLeft - 26),
              top:
                Math.max(
                  geometry.tableTop + 14,
                  Math.min(
                    geometry.tableBottom - 14,
                    (geometry.rowTop + geometry.rowBottom) / 2,
                  ),
                ) - 14,
            }}
            side="left"
            className="h-7 w-5"
            onPointerEnter={() => setHoveredMenu("row")}
            onPointerLeave={() => setHoveredMenu(null)}
            trigger={<GripVertical className="size-4" />}
          >
            <DropdownMenuItem
              onClick={() => run((chain) => chain.addRowBefore())}
            >
              <BetweenHorizontalStart />
              {t(($) => $.table_menu.add_row_above)}
            </DropdownMenuItem>
            <DropdownMenuItem
              onClick={() => run((chain) => chain.addRowAfter())}
            >
              <BetweenHorizontalEnd />
              {t(($) => $.table_menu.add_row_below)}
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem
              variant="destructive"
              onClick={() => run((chain) => chain.deleteRow())}
            >
              <Rows3 />
              {t(($) => $.table_menu.delete_row)}
            </DropdownMenuItem>
          </SpatialMenu>
        </TooltipProvider>
      )}
    </div>,
    document.body,
  );
}

export { EditorTableMenu };
