import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import ConfirmDialog from "./ConfirmDialog";

// While the caller's action is in flight the dialog must stay up. The first ESC is
// cancelable and ConfirmDialog swallows it; a second one with no user activation in
// between is not, so the browser closes the native <dialog> and fires `close`
// regardless (R02-15). That close must not reach onCancel, and the dialog must be
// put back.

let showModal: ReturnType<typeof vi.fn>;

beforeEach(() => {
  showModal = vi.fn(function (this: HTMLDialogElement) {
    this.open = true;
  });
  HTMLDialogElement.prototype.showModal = showModal as unknown as () => void;
  HTMLDialogElement.prototype.close = vi.fn(function (this: HTMLDialogElement) {
    this.open = false;
  });
});

function nativeClose() {
  const dialog = screen.getByTestId("confirm-dialog") as HTMLDialogElement;
  dialog.open = false;
  dialog.dispatchEvent(new Event("close"));
}

describe("ConfirmDialog — a native close while busy", () => {
  it("re-opens instead of cancelling (R02-15)", () => {
    const onCancel = vi.fn();
    render(
      <ConfirmDialog
        title="Delete"
        message="Sure?"
        confirmLabel="Delete"
        busyLabel="Deleting…"
        busy
        onConfirm={vi.fn()}
        onCancel={onCancel}
      />,
    );
    expect(showModal).toHaveBeenCalledTimes(1);

    nativeClose();

    expect(onCancel).not.toHaveBeenCalled();
    expect(showModal).toHaveBeenCalledTimes(2);
  });

  it("still cancels on a native close when idle", () => {
    const onCancel = vi.fn();
    render(
      <ConfirmDialog
        title="Delete"
        message="Sure?"
        confirmLabel="Delete"
        busyLabel="Deleting…"
        onConfirm={vi.fn()}
        onCancel={onCancel}
      />,
    );

    nativeClose();

    expect(onCancel).toHaveBeenCalledTimes(1);
  });
});
