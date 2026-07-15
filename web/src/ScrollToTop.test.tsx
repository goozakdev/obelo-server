import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Routes, Route, Link, useNavigate } from "react-router-dom";
import ScrollToTop from "./ScrollToTop";

// A screen with a forward link and a programmatic Back, so we can exercise both
// a PUSH navigation (link) and a POP navigation (history back).
function Home() {
  return <Link to="/detail">go to detail</Link>;
}

function Detail() {
  const navigate = useNavigate();
  return (
    <button type="button" onClick={() => navigate(-1)}>
      back
    </button>
  );
}

function app() {
  return (
    <MemoryRouter initialEntries={["/"]}>
      <ScrollToTop />
      <Routes>
        <Route path="/" element={<Home />} />
        <Route path="/detail" element={<Detail />} />
      </Routes>
    </MemoryRouter>
  );
}

describe("ScrollToTop", () => {
  let scrollTo: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    scrollTo = vi.spyOn(window, "scrollTo").mockImplementation(() => {});
  });

  afterEach(() => {
    scrollTo.mockRestore();
  });

  it("scrolls to the top when following a link (PUSH)", async () => {
    render(app());
    scrollTo.mockClear(); // ignore the initial mount

    await userEvent.click(screen.getByText("go to detail"));

    expect(scrollTo).toHaveBeenCalledWith(0, 0);
  });

  it("does not scroll on Back (POP) so the browser restores position", async () => {
    render(app());
    await userEvent.click(screen.getByText("go to detail"));
    scrollTo.mockClear();

    await userEvent.click(screen.getByText("back"));

    expect(scrollTo).not.toHaveBeenCalled();
  });
});
