import { describe, expect, it, vi } from "vitest";
import {
  act,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { Editor } from "@tiptap/core";

const autoUpdateMock = vi.hoisted(() =>
  vi.fn((_reference: unknown, _floating: unknown, update: () => void) => {
    update();
    return vi.fn();
  }),
);

vi.mock("@floating-ui/dom", () => ({
  autoUpdate: autoUpdateMock,
}));

const labels = {
  label: "Edit table",
  row_options: "Row options",
  column_options: "Column options",
  add_row_above: "Add row above",
  add_row_below: "Add row below",
  add_column_left: "Add column left",
  add_column_right: "Add column right",
};

vi.mock("../i18n", () => ({
  useT: () => ({
    t: (selector: (value: { table_menu: typeof labels }) => string) =>
      selector({ table_menu: labels }),
  }),
}));

import { EditorTableMenu } from "./table-menu";
import { createEditorExtensions } from "./extensions";

type EditorEvent = "transaction" | "blur";

function rect(
  left: number,
  top: number,
  width: number,
  height: number,
): DOMRect {
  return {
    x: left,
    y: top,
    left,
    top,
    width,
    height,
    right: left + width,
    bottom: top + height,
    toJSON: () => ({}),
  } as DOMRect;
}

function createEditorHarness() {
  let inTable = false;
  const listeners = new Map<EditorEvent, Set<() => void>>();
  const editorDom = document.createElement("div");
  const tableWrapper = document.createElement("div");
  const table = document.createElement("table");
  const row = document.createElement("tr");
  const cell = document.createElement("td");
  tableWrapper.className = "tableWrapper";
  row.append(cell);
  table.append(row);
  tableWrapper.append(table);
  editorDom.append(tableWrapper);

  cell.getBoundingClientRect = () => rect(120, 100, 180, 48);
  row.getBoundingClientRect = () => rect(100, 100, 360, 48);
  table.getBoundingClientRect = () => rect(100, 100, 360, 96);
  tableWrapper.getBoundingClientRect = () => rect(100, 100, 360, 96);

  const run = vi.fn(() => true);
  const commands = {
    focus: vi.fn(),
    addRowBefore: vi.fn(),
    addRowAfter: vi.fn(),
    addColumnBefore: vi.fn(),
    addColumnAfter: vi.fn(),
    run,
  };
  commands.focus.mockReturnValue(commands);
  commands.addRowBefore.mockReturnValue(commands);
  commands.addRowAfter.mockReturnValue(commands);
  commands.addColumnBefore.mockReturnValue(commands);
  commands.addColumnAfter.mockReturnValue(commands);

  const editor = {
    isDestroyed: false,
    isEditable: true,
    isInitialized: true,
    isActive: vi.fn((name: string) => name === "table" && inTable),
    chain: vi.fn(() => commands),
    on: vi.fn((event: EditorEvent, listener: () => void) => {
      const eventListeners = listeners.get(event) ?? new Set();
      eventListeners.add(listener);
      listeners.set(event, eventListeners);
    }),
    off: vi.fn((event: EditorEvent, listener: () => void) => {
      listeners.get(event)?.delete(listener);
    }),
    state: {
      selection: { from: 1, to: 1, empty: true },
    },
    view: {
      dom: editorDom,
      domAtPos: vi.fn(() => ({ node: cell, offset: 0 })),
      hasFocus: vi.fn(() => true),
    },
  } as unknown as Editor;

  return {
    cell,
    commands,
    editor,
    emit(event: EditorEvent) {
      act(() => {
        for (const listener of listeners.get(event) ?? []) listener();
      });
    },
    setInTable(next: boolean) {
      inTable = next;
    },
  };
}

async function renderInTable() {
  const harness = createEditorHarness();
  harness.setInTable(true);
  render(<EditorTableMenu editor={harness.editor} />);
  await screen.findByRole("toolbar", { name: "Edit table" });
  return harness;
}

function createProductionTableEditor(): Editor {
  const element = document.createElement("div");
  document.body.appendChild(element);
  const editor = new Editor({
    element,
    extensions: createEditorExtensions({
      placeholder: "",
      disableMentions: true,
      enableSlashCommands: false,
      onUploadFileRef: { current: undefined },
    }),
  });

  editor.commands.setContent(
    ["| A | B |", "| --- | --- |", "| C | D |"].join("\n"),
    { contentType: "markdown" },
  );

  let firstCellPosition: number | null = null;
  editor.state.doc.descendants((node, position) => {
    if (firstCellPosition !== null) return false;
    if (node.type.name !== "tableHeader" && node.type.name !== "tableCell") {
      return true;
    }
    firstCellPosition = position;
    return false;
  });
  if (firstCellPosition === null) throw new Error("Expected a parsed table");
  editor.commands.setTextSelection(firstCellPosition + 2);

  const wrapper = editor.view.dom.querySelector<HTMLElement>(".tableWrapper");
  const table = wrapper?.querySelector<HTMLTableElement>("table");
  const row = table?.querySelector<HTMLTableRowElement>("tr");
  const cell = row?.querySelector<HTMLTableCellElement>("th, td");
  if (!wrapper || !table || !row || !cell) {
    throw new Error("Expected rendered production table elements");
  }

  cell.getBoundingClientRect = () => rect(120, 100, 180, 48);
  row.getBoundingClientRect = () => rect(100, 100, 360, 48);
  table.getBoundingClientRect = () => rect(100, 100, 360, 96);
  wrapper.getBoundingClientRect = () => rect(100, 100, 360, 96);
  return editor;
}

describe("EditorTableMenu", () => {
  it("appears when the caret enters a table and hides when it leaves", async () => {
    const harness = createEditorHarness();
    render(<EditorTableMenu editor={harness.editor} />);

    expect(screen.queryByRole("toolbar", { name: "Edit table" })).toBeNull();

    harness.setInTable(true);
    harness.emit("transaction");
    expect(
      await screen.findByRole("toolbar", { name: "Edit table" }),
    ).toBeVisible();

    harness.setInTable(false);
    harness.emit("transaction");
    expect(screen.queryByRole("toolbar", { name: "Edit table" })).toBeNull();
  });

  it("tracks the selected cell so controls follow table scrolling", async () => {
    const harness = createEditorHarness();
    const callsBeforeEnteringTable = autoUpdateMock.mock.calls.length;
    render(<EditorTableMenu editor={harness.editor} />);

    harness.setInTable(true);
    harness.emit("transaction");
    await waitFor(() =>
      expect(autoUpdateMock.mock.calls.length).toBeGreaterThan(
        callsBeforeEnteringTable,
      ),
    );

    expect(autoUpdateMock.mock.calls.at(-1)?.[0]).toBe(harness.cell);
  });

  it.each([
    ["Add row above", "addRowBefore"],
    ["Add row below", "addRowAfter"],
    ["Add column left", "addColumnBefore"],
    ["Add column right", "addColumnAfter"],
  ] as const)("runs the spatial %s action", async (label, command) => {
    const harness = await renderInTable();

    fireEvent.click(screen.getByRole("button", { name: label }));

    expect(harness.commands.focus).toHaveBeenCalledTimes(1);
    expect(harness.commands[command]).toHaveBeenCalledTimes(1);
    expect(harness.commands.run).toHaveBeenCalledTimes(1);
  });

  it.each([
    ["Add column left", "column-before", "119px", "100px", "96px"],
    ["Add row below", "row-after", "100px", "147px", "360px"],
  ] as const)(
    "shows a positioned insertion guide while hovering %s",
    async (label, guide, left, top, extent) => {
      await renderInTable();
      const handle = screen.getByRole("button", { name: label });

      fireEvent.pointerEnter(handle);
      const indicator = document.querySelector<HTMLElement>(
        `[data-table-guide="${guide}"]`,
      );

      expect(indicator).not.toBeNull();
      expect(indicator).toHaveStyle({ left, top });
      expect(indicator).toHaveStyle(
        guide.startsWith("column") ? { height: extent } : { width: extent },
      );

      fireEvent.pointerLeave(handle);
      expect(
        document.querySelector(`[data-table-guide="${guide}"]`),
      ).toBeNull();
    },
  );

  it.each([
    ["Add column left", "114px", "100px", "12px", "96px"],
    ["Add row below", "100px", "142px", "360px", "12px"],
  ] as const)(
    "uses the %s border as the complete hover and click target",
    async (label, left, top, width, height) => {
      await renderInTable();

      expect(screen.getByRole("button", { name: label })).toHaveStyle({
        left,
        top,
        width,
        height,
      });
    },
  );

  it("does not render persistent table, row, or column menu buttons", async () => {
    await renderInTable();

    expect(screen.queryByRole("button", { name: "Edit table" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Row options" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Column options" })).toBeNull();
    expect(screen.getAllByRole("button")).toHaveLength(4);
  });

  it.each([
    ["Add row below", "tr", 3],
    ["Add column right", "tr:first-child > th, tr:first-child > td", 3],
  ] as const)(
    "changes the real Tiptap document when clicking %s",
    async (label, selector, expectedCount) => {
      const editor = createProductionTableEditor();
      try {
        render(<EditorTableMenu editor={editor} />);
        await screen.findByRole("toolbar", { name: "Edit table" });

        fireEvent.click(screen.getByRole("button", { name: label }));

        expect(editor.view.dom.querySelectorAll(selector)).toHaveLength(
          expectedCount,
        );
      } finally {
        editor.destroy();
      }
    },
  );
});
