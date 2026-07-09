import { useState } from "react";

// A masked API-key input primitive (metadata-providers 02). A secret is entered
// masked by default (type="password") with a reveal toggle for shoulder-surf-safe
// pasting, and a "configured"/"not set" indicator sourced from `hasKey` — the
// server NEVER returns the stored key, so the field starts empty and only ever
// holds what the Admin is typing now. Typing a value SETS the key on save;
// leaving it empty leaves the key unchanged. The optional Clear action marks the
// stored key to be cleared on save.
//
// It is deliberately presentational: the parent owns the value + the draft "clear"
// state, so one Save persists every provider's edits together.

export default function MaskedKeyInput({
  slug,
  hasKey,
  value,
  cleared,
  onChange,
  onClear,
  disabled,
}: {
  slug: string;
  /** Whether a key is currently on file (from the server; never the value). */
  hasKey: boolean;
  /** The key the Admin is typing now ("" = not typing → unchanged on save). */
  value: string;
  /** Whether the stored key is marked to be cleared on the next save. */
  cleared: boolean;
  onChange: (value: string) => void;
  onClear: () => void;
  disabled?: boolean;
}) {
  const [reveal, setReveal] = useState(false);
  // The indicator reads the RESULTING state: a typed value will configure it, a
  // pending clear will unset it, otherwise it reflects what's on file.
  const willBeConfigured = value !== "" || (hasKey && !cleared);

  return (
    <div className="masked-key" data-testid={`provider-key-${slug}`}>
      <input
        className="field-input masked-key-input"
        data-testid={`provider-key-input-${slug}`}
        type={reveal ? "text" : "password"}
        value={value}
        placeholder={hasKey ? "•••••••• (configured)" : "Enter API key"}
        autoComplete="off"
        spellCheck={false}
        onChange={(e) => onChange(e.target.value)}
        disabled={disabled}
        aria-label={`API key for ${slug}`}
      />
      <button
        className="nav-link masked-key-reveal"
        type="button"
        data-testid={`provider-key-reveal-${slug}`}
        onClick={() => setReveal((r) => !r)}
        disabled={disabled}
        aria-pressed={reveal}
      >
        {reveal ? "Hide" : "Show"}
      </button>
      <span
        className="masked-key-status"
        data-testid={`provider-key-status-${slug}`}
        data-configured={willBeConfigured ? "true" : "false"}
      >
        {willBeConfigured ? "Configured" : "Not set"}
      </span>
      {hasKey && !cleared && (
        <button
          className="nav-link masked-key-clear"
          type="button"
          data-testid={`provider-key-clear-${slug}`}
          onClick={onClear}
          disabled={disabled}
        >
          Clear
        </button>
      )}
      {cleared && (
        <span
          className="masked-key-cleared"
          data-testid={`provider-key-cleared-${slug}`}
        >
          Will clear on save
        </span>
      )}
    </div>
  );
}
