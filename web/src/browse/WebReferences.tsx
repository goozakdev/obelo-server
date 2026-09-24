import { apiClient } from "../api/client";
import { useAsync } from "./useAsync";

// The Title detail's Web references: where a person can read about this Title
// elsewhere ("IMDb", "Trakt"), as answered by the Web reference provider plugins.
// The server has already kept only https addresses keyed to ids it holds for the
// Title, so this renders what it is given and decides nothing. Every role sees
// them. Loading, an empty list and a failed request all render NOTHING — a
// missing link is never worth an error on a detail page — so the caller can
// render this unconditionally.

export interface WebReferencesProps {
  titleId: string;
}

export default function WebReferences({ titleId }: WebReferencesProps) {
  const state = useAsync((signal) => apiClient.getWebReferences(titleId, signal), [titleId]);
  if (state.status !== "ready" || state.data.length === 0) {
    return null;
  }
  return (
    <div className="detail-web-references" data-testid="web-references">
      <h2 className="section-title">Elsewhere</h2>
      <ul className="web-reference-list">
        {state.data.map((ref) => (
          <li key={ref.url}>
            <a
              href={ref.url}
              target="_blank"
              rel="noopener noreferrer"
              data-testid="web-reference"
            >
              {ref.label}
            </a>
          </li>
        ))}
      </ul>
    </div>
  );
}
