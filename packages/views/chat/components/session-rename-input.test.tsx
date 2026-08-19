import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import enChat from "../../locales/en/chat.json";
import { SessionRenameInput } from "./session-rename-input";

const TEST_RESOURCES = { en: { chat: enChat } };
const RENAME_LABEL = enChat.session_history.row_rename_aria;
const onSubmit = vi.fn();
const onCancel = vi.fn();
const onCompositionChange = vi.fn();

function renderInput(): HTMLInputElement {
  render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <SessionRenameInput
        initialValue="Original title"
        onSubmit={onSubmit}
        onCancel={onCancel}
        onCompositionChange={onCompositionChange}
      />
    </I18nProvider>,
  );
  return screen.getByRole("textbox", { name: RENAME_LABEL });
}

describe("SessionRenameInput", () => {
  beforeEach(() => {
    onSubmit.mockReset();
    onCancel.mockReset();
    onCompositionChange.mockReset();
  });

  it.each([
    ["standard composition signal", { isComposing: true, keyCode: 13 }],
    ["Safari composition signal", { isComposing: false, keyCode: 229 }],
  ])("does not submit Enter with the %s", (_name, eventInit) => {
    const input = renderInput();
    fireEvent.change(input, { target: { value: "yanjiu" } });

    fireEvent.keyDown(input, { key: "Enter", ...eventInit });

    expect(onSubmit).not.toHaveBeenCalled();
    expect(input).toHaveValue("yanjiu");
  });

  it("blocks outside pointerdown until composition ends, then submits", () => {
    const input = renderInput();
    fireEvent.change(input, { target: { value: "yanjiu" } });
    fireEvent.compositionStart(input);

    expect(fireEvent.pointerDown(document.body)).toBe(false);
    expect(onSubmit).not.toHaveBeenCalled();
    expect(onCompositionChange).toHaveBeenLastCalledWith(true);

    fireEvent.change(input, { target: { value: "研究" } });
    fireEvent.compositionEnd(input);
    fireEvent.pointerDown(document.body);

    expect(onCompositionChange).toHaveBeenLastCalledWith(false);
    expect(onSubmit).toHaveBeenCalledTimes(1);
    expect(onSubmit).toHaveBeenCalledWith("研究");
  });

  it("keeps normal Enter and Escape behavior", () => {
    const input = renderInput();
    fireEvent.change(input, { target: { value: "Renamed chat" } });

    fireEvent.keyDown(input, { key: "Enter", isComposing: false, keyCode: 13 });
    fireEvent.keyDown(input, { key: "Escape" });

    expect(onSubmit).toHaveBeenCalledTimes(1);
    expect(onSubmit).toHaveBeenCalledWith("Renamed chat");
    expect(onCancel).toHaveBeenCalledTimes(1);
  });
});
