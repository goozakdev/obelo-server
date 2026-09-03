import type { AdminUser } from "../api/types";
import { formatAgo } from "../time";
import { EditIcon, TrashIcon } from "../browse/ActionIcons";

// One User row in the Admin Users hub: identity (username + role) on the left and
// two icon actions on the right — Edit and Delete. Nothing else lives here.
//
// The row is purely presentational: it owns no API call and no dialog. Both
// actions hand the User up to AdminUsersScreen, which mounts the Edit dialog and
// the delete confirmation. That's deliberate — the delete confirmation is
// reachable from TWO places (this row's trash icon and the Edit dialog's "Delete
// user" button), so it belongs to the one component both paths report to rather
// than being duplicated in each.
//
// Everything this row used to carry inline — the grant checklist, the Rating
// ceiling, the password reset, the two-step delete — now lives in EditUserDialog,
// so a long roster reads as a scannable list instead of a stack of open forms.
//
// The one exception is the LINK BADGE a `remote` User carries. A linked Server
// is the only User whose row has to answer a question the operator cannot answer
// any other way: has the other household actually redeemed the invite I sent
// them? Its Device is created by the redemption and by nothing else (ADR-0055
// §4), so an absent last-seen is "never linked" and a present one is when that
// Server last spoke. It is on the ROW rather than in the dialog because it is
// the thing you scan the list for.

export default function UserAdminRow({
  user,
  onEdit,
  onDelete,
}: {
  user: AdminUser;
  /** Open the Edit dialog for this User. */
  onEdit: (user: AdminUser) => void;
  /** Ask to delete this User (the screen confirms first). */
  onDelete: (user: AdminUser) => void;
}) {
  const isRemote = user.role === "remote";
  const seen = formatAgo(user.lastSeenAt);
  return (
    <li
      className="admin-user-row admin-panel-row"
      data-testid="admin-user-row"
      data-user-id={user.id}
    >
      <div className="admin-user-identity">
        <span className="user-username" data-testid="admin-user-username">
          {user.username}
        </span>
        <span className="user-role" data-testid="admin-user-role">
          {user.role}
        </span>
        {isRemote && (
          <span
            className={`user-link-state ${seen ? "is-linked" : "is-unlinked"}`}
            data-testid="admin-user-link-state"
          >
            {seen ? `Linked, seen ${seen}` : "Never linked"}
          </span>
        )}
      </div>

      <div className="admin-row-actions">
        <button
          className="icon-button"
          type="button"
          data-testid="edit-user-button"
          aria-label={`Edit ${user.username}`}
          title="Edit"
          onClick={() => onEdit(user)}
        >
          <EditIcon />
        </button>
        <button
          className="icon-button icon-button-danger"
          type="button"
          data-testid="delete-user-button"
          aria-label={`Delete ${user.username}`}
          title="Delete"
          onClick={() => onDelete(user)}
        >
          <TrashIcon />
        </button>
      </div>
    </li>
  );
}
