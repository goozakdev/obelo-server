import { Component, type ErrorInfo, type ReactNode } from "react";

// Recovery from a failed lazy chunk load (a stale tab after a server upgrade:
// the hashed asset it asks for no longer exists and the server 404s it). There
// is deliberately no automatic reload: the error boundary turns the failure into
// an inline "Reload" message and the user decides, so it cannot loop and never
// interrupts playback (the player lives outside the boundary).

function isChunkLoadError(err: unknown): boolean {
  const msg = err instanceof Error ? `${err.name} ${err.message}` : String(err);
  return /dynamically imported module|importing a module script failed|Loading chunk|Loading CSS chunk|Unable to preload CSS|error loading dynamically/i.test(
    msg,
  );
}

interface Props {
  children: ReactNode;
  /** Changing this clears a caught error (pass the route path). */
  resetKey?: string;
  /** Injectable for tests. */
  reload?: () => void;
}

interface BoundaryState {
  error: unknown;
  resetKey?: string;
}

/** Catches render/lazy-load errors beneath it and shows a small inline reload
 * prompt, leaving everything outside the boundary mounted. */
export class ChunkErrorBoundary extends Component<Props, BoundaryState> {
  state: BoundaryState = { error: null, resetKey: this.props.resetKey };

  static getDerivedStateFromError(error: unknown): Partial<BoundaryState> {
    return { error: error ?? new Error("unknown error") };
  }

  static getDerivedStateFromProps(props: Props, state: BoundaryState): Partial<BoundaryState> | null {
    if (props.resetKey !== state.resetKey) return { error: null, resetKey: props.resetKey };
    return null;
  }

  componentDidCatch(error: unknown, info: ErrorInfo) {
    console.error("render failed", error, info.componentStack);
  }

  render() {
    if (this.state.error === null) return this.props.children;
    const chunk = isChunkLoadError(this.state.error);
    const reload = this.props.reload ?? (() => window.location.reload());
    return (
      <div className="status status-error" role="alert" data-testid="chunk-error">
        <span className="dot dot-error" aria-hidden="true" />
        {chunk ? "This page failed to load" : "Something went wrong"} &mdash;{" "}
        <button type="button" onClick={reload}>
          Reload
        </button>
      </div>
    );
  }
}
