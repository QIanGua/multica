"use client";

import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
} from "react";
import { createPortal } from "react-dom";
import { autoUpdate } from "@floating-ui/dom";
import type { Editor } from "@tiptap/core";
import { Columns3, Plus, Rows3, Trash2 } from "lucide-react";
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuSeparator,
} from "@multica/ui/components/ui/context-menu";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../i18n";

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

interface TableContextMenuPosition {
  x: number;
  y: number;
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

function BorderInsertionHandle({
  label,
  style,
  orientation,
  active,
  onActiveChange,
  onAction,
}: {
  label: string;
  style: CSSProperties;
  orientation: "horizontal" | "vertical";
  active: boolean;
  onActiveChange: (active: boolean) => void;
  onAction: () => void;
}) {
  return (
    <button
      type="button"
      aria-label={label}
      className="group pointer-events-auto fixed cursor-pointer border-0 bg-transparent p-0 focus-visible:outline-none"
      style={style}
      onMouseDown={(event) => event.preventDefault()}
      onPointerEnter={() => onActiveChange(true)}
      onPointerLeave={() => onActiveChange(false)}
      onFocus={() => onActiveChange(true)}
      onBlur={() => onActiveChange(false)}
      onClick={onAction}
    >
      <span
        aria-hidden="true"
        className={cn(
          "pointer-events-none absolute flex size-6 items-center justify-center rounded-full bg-brand text-brand-foreground shadow-md transition-all",
          orientation === "vertical"
            ? "left-1/2 top-0 -translate-x-1/2 -translate-y-1/2"
            : "left-0 top-1/2 -translate-x-1/2 -translate-y-1/2",
          active ? "scale-100 opacity-100" : "scale-75 opacity-0",
        )}
      >
        <Plus className="size-4" />
      </span>
    </button>
  );
}

/** Border-hover insertion affordances around the table cell holding selection. */
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
  const [activeGuide, setActiveGuide] = useState<GuideKind | null>(null);
  const [contextMenuPosition, setContextMenuPosition] =
    useState<TableContextMenuPosition | null>(null);
  const overlayRef = useRef<HTMLDivElement>(null);

  const contextMenuAnchor = useMemo(
    () =>
      contextMenuPosition
        ? {
            getBoundingClientRect: () =>
              DOMRect.fromRect({
                x: contextMenuPosition.x,
                y: contextMenuPosition.y,
                width: 0,
                height: 0,
              }),
          }
        : undefined,
    [contextMenuPosition],
  );

  useEffect(() => {
    const onTransaction = () => {
      if (!editor.isInitialized) return;
      const nextElements = selectedTableElements(editor);
      const nextVisible = shouldShowTableMenu(editor) && nextElements !== null;
      setVisible(nextVisible);
      setActiveGuide(null);

      if (!nextVisible || !nextElements) {
        setElements(null);
        setGeometry(null);
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
    const editorDom = editor.view.dom;
    const onContextMenu = (event: MouseEvent) => {
      if (!editor.isEditable || editor.isDestroyed) return;
      const target = event.target;
      if (!(target instanceof Element)) return;
      const cell = target.closest("td, th");
      if (!(cell instanceof HTMLTableCellElement)) return;

      try {
        // Put the Tiptap selection inside the cell that was actually clicked.
        // Table commands operate on selection, and right-click does not move it
        // consistently across browsers.
        const cellContentStart = editor.view.posAtDOM(cell, 0);
        const selectionPosition = Math.min(
          cellContentStart + 1,
          editor.state.doc.content.size,
        );
        if (!editor.commands.setTextSelection(selectionPosition)) return;
      } catch {
        return;
      }

      event.preventDefault();
      event.stopPropagation();
      setActiveGuide(null);
      setContextMenuPosition({ x: event.clientX, y: event.clientY });
    };

    editorDom.addEventListener("contextmenu", onContextMenu);
    return () => {
      editorDom.removeEventListener("contextmenu", onContextMenu);
    };
  }, [editor]);

  useEffect(() => {
    const onBlur = () => {
      setTimeout(() => {
        if (editor.isDestroyed || editor.view.hasFocus()) return;
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
      setActiveGuide(null);
      command(editor.chain().focus()).run();
    },
    [editor],
  );

  if (
    ((!visible || !elements) && !contextMenuPosition) ||
    typeof document === "undefined"
  ) {
    return null;
  }

  const tableWidth = geometry ? geometry.tableRight - geometry.tableLeft : 0;
  const tableHeight = geometry ? geometry.tableBottom - geometry.tableTop : 0;

  return createPortal(
    <>
      {visible && elements && (
        <div
          ref={overlayRef}
          role="toolbar"
          aria-label={t(($) => $.table_menu.label)}
          className="pointer-events-none fixed inset-0 z-50"
        >
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

          {geometry &&
            boundaryIsVisible(
              geometry.cellLeft,
              geometry.tableLeft,
              geometry.tableRight,
            ) && (
              <BorderInsertionHandle
                label={t(($) => $.table_menu.add_column_left)}
                style={{
                  left: geometry.cellLeft - 6,
                  top: geometry.tableTop,
                  width: 12,
                  height: tableHeight,
                }}
                orientation="vertical"
                active={activeGuide === "column-before"}
                onActiveChange={(active) =>
                  setActiveGuide(active ? "column-before" : null)
                }
                onAction={() => run((chain) => chain.addColumnBefore())}
              />
            )}
          {geometry &&
            boundaryIsVisible(
              geometry.cellRight,
              geometry.tableLeft,
              geometry.tableRight,
            ) && (
              <BorderInsertionHandle
                label={t(($) => $.table_menu.add_column_right)}
                style={{
                  left: geometry.cellRight - 6,
                  top: geometry.tableTop,
                  width: 12,
                  height: tableHeight,
                }}
                orientation="vertical"
                active={activeGuide === "column-after"}
                onActiveChange={(active) =>
                  setActiveGuide(active ? "column-after" : null)
                }
                onAction={() => run((chain) => chain.addColumnAfter())}
              />
            )}
          {geometry &&
            boundaryIsVisible(
              geometry.rowTop,
              geometry.tableTop,
              geometry.tableBottom,
            ) && (
              <BorderInsertionHandle
                label={t(($) => $.table_menu.add_row_above)}
                style={{
                  left: geometry.tableLeft,
                  top: geometry.rowTop - 6,
                  width: tableWidth,
                  height: 12,
                }}
                orientation="horizontal"
                active={activeGuide === "row-before"}
                onActiveChange={(active) =>
                  setActiveGuide(active ? "row-before" : null)
                }
                onAction={() => run((chain) => chain.addRowBefore())}
              />
            )}
          {geometry &&
            boundaryIsVisible(
              geometry.rowBottom,
              geometry.tableTop,
              geometry.tableBottom,
            ) && (
              <BorderInsertionHandle
                label={t(($) => $.table_menu.add_row_below)}
                style={{
                  left: geometry.tableLeft,
                  top: geometry.rowBottom - 6,
                  width: tableWidth,
                  height: 12,
                }}
                orientation="horizontal"
                active={activeGuide === "row-after"}
                onActiveChange={(active) =>
                  setActiveGuide(active ? "row-after" : null)
                }
                onAction={() => run((chain) => chain.addRowAfter())}
              />
            )}
        </div>
      )}

      {contextMenuPosition && (
        <ContextMenu
          open
          onOpenChange={(open) => {
            if (!open) setContextMenuPosition(null);
          }}
        >
          <ContextMenuContent anchor={contextMenuAnchor}>
            <ContextMenuItem
              variant="destructive"
              onClick={() => {
                setContextMenuPosition(null);
                run((chain) => chain.deleteRow());
              }}
            >
              <Rows3 />
              {t(($) => $.table_menu.delete_row)}
            </ContextMenuItem>
            <ContextMenuItem
              variant="destructive"
              onClick={() => {
                setContextMenuPosition(null);
                run((chain) => chain.deleteColumn());
              }}
            >
              <Columns3 />
              {t(($) => $.table_menu.delete_column)}
            </ContextMenuItem>
            <ContextMenuSeparator />
            <ContextMenuItem
              variant="destructive"
              onClick={() => {
                setContextMenuPosition(null);
                run((chain) => chain.deleteTable());
              }}
            >
              <Trash2 />
              {t(($) => $.table_menu.delete_table)}
            </ContextMenuItem>
          </ContextMenuContent>
        </ContextMenu>
      )}
    </>,
    document.body,
  );
}

export { EditorTableMenu };
